package sshrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"ghosthand/proto"
)

// TerminalCommand delivers terminal input or resize commands to an active SSH PTY session.
type TerminalCommand struct {
	Kind  proto.MessageType
	Frame proto.TerminalFrame
}

// Manager maintains authenticated SSH client connections and executes interactive PTY shells
// as well as Linux MCP tools directly on the connected SSH hosts.
// It is shared identically by both GhostHand Agents (delegated SSH mode) and the GhostHand Server (direct SSH mode).
type Manager struct {
	ctx     context.Context
	cancel  context.CancelFunc
	onState func(proto.SSHStatePayload)

	mu      sync.Mutex
	targets map[string]*sshTarget
	wg      sync.WaitGroup
}

type sshTarget struct {
	cfg       proto.SSHConnectPayload
	cancel    context.CancelFunc
	mu        sync.RWMutex
	client    *ssh.Client
	lastState proto.SSHStatePayload
}

func NewManager(parent context.Context, onState func(proto.SSHStatePayload)) *Manager {
	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		ctx:     ctx,
		cancel:  cancel,
		onState: onState,
		targets: make(map[string]*sshTarget),
	}
}

func (m *Manager) emit(state proto.SSHStatePayload) {
	if m.onState != nil {
		m.onState(state)
	}
}

// Connect establishes (or updates) a persistent supervised SSH login to the target Linux host.
func (m *Manager) Connect(cfg proto.SSHConnectPayload) {
	if cfg.SSHDeviceID == "" || strings.TrimSpace(cfg.Host) == "" || strings.TrimSpace(cfg.Username) == "" {
		return
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = 22
	}
	if cfg.AuthKind == "" {
		cfg.AuthKind = proto.SSHAuthPassword
	}

	m.mu.Lock()
	if existing := m.targets[cfg.SSHDeviceID]; existing != nil {
		if existing.cfg == cfg {
			existing.mu.RLock()
			state := existing.lastState
			existing.mu.RUnlock()
			m.mu.Unlock()
			if state.SSHDeviceID != "" {
				m.emit(state)
			}
			return
		}
		existing.cancel()
		existing.closeClient()
		delete(m.targets, cfg.SSHDeviceID)
	}
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	targetCtx, cancel := context.WithCancel(m.ctx)
	target := &sshTarget{
		cfg:    cfg,
		cancel: cancel,
		lastState: proto.SSHStatePayload{
			SSHDeviceID: cfg.SSHDeviceID,
			AttemptID:   cfg.AttemptID,
			State:       proto.Offline,
		},
	}
	m.targets[cfg.SSHDeviceID] = target
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		m.supervise(targetCtx, target)
	}()
}

// Disconnect terminates and removes a managed SSH target.
func (m *Manager) Disconnect(sshDeviceID string) {
	m.mu.Lock()
	target := m.targets[sshDeviceID]
	delete(m.targets, sshDeviceID)
	m.mu.Unlock()
	if target != nil {
		target.cancel()
		target.closeClient()
	}
}

// DisconnectManual closes explicit logins when the connection to their controller ends.
func (m *Manager) DisconnectManual() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, target := range m.targets {
		if target.cfg.AttemptID != "" {
			delete(m.targets, id)
			target.cancel()
			target.closeClient()
		}
	}
}

// EmitStates re-emits the latest known state for every managed SSH target.
func (m *Manager) EmitStates() {
	m.mu.Lock()
	states := make([]proto.SSHStatePayload, 0, len(m.targets))
	for _, target := range m.targets {
		target.mu.RLock()
		if target.lastState.SSHDeviceID != "" {
			states = append(states, target.lastState)
		}
		target.mu.RUnlock()
	}
	m.mu.Unlock()
	for _, st := range states {
		m.emit(st)
	}
}

