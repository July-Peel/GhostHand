package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"ghosthand/launcher/state"
	"ghosthand/proto"
	"ghosthand/proto/sshrunner"
)

type sshOutbound struct {
	kind    proto.MessageType
	payload any
}

type agent struct {
	cfg          atomic.Pointer[configuration]
	client       *http.Client
	local        *journal
	logs         *memLogBuffer
	platform     *host
	browser      *browserBridge
	ssh          *sshrunner.Manager
	sshEvents    chan sshOutbound
	corePath     string
	helperPath   string
	operations   chan struct{}
	taskChanges  chan struct{}
	background   chan struct{}
	canUninstall bool
	welcomed     chan struct{}
	firstWelcome sync.Once
	rdpMu        sync.Mutex
}

func (a *agent) enqueueSSHOutbound(kind proto.MessageType, payload any) {
	if a.sshEvents == nil {
		return
	}
	select {
	case a.sshEvents <- sshOutbound{kind: kind, payload: payload}:
	default:
		slog.Warn("dropping ssh outbound message due to full queue", "kind", kind)
	}
}

var (
	errReconnect = errors.New("network or power changed")
	errSleeping  = errors.New("system is suspending")
	errHeartbeat = errors.New("controller did not answer heartbeat")
)

func (a *agent) connect(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		if a.platform.sleeping.Load() {
			select {
			case <-ctx.Done():
				return
			case <-a.platform.wake:
				continue
			}
		}
		started := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errReconnect) || errors.Is(err, errSleeping) {
			delay = time.Second
			continue
		}
		if time.Since(started) > 20*time.Second {
			delay = time.Second
		}
		if websocket.CloseStatus(err) == websocket.StatusPolicyViolation || errors.Is(err, errSequence) {
			a.local.resetForFreshSession()
			if enrollErr := a.enroll(ctx); enrollErr != nil {
				slog.Warn("re-enrollment failed", "error", enrollErr)
			}
		}
		slog.Warn("controller disconnected", "error", err, "retry_seconds", delay.Seconds())
		wait := time.Duration(float64(delay) * (0.8 + 0.4*rand.Float64()))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-a.platform.wake:
			delay = time.Second
		case <-timer.C:
			delay = min(delay*2, 20*time.Second)
		}
		timer.Stop()
	}
}