// Close shuts down all supervised SSH connections.
func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	for id, target := range m.targets {
		target.closeClient()
		target.cancel()
		delete(m.targets, id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (t *sshTarget) setClient(c *ssh.Client) {
	t.mu.Lock()
	old := t.client
	t.client = c
	t.mu.Unlock()
	if old != nil && old != c {
		_ = old.Close()
	}
}

func (t *sshTarget) getClient() *ssh.Client {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.client
}

func (t *sshTarget) closeClient() {
	t.mu.Lock()
	c := t.client
	t.client = nil
	t.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (t *sshTarget) setState(st proto.SSHStatePayload) {
	t.mu.Lock()
	t.lastState = st
	t.mu.Unlock()
}

func (m *Manager) getClient(sshDeviceID string) (*ssh.Client, string, error) {
	m.mu.Lock()
	target := m.targets[sshDeviceID]
	m.mu.Unlock()
	if target == nil {
		return nil, "", errors.New("SSH 目标尚未配置连接")
	}
	c := target.getClient()
	if c == nil {
		return nil, "", errors.New("SSH 设备当前离线或正在重连")
	}
	return c, target.cfg.Username, nil
}

func (m *Manager) supervise(ctx context.Context, target *sshTarget) {
	defer target.closeClient()
	delay := 3 * time.Second
	for ctx.Err() == nil {
		client, hostFP, err := dialSSH(ctx, target.cfg)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			st := proto.SSHStatePayload{
				SSHDeviceID: target.cfg.SSHDeviceID,
				AttemptID:   target.cfg.AttemptID,
				State:       proto.Offline,
				Error:       limitError(err.Error()),
			}
			target.setState(st)
			m.emit(st)
			if target.cfg.AttemptID != "" {
				return
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			delay = min(delay*2, 30*time.Second)
			continue
		}

		delay = 3 * time.Second
		target.setClient(client)
		if ctx.Err() != nil {
			return
		}
		osVersion, arch := probeRemoteSystem(ctx, client)
		if ctx.Err() != nil {
			return
		}
		st := proto.SSHStatePayload{
			SSHDeviceID: target.cfg.SSHDeviceID,
			AttemptID:   target.cfg.AttemptID,
			State:       proto.Online,
			OSVersion:   osVersion,
			Arch:        arch,
			HostKeyFP:   hostFP,
		}
		target.setState(st)
		m.emit(st)

		disconnectErr := waitKeepAlive(ctx, client)
		target.closeClient()
		if ctx.Err() != nil {
			return
		}
		errMsg := "SSH 连接已断开"
		if disconnectErr != nil {
			errMsg = limitError("SSH 连接断开: " + disconnectErr.Error())
		}
		off := proto.SSHStatePayload{
			SSHDeviceID: target.cfg.SSHDeviceID,
			AttemptID:   target.cfg.AttemptID,
			State:       proto.Offline,
			Error:       errMsg,
		}
		target.setState(off)
		m.emit(off)
		if target.cfg.AttemptID != "" {
			return
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func dialSSH(ctx context.Context, cfg proto.SSHConnectPayload) (*ssh.Client, string, error) {
	var authMethods []ssh.AuthMethod
	switch cfg.AuthKind {
	case proto.SSHAuthPrivateKey:
		signer, err := ssh.ParsePrivateKey([]byte(cfg.Secret))
		if err != nil {
			return nil, "", fmt.Errorf("解析 SSH 私钥失败: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	default:
		password := cfg.Secret
		authMethods = append(authMethods,
			ssh.Password(password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = password
				}
				return answers, nil
			}),
		)
	}

	var capturedFP string
	expectedFP := strings.TrimSpace(cfg.HostKeyFP)
	hostKeyCallback := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if expectedFP != "" && fp != expectedFP {
			return fmt.Errorf("SSH 主机指纹不匹配（期望 %s，实际 %s）", expectedFP, fp)
		}
		capturedFP = fp
		return nil
	}

	clientConfig := &ssh.ClientConfig{
		User:            cfg.Username,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}

	addr := net.JoinHostPort(strings.TrimSpace(cfg.Host), strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("连接 %s 失败: %w", addr, err)
	}

	// Ensure handshake respects ctx cancellation and timeout.
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = rawConn.Close()
		case <-handshakeDone:
		}
	}()
	_ = rawConn.SetDeadline(time.Now().Add(15 * time.Second))
	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, addr, clientConfig)
	close(handshakeDone)
	if err != nil {
		_ = rawConn.Close()
		return nil, "", fmt.Errorf("SSH 认证失败: %w", err)
	}
	_ = rawConn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, chans, reqs), capturedFP, nil
}

func waitKeepAlive(ctx context.Context, client *ssh.Client) error {
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- client.Wait()
	}()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-waitDone:
			return err
		case <-ticker.C:
			kaDone := make(chan error, 1)
			go func() {
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				kaDone <- err
			}()
			kaTimer := time.NewTimer(10 * time.Second)
			select {
			case <-ctx.Done():
				kaTimer.Stop()
				return ctx.Err()
			case err := <-waitDone:
				kaTimer.Stop()
				return err
			case err := <-kaDone:
				kaTimer.Stop()
				if err != nil {
					return err
				}
			case <-kaTimer.C:
				return errors.New("SSH keepalive timeout")
			}
		}
	}
}

func probeRemoteSystem(ctx context.Context, client *ssh.Client) (osVersion string, arch string) {
	osVersion, arch = "Linux (SSH)", "amd64"
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, _, err := runSimpleCommand(probeCtx, client, `uname -m; printf '\n---OS---\n'; if [ -r /etc/os-release ]; then sed -n 's/^PRETTY_NAME=//p' /etc/os-release | head -n1 | tr -d '"'; else uname -sr; fi`)
	if err != nil {
		return osVersion, arch
	}
	parts := strings.SplitN(out, "\n---OS---\n", 2)
	if len(parts) >= 1 {
		rawArch := strings.TrimSpace(parts[0])
		switch rawArch {
		case "x86_64", "amd64":
			arch = "amd64"
		case "aarch64", "arm64":
			arch = "arm64"
		default:
			if rawArch != "" {
				arch = rawArch
			}
		}
	}
	if len(parts) == 2 {
		if pretty := strings.TrimSpace(parts[1]); pretty != "" {
			if len(pretty) > 120 {
				pretty = pretty[:120]
			}
			osVersion = pretty
		}
	}
	return osVersion, arch
}

func runSimpleCommand(ctx context.Context, client *ssh.Client, command string) (string, string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", "", err
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()
	select {
	case err = <-done:
		return stdout.String(), stderr.String(), err
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return stdout.String(), stderr.String(), ctx.Err()
	}
}

// OpenTerminal opens an interactive PTY shell session on the already-authenticated SSH client
// and streams ANSI/VT output to the output callback until the session exits or ctx is canceled.
func (m *Manager) OpenTerminal(ctx context.Context, req proto.TerminalFrame, commands <-chan TerminalCommand, output func(string)) proto.TerminalFrame {
	client, _, err := m.getClient(req.TargetDeviceID)
	if err != nil {
		return proto.TerminalFrame{Error: err.Error()}
	}
	session, err := client.NewSession()
	if err != nil {
		return proto.TerminalFrame{Error: fmt.Sprintf("创建 SSH 会话失败: %v", err)}
	}
	defer session.Close()

	cols, rows := proto.TerminalDimensions(req.Cols, req.Rows)
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		return proto.TerminalFrame{Error: fmt.Sprintf("请求 PTY 终端失败: %v", err)}
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return proto.TerminalFrame{Error: fmt.Sprintf("获取 SSH 输入管道失败: %v", err)}
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return proto.TerminalFrame{Error: fmt.Sprintf("获取 SSH 输出管道失败: %v", err)}
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return proto.TerminalFrame{Error: fmt.Sprintf("获取 SSH 错误管道失败: %v", err)}
	}
	if err := session.Shell(); err != nil {
		return proto.TerminalFrame{Error: fmt.Sprintf("启动远端 Shell 失败: %v", err)}
	}

	var emitMu sync.Mutex
	safeEmit := func(text string) {
		emitMu.Lock()
		output(text)
		emitMu.Unlock()
	}

	var outWG sync.WaitGroup
	outWG.Add(2)
	go func() {
		defer outWG.Done()
		streamUTF8(stdout, safeEmit)
	}()
	go func() {
		defer outWG.Done()
		streamUTF8(stderr, safeEmit)
	}()

	controlCtx, stopControls := context.WithCancel(ctx)
	controlDone := make(chan struct{})
	controlErr := make(chan error, 1)
	go func() {
		defer close(controlDone)
		for {
			select {
			case <-controlCtx.Done():
				return
			case cmd, ok := <-commands:
				if !ok {
					return
				}
				switch cmd.Kind {
				case proto.TerminalInputMessage:
					if _, err := io.WriteString(stdin, cmd.Frame.Data); err != nil {
						controlErr <- fmt.Errorf("写入 SSH 终端输入失败: %w", err)
						return
					}
				case proto.TerminalResizeMessage:
					c, r := proto.TerminalDimensions(cmd.Frame.Cols, cmd.Frame.Rows)
					_ = session.WindowChange(r, c)
				}
			}
		}
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- session.Wait()
	}()

	var result proto.TerminalFrame
	select {
	case err := <-waitDone:
		if err != nil {
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				code := exitErr.ExitStatus()
				result.ExitCode = &code
			} else if !errors.Is(err, io.EOF) {
				result.Error = fmt.Sprintf("SSH Shell 异常退出: %v", err)
			} else {
				code := 0
				result.ExitCode = &code
			}
		} else {
			code := 0
			result.ExitCode = &code
		}
	case err := <-controlErr:
		result.Error = err.Error()
		_ = session.Signal(ssh.SIGHUP)
		_ = stdin.Close()
		_ = session.Close()
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGHUP)
		_ = stdin.Close()
		_ = session.Close()
	}

	stopControls()
	_ = stdin.Close()
	_ = session.Close()
	<-controlDone
	outWG.Wait()
	return result
}

func streamUTF8(r io.Reader, emit func(string)) {
	var buffer [proto.TerminalChunkSize]byte
	var pending int
	for {
		n, err := r.Read(buffer[pending:])
		total := pending + n
		end := total
		if err == nil {
			for start := total - 1; start >= max(0, total-utf8.UTFMax+1); start-- {
				if utf8.RuneStart(buffer[start]) {
					if !utf8.FullRune(buffer[start:total]) {
						end = start
					}
					break
				}
			}
		}
		if end != 0 {
			emit(strings.ToValidUTF8(string(buffer[:end]), "?"))
		}
		if err != nil {
			return
		}
		pending = copy(buffer[:], buffer[end:total])
	}
}

func limitError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400]
	}
	return s
}