func (a *agent) session(ctx context.Context) error {
	cfg := *a.cfg.Load()
	url := strings.Replace(cfg.Server, "http", "ws", 1) + proto.AgentSocketPath
	helloCtx, stopHello := context.WithTimeout(ctx, 8*time.Second)
	defer stopHello()
	conn, response, err := websocket.Dial(helloCtx, url, &websocket.DialOptions{HTTPClient: a.client})
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(proto.MaxToolArgsSize + (64 << 10))
	fresh := a.local.isFresh()
	lastAck, running, pendingResults, err := a.local.taskInventory()
	if err != nil {
		return err
	}
	if fresh {
		lastAck = 0
		running = nil
		pendingResults = nil
	}
	snapshot := a.platform.snapshot()
	platform := proto.Platform(runtime.GOOS)
	hello := proto.HelloPayload{
		DeviceID: cfg.DeviceID, Token: cfg.Token, Platform: platform, AgentVersion: version,
		DesktopOperations:  platform == proto.Windows,
		TerminalOperations: platform == proto.Windows || platform == proto.Linux,
		LastAck:            lastAck, Fresh: fresh, CapsHash: proto.TaskCapabilitiesHash(platform), RunningTasks: running, State: snapshot,
		TaskInventory: true, PendingTaskResults: pendingResults,
	}
	if a.canUninstall {
		if platform == proto.Linux {
			hello.CapsHash = proto.LinuxUninstallCapsHash
		} else {
			hello.CapsHash = proto.WindowsUninstallCapsHash
		}
	}
	if err := write(helloCtx, conn, proto.Hello, hello, 0); err != nil {
		return err
	}
	a.platform.reportedPower(snapshot)
	var frame proto.Envelope
	if err := wsjson.Read(helloCtx, conn, &frame); err != nil {
		return err
	}
	var welcome proto.WelcomePayload
	if frame.Version != proto.Version || frame.Type != proto.Welcome || frame.Seq != 0 ||
		json.Unmarshal(frame.Payload, &welcome) != nil || welcome.HeartbeatSec < 15 || welcome.HeartbeatSec > 270 {
		return errors.New("invalid controller welcome")
	}
	if err := a.local.acceptWelcome(welcome.LastAck); err != nil {
		return err
	}
	policy := proto.DesktopPolicy{}
	if welcome.DesktopPolicy != nil {
		policy = *welcome.DesktopPolicy
	}
	a.platform.applyDesktopPolicy(policy)
	if welcome.TokenRotate != "" {
		if _, err := proto.TokenHash(welcome.TokenRotate); err != nil {
			return err
		}
		cfg.Token = welcome.TokenRotate
		a.cfg.Store(&cfg)
	}
	a.firstWelcome.Do(func() { close(a.welcomed) })
	slog.Info("controller connected", "generation", welcome.ConnGen, "version", version)
	if welcome.NeedCaps {
		report := proto.CapabilitiesReport{Hash: hello.CapsHash, CapsReportPayload: proto.CapsReportPayload{
			Capabilities: proto.TaskCapabilities(platform), FlowsCached: []string{},
		}}
		if platform == proto.Windows {
			report.Tools = proto.DeviceTools
			report.Resources = proto.DeviceResources
		} else if platform == proto.Linux {
			report.Tools = proto.LinuxTools
			report.Resources = proto.LinuxResources
		}
		if err := a.local.enqueueCaps(report); err != nil {
			return err
		}
	}
	return a.exchange(ctx, conn, time.Duration(welcome.HeartbeatSec)*time.Second, lastAck, welcome.LastAck, welcome.TaskHeartbeat)
}

func write(ctx context.Context, conn *websocket.Conn, kind proto.MessageType, payload any, ack uint64) error {
	frame, err := proto.NewEnvelope(kind, payload)
	if err != nil {
		return err
	}
	frame.Ack = ack
	return writeEnvelope(ctx, conn, frame)
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, frame proto.Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wsjson.Write(ctx, conn, frame)
}

type readResult struct {
	frame proto.Envelope
	err   error
}

func (a *agent) exchange(ctx context.Context, conn *websocket.Conn, interval time.Duration, inbound, sent uint64, taskHeartbeat bool) error {
	ctx, cancel := context.WithCancel(ctx)
	terminals := newTerminalManager(ctx, a.ssh)
	terminalWriterDone := make(chan struct{})
	go func() {
		defer close(terminalWriterDone)
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-terminals.events:
				if write(ctx, conn, event.kind, event.frame, 0) != nil {
					_ = conn.CloseNow()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		terminals.closeAll()
		<-terminalWriterDone
	}()
	if a.ssh != nil {
		go a.ssh.EmitStates()
	}
	received := make(chan readResult, 1)
	go func() {
		for {
			var frame proto.Envelope
			err := wsjson.Read(ctx, conn, &frame)
			if err == nil && frame.Seq == 0 && isTerminalCommand(frame.Type) {
				err = terminals.receive(frame)
				if err == nil {
					continue
				}
			}
			select {
			case received <- readResult{frame, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var taskTicker *time.Ticker
	var taskTick <-chan time.Time
	defer func() {
		if taskTicker != nil {
			taskTicker.Stop()
		}
	}()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	var timeout <-chan time.Time = deadline.C
	desktopResults := make(chan proto.DesktopResult, 8)
	desktopSlots := make(chan struct{}, 8)
	sshCallSlots := make(chan struct{}, 16)
	if err := a.sendState(ctx, conn, proto.Ping, inbound); err != nil {
		return err
	}
	for {
		if a.platform.sleeping.Load() {
			if err := a.sendState(ctx, conn, proto.StateSleeping, inbound); err != nil {
				return err
			}
			wait := time.NewTimer(750 * time.Millisecond)
			defer wait.Stop()
			for timeout := wait.C; ; {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timeout:
					return errSleeping
				case input := <-received:
					if input.err != nil || input.frame.Type == proto.Pong {
						return errSleeping
					}
				}
			}
		}
		frames, err := a.local.framesAfter(sent)
		if err != nil {
			return err
		}
		for _, frame := range frames {
			frame.Ack = inbound
			if err := writeEnvelope(ctx, conn, frame); err != nil {
				return err
			}
			sent = frame.Seq
		}
		tasks, err := a.local.tasks()
		if err != nil {
			return err
		}
		if taskHeartbeat && len(tasks) > 0 && taskTicker == nil {
			taskTicker = time.NewTicker(10 * time.Second)
			taskTick = taskTicker.C
		} else if len(tasks) == 0 && taskTicker != nil {
			taskTicker.Stop()
			taskTicker, taskTick = nil, nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.platform.wake:
			return errReconnect
		case <-a.platform.changed:
			if !a.platform.sleeping.Load() {
				if err := a.sendState(ctx, conn, proto.StateUpdate, inbound); err != nil {
					return err
				}
			}
		case <-a.local.changed:
		case out := <-a.sshEvents:
			if err := write(ctx, conn, out.kind, out.payload, inbound); err != nil {
				return err
			}
		case result := <-desktopResults:
			kind := proto.RDPResult
			if result.Kind == proto.BrowserExtensionStatusRequest {
				kind = proto.BrowserExtensionStatusResult
			}
			if err := a.sendState(ctx, conn, proto.StateUpdate, inbound); err != nil {
				return err
			}
			if err := write(ctx, conn, kind, result, inbound); err != nil {
				return err
			}
		case <-taskTick:
			for _, task := range tasks {
				if task.State == proto.Queued {
					continue
				}
				if err := write(ctx, conn, proto.TaskHeartbeat, proto.TaskStatus{
					TaskID: task.TaskID, State: task.State, StepIndex: task.Step, HeartbeatOnly: true,
				}, inbound); err != nil {
					return err
				}
			}
		case <-timeout:
			return errHeartbeat
		case <-ticker.C:
			if err := a.sendState(ctx, conn, proto.Ping, inbound); err != nil {
				return err
			}
			deadline.Reset(10 * time.Second)
			timeout = deadline.C
		case result := <-received:
			if result.err != nil {
				return result.err
			}
			frame := result.frame
			isDesktop := frame.Seq == 0 && (frame.Type == proto.BrowserExtensionStatusRequest ||
				frame.Type == proto.RDPPreflight || frame.Type == proto.RDPUnlock || frame.Type == proto.RDPLock)
			isPolicy := frame.Seq == 0 && frame.Type == proto.ConfigUpdate
			isSSH := frame.Seq == 0 && (frame.Type == proto.SSHConnectMessage ||
				frame.Type == proto.SSHDisconnectMessage || frame.Type == proto.SSHCallMessage)
			if frame.Version != proto.Version || !state.IDPattern.MatchString(frame.ID) ||
				(frame.Seq == 0 && frame.Type != proto.Pong && !isDesktop && !isPolicy && !isSSH) ||
				(frame.Seq > 0 && !validTaskFrame(frame) && !validOperation(frame)) {
				return errors.New("invalid controller message")
			}
			inbound, err = a.local.receive(frame)
			if err != nil {
				return err
			}
			if isSSH {
				if err := a.handleSSHFrame(ctx, frame, sshCallSlots); err != nil {
					return err
				}
			} else if isPolicy {
				var policy proto.DesktopPolicy
				if json.Unmarshal(frame.Payload, &policy) != nil {
					return errors.New("invalid desktop policy")
				}
				a.platform.applyDesktopPolicy(policy)
			} else if isDesktop {
				var request proto.DesktopRequest
				if json.Unmarshal(frame.Payload, &request) != nil || !state.IDPattern.MatchString(request.OperationID) {
					return errors.New("invalid desktop operation")
				}
				select {
				case desktopSlots <- struct{}{}:
					go func() {
						defer func() { <-desktopSlots }()
						progress := func(stage string) {
							select {
							case desktopResults <- proto.DesktopResult{OperationID: request.OperationID, Kind: frame.Type, Stage: stage}:
							default:
							}
						}
						outcome := a.desktopOperation(ctx, frame.Type, request, progress)
						select {
						case desktopResults <- outcome:
						case <-ctx.Done():
						}
					}()
				default:
					select {
					case desktopResults <- proto.DesktopResult{OperationID: request.OperationID, Kind: frame.Type, Code: proto.ErrBusy}:
					default:
					}
				}
			} else if frame.Type == proto.Pong {
				deadline.Stop()
				timeout = nil
			} else {
				if isTaskFrame(frame.Type) {
					signal(a.taskChanges)
				} else {
					signal(a.operations)
				}
				if err := a.sendState(ctx, conn, proto.Ping, inbound); err != nil {
					return err
				}
			}
		}
	}
}

func (a *agent) handleSSHFrame(ctx context.Context, frame proto.Envelope, slots chan struct{}) error {
	if a.ssh == nil {
		return nil
	}
	switch frame.Type {
	case proto.SSHConnectMessage:
		var payload proto.SSHConnectPayload
		if json.Unmarshal(frame.Payload, &payload) != nil || !state.IDPattern.MatchString(payload.SSHDeviceID) {
			return errors.New("invalid ssh.connect payload")
		}
		a.ssh.Connect(payload)
	case proto.SSHDisconnectMessage:
		var payload proto.SSHDisconnectPayload
		if json.Unmarshal(frame.Payload, &payload) != nil || !state.IDPattern.MatchString(payload.SSHDeviceID) {
			return errors.New("invalid ssh.disconnect payload")
		}
		a.ssh.Disconnect(payload.SSHDeviceID)
	case proto.SSHCallMessage:
		var payload proto.SSHMCPCallPayload
		if json.Unmarshal(frame.Payload, &payload) != nil || !state.IDPattern.MatchString(payload.CallID) ||
			!state.IDPattern.MatchString(payload.TargetDeviceID) {
			return errors.New("invalid ssh.mcp_call payload")
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				res := a.ssh.ExecuteTool(ctx, payload.TargetDeviceID, proto.MCPCall{
					CallID:    payload.CallID,
					TaskID:    payload.TaskID,
					Tool:      payload.Tool,
					Arguments: payload.Arguments,
					Approved:  payload.Approved,
				})
				a.enqueueSSHOutbound(proto.SSHResultMessage, proto.SSHMCPResultPayload{
					CallID:         payload.CallID,
					TargetDeviceID: payload.TargetDeviceID,
					TaskID:         payload.TaskID,
					Data:           res.Data,
					Error:          res.Error,
				})
			}()
		default:
			a.enqueueSSHOutbound(proto.SSHResultMessage, proto.SSHMCPResultPayload{
				CallID:         payload.CallID,
				TargetDeviceID: payload.TargetDeviceID,
				TaskID:         payload.TaskID,
				Error:          string(proto.ErrBusy),
			})
		}
	}
	return nil
}

func (a *agent) sendState(ctx context.Context, conn *websocket.Conn, kind proto.MessageType, ack uint64) error {
	snapshot := a.platform.snapshot()
	if err := write(ctx, conn, kind, snapshot, ack); err != nil {
		return err
	}
	a.platform.reportedPower(snapshot)
	return nil
}

func (a *agent) occupyBackground(ctx context.Context) (func(), error) {
	select {
	case a.background <- struct{}{}:
		return func() { <-a.background }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func validOperation(frame proto.Envelope) bool {
	var op proto.Operation
	if json.Unmarshal(frame.Payload, &op) != nil || !state.IDPattern.MatchString(op.ID) {
		return false
	}
	switch frame.Type {
	case proto.AgentUpdate:
		return op.Release != nil && state.IDPattern.MatchString(op.Release.ID)
	case proto.LogCollect:
		return op.Hours >= 1 && op.Hours <= 168
	case proto.AgentRestart, proto.HelperRestart, proto.AgentRollback:
		return true
	case proto.AgentUninstall:
		return op.Hours == 0 && op.Release == nil
	default:
		return false
	}
}
