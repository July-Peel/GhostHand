package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

type peer struct {
	secureTransport      bool
	desktopOperations    bool
	desktopRequests      chan desktopRequest
	desktopDone          chan struct{}
	desktopPending       sync.Map
	desktopPolicyChanged chan struct{}
	terminalFrames       chan proto.Envelope
	terminalDone         chan struct{}
	terminalMu           sync.Mutex
	terminalSessions     map[string]*TerminalSession
	sshFrames            chan proto.Envelope
	sshDone              chan struct{}
	conn                 *websocket.Conn
	id                   string
	gen                  uint64
	interval             time.Duration
	asleep               bool
}

var errStale = errors.New("stale device connection")

func (s *Service) Connect(w http.ResponseWriter, r *http.Request) {
	// Device clients do not use browser origins or cookies.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "device endpoint", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(proto.MaxToolDataSize + (64 << 10))
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	handshake, end := context.WithTimeout(ctx, 5*time.Second)
	var first proto.Envelope
	err = wsjson.Read(handshake, conn, &first)
	end()
	var hello proto.HelloPayload
	if err != nil || first.Version != proto.Version || first.Type != proto.Hello || first.Seq != 0 ||
		first.Ack != 0 || json.Unmarshal(first.Payload, &hello) != nil || len(hello.AgentVersion) > 64 ||
		len(hello.CapsHash) > 128 || !validState(hello.State) {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	secure := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
	p, welcome, err := s.attach(ctx, conn, hello, ip, secure)
	if err != nil {
		if errors.Is(err, ErrDenied) {
			_ = conn.Close(websocket.StatusPolicyViolation, "E_AUTH")
		} else if ctx.Err() == nil {
			slog.Error("device handshake", "error", err)
		}
		return
	}
	reason := "network_error"
	defer func() { s.detach(p, reason) }()
	defer close(p.desktopDone)
	defer close(p.sshDone)
	if err := sendFrame(ctx, conn, proto.Welcome, welcome, 0); err != nil {
		return
	}
	go s.syncHostAgentSSH(ctx, p)
	reason = s.servePeer(ctx, p, hello.LastAck)
}

func validState(state proto.DeviceState) bool {
	switch state.HelperState {
	case "", "none", "unavailable", "ready", "restarting":
	default:
		return false
	}
	switch state.DesktopState {
	case "", "no_session", "unlocked", "locked", "headless", "screen_off":
	default:
		return false
	}
	return state.BatteryPct == nil || (*state.BatteryPct >= 0 && *state.BatteryPct <= 100)
}

func (s *Service) attach(ctx context.Context, conn *websocket.Conn, hello proto.HelloPayload, ip string, secure bool) (*peer, proto.WelcomePayload, error) {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	now := time.Now()
	interval := 50
	if hello.Platform == proto.Android {
		interval = 60
	}
	if hello.State.HeartbeatSec != nil && *hello.State.HeartbeatSec >= 15 && *hello.State.HeartbeatSec <= 270 {
		interval = *hello.State.HeartbeatSec
	}
	if s.cloudflare && interval > 90 {
		interval = 90
	}
	p := &peer{conn: conn, id: hello.DeviceID, interval: time.Duration(interval) * time.Second, secureTransport: secure,
		desktopOperations: hello.DesktopOperations && hello.Platform == proto.Windows,
		desktopRequests:   make(chan desktopRequest), desktopDone: make(chan struct{}), desktopPolicyChanged: make(chan struct{}, 1),
		sshFrames: make(chan proto.Envelope, 32), sshDone: make(chan struct{})}
	if hello.TerminalOperations && (hello.Platform == proto.Windows || hello.Platform == proto.Linux) {
		p.terminalFrames = make(chan proto.Envelope, 32)
		p.terminalDone = make(chan struct{})
		p.terminalSessions = make(map[string]*TerminalSession)
	}
	welcome := proto.WelcomePayload{HeartbeatSec: interval, ServerTime: now.UnixMilli(), TaskHeartbeat: hello.TaskInventory}
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		c, hash, err := authenticate(ctx, tx, p.id, hello.Token)
		if err != nil {
			return err
		}
		var platform proto.Platform
		var capsHash string
		var serverSeq, serverAck uint64
		err = tx.QueryRowContext(ctx, `SELECT platform,caps_hash,conn_gen,agent_ack,controller_seq,controller_ack
			FROM devices WHERE id=? AND id<>?`, p.id, workbench.FreeChatDeviceID).Scan(&platform, &capsHash, &p.gen, &welcome.LastAck, &serverSeq, &serverAck)
		if err != nil {
			return err
		}
		if platform != hello.Platform {
			return ErrDenied
		}
		if p.desktopOperations {
			policy, err := readDesktopPolicy(ctx, tx, p.id)
			if err != nil {
				return err
			}
			welcome.DesktopPolicy = &policy
		}
		if hello.Fresh {
			if hello.LastAck != 0 || len(hello.RunningTasks) != 0 {
				return ErrDenied
			}
			if err := workbench.ResetAgentTasks(ctx, tx, p.id, now.UnixMilli()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM outbox WHERE device_id=?", p.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM device_inbox WHERE device_id=?", p.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE devices SET controller_seq=0,controller_ack=0,agent_ack=0 WHERE id=?", p.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE device_operations
				SET state=CASE WHEN kind='agent.restart' THEN 'succeeded' ELSE 'failed' END,
				    detail=CASE WHEN kind='agent.restart' THEN '节点已重新启动' ELSE '节点已重新连接' END,
				    finished_at=?,ticket_hash=NULL,ticket_expiry=NULL
				WHERE device_id=? AND state='queued'`, now.UnixMilli(), p.id); err != nil {
				return err
			}
			welcome.LastAck = 0
			serverSeq, serverAck = 0, 0
		} else if hello.LastAck > serverSeq || hello.LastAck < serverAck {
			return ErrDenied
		}
		welcome.TokenRotate, err = s.rotate(ctx, tx, p.id, c, hash, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM device_sessions WHERE device_id=?`, p.id); err != nil {
			return err
		}
		p.gen++
		welcome.ConnGen = p.gen
		welcome.NeedCaps = capsHash != hello.CapsHash || hello.Fresh
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_sessions(device_id,conn_gen,connected_at,remote_ip,agent_ver)
			VALUES (?,?,?,?,?)`, p.id, p.gen, now.UnixMilli(), ip, hello.AgentVersion); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET conn_gen=?,agent_ver=? WHERE id=?", p.gen, hello.AgentVersion, p.id); err != nil {
			return err
		}
		taskProtocol := 0
		if proto.SupportsTasks(platform, hello.CapsHash) {
			taskProtocol = 1
		}
		if proto.SupportsBrain(platform, hello.CapsHash) {
			taskProtocol = 2
		}
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET task_protocol=? WHERE id=?", taskProtocol, p.id); err != nil {
			return err
		}
		if !hello.Fresh {
			if err := commitAck(ctx, tx, p.id, hello.LastAck); err != nil {
				return err
			}
			if taskProtocol == 1 && !hello.TaskInventory {
				if err := workbench.ReconcileLegacyBrain(ctx, tx, p.id, now.UnixMilli()); err != nil {
					return err
				}
			}
			if err := workbench.ReconcileTasks(ctx, tx, p.id, hello, now.UnixMilli()); err != nil {
				return err
			}
		}
		return updateDevice(ctx, tx, p.id, proto.Online, hello.State, now.UnixMilli(), true)
	})
	if err != nil {
		return nil, welcome, err
	}
	if old := s.connections[p.id]; old != nil {
		_ = old.conn.CloseNow()
	}
	s.connections[p.id] = p
	return p, welcome, nil
}

func commitAck(ctx context.Context, tx *sql.Tx, id string, ack uint64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM outbox WHERE device_id=? AND seq<=?", id, ack); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE devices SET controller_ack=? WHERE id=?", ack, id)
	return err
}

func updateDevice(ctx context.Context, tx *sql.Tx, id string, connection proto.ConnState, state proto.DeviceState, now int64, newSession bool) error {
	updated, err := tx.ExecContext(ctx, `UPDATE devices SET conn_state=?,last_seen_at=?,
		helper_state=COALESCE(NULLIF(?,''),helper_state),desktop_state=COALESCE(NULLIF(?,''),desktop_state),
		battery_pct=?,charging=? WHERE id=? AND (? OR conn_state IS NOT ?
		OR helper_state IS NOT COALESCE(NULLIF(?,''),helper_state)
		OR desktop_state IS NOT COALESCE(NULLIF(?,''),desktop_state)
		OR battery_pct IS NOT ? OR charging IS NOT ?)`, connection, now, state.HelperState, state.DesktopState,
		state.BatteryPct, state.Charging, id, newSession, connection, state.HelperState, state.DesktopState, state.BatteryPct, state.Charging)
	if err != nil {
		return err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		_, err := tx.ExecContext(ctx, "UPDATE devices SET last_seen_at=? WHERE id=?", now, id)
		return err
	}
	return workbench.RecordChange(ctx, tx, id)
}

func sendFrame(ctx context.Context, conn *websocket.Conn, kind proto.MessageType, payload any, ack uint64) error {
	frame, err := proto.NewEnvelope(kind, payload)
	if err != nil {
		return err
	}
	frame.Ack = ack
	return writeFrame(ctx, conn, frame)
}

func writeFrame(ctx context.Context, conn *websocket.Conn, frame proto.Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wsjson.Write(ctx, conn, frame)
}

type frameRead struct {
	frame proto.Envelope
	err   error
}

func (s *Service) servePeer(ctx context.Context, p *peer, sent uint64) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if p.terminalFrames != nil {
		go p.sendTerminals(ctx)
	}
	in := make(chan frameRead, 1)
	go func() {
		for {
			var frame proto.Envelope
			err := wsjson.Read(ctx, p.conn, &frame)
			if err == nil && frame.Seq == 0 && (frame.Type == proto.TerminalOutputMessage || frame.Type == proto.TerminalExitMessage) {
				err = p.deliverTerminal(frame)
				if err == nil {
					continue
				}
			}
			if err == nil && frame.Seq == 0 && (frame.Type == proto.SSHStateMessage || frame.Type == proto.SSHResultMessage) {
				if frame.Version != proto.Version || len(frame.ID) != 26 {
					err = ErrInvalid
				} else if frame.Type == proto.SSHStateMessage {
					var st proto.SSHStatePayload
					if json.Unmarshal(frame.Payload, &st) != nil {
						err = ErrInvalid
					} else {
						go s.handleAgentSSHState(p.id, st)
						continue
					}
				} else {
					var res proto.SSHMCPResultPayload
					if json.Unmarshal(frame.Payload, &res) != nil {
						err = ErrInvalid
					} else {
						s.handleAgentSSHResult(p.id, res)
						continue
					}
				}
			}
			select {
			case in <- frameRead{frame, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var ack uint64
	if err := s.store.QueryRowContext(ctx, "SELECT agent_ack FROM devices WHERE id=?", p.id).Scan(&ack); err != nil {
		return "network_error"
	}
	unstable := time.NewTimer(p.interval * 3 / 2)
	offline := time.NewTimer(p.interval * 5 / 2)
	defer unstable.Stop()
	defer offline.Stop()
	for {
		changed := s.store.Changed()
		if err := s.flush(ctx, p, &sent, ack); err != nil {
			return "network_error"
		}
		select {
		case <-ctx.Done():
			return "server_restart"
		case <-changed:
		case <-p.desktopPolicyChanged:
			policy, err := readDesktopPolicy(ctx, s.store, p.id)
			if err != nil || sendFrame(ctx, p.conn, proto.ConfigUpdate, policy, ack) != nil {
				return "network_error"
			}
		case req := <-p.desktopRequests:
			if err := sendFrame(ctx, p.conn, req.kind, req.payload, ack); err != nil {
				return "network_error"
			}
		case frame := <-p.sshFrames:
			frame.Ack = ack
			if err := writeFrame(ctx, p.conn, frame); err != nil {
				return "network_error"
			}
		case <-unstable.C:
			if !p.asleep {
				s.markState(p, proto.Unstable)
			}
		case <-offline.C:
			if p.asleep {
				return "sleeping"
			}
			return "heartbeat_timeout"
		case result := <-in:
			if result.err != nil {
				if ctx.Err() != nil {
					return "server_restart"
				}
				code := websocket.CloseStatus(result.err)
				if p.asleep {
					return "sleeping"
				}
				if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
					return "client_close"
				}
				return "network_error"
			}
			var err error
			ack, err = s.receive(ctx, p, result.frame)
			if err != nil {
				if !errors.Is(err, errStale) && ctx.Err() == nil {
					slog.Warn("device message rejected", "device", p.id, "error", err)
				}
				return "network_error"
			}
			unstable.Reset(p.interval * 3 / 2)
			offline.Reset(p.interval * 5 / 2)
			if err := sendFrame(ctx, p.conn, proto.Pong, struct{}{}, ack); err != nil {
				return "network_error"
			}
		}
	}
}

func readDesktopPolicy(ctx context.Context, q rowQuerier, device string) (proto.DesktopPolicy, error) {
	var policy proto.DesktopPolicy
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT keep_system_awake FROM device_rdp_credentials WHERE device_id=?),0)`,
		device).Scan(&policy.KeepSystemAwake)
	return policy, err
}

func (s *Service) flush(ctx context.Context, p *peer, sent *uint64, ack uint64) error {
	rows, err := s.store.QueryContext(ctx, `SELECT envelope_json FROM outbox
		WHERE device_id=? AND seq>? ORDER BY seq LIMIT 128`, p.id, *sent)
	if err != nil {
		return err
	}
	var frames []proto.Envelope
	for rows.Next() {
		var raw string
		var frame proto.Envelope
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			rows.Close()
			return err
		}
		frames = append(frames, frame)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, frame := range frames {
		frame.Ack = ack
		if err := writeFrame(ctx, p.conn, frame); err != nil {
			return err
		}
		*sent = frame.Seq
	}
	return nil
}

func (s *Service) receive(ctx context.Context, p *peer, frame proto.Envelope) (agentAck uint64, err error) {
	if frame.Version != proto.Version || len(frame.ID) != 26 {
		return 0, ErrInvalid
	}
	if frame.Seq == 0 && frame.Type != proto.Ping && frame.Type != proto.StateUpdate && frame.Type != proto.StateSleeping &&
		frame.Type != proto.TaskHeartbeat && frame.Type != proto.BrowserExtensionStatusResult && frame.Type != proto.RDPResult {
		return 0, ErrInvalid
	}
	if frame.Seq != 0 && frame.Type != proto.CapsReport && frame.Type != proto.OperationResult &&
		frame.Type != proto.AgentUpdateResult && frame.Type != proto.LogReady &&
		frame.Type != proto.TaskAccepted && frame.Type != proto.TaskProgress && frame.Type != proto.TaskHeartbeat &&
		frame.Type != proto.TaskResult && frame.Type != proto.UploadRequest && frame.Type != proto.MCPToolResult {
		return 0, ErrInvalid
	}
	now := time.Now().UnixMilli()
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		var gen, controllerSeq, controllerAck uint64
		err := tx.QueryRowContext(ctx, `SELECT d.conn_gen,d.agent_ack,d.controller_seq,d.controller_ack
			FROM devices d JOIN device_credentials c ON c.device_id=d.id
			WHERE d.id=? AND c.revoked_at IS NULL`, p.id).Scan(&gen, &agentAck, &controllerSeq, &controllerAck)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && gen != p.gen) {
			return errStale
		}
		if err != nil {
			return err
		}
		if frame.Ack > controllerSeq {
			return ErrInvalid
		}
		if frame.Ack > controllerAck {
			if err := commitAck(ctx, tx, p.id, frame.Ack); err != nil {
				return err
			}
		}
		if frame.Seq == 0 {
			if frame.Type == proto.BrowserExtensionStatusResult || frame.Type == proto.RDPResult {
				var result proto.DesktopResult
				if !p.desktopOperations || len(frame.Payload) > 8192 || json.Unmarshal(frame.Payload, &result) != nil {
					return ErrInvalid
				}
				if deliverDesktopResult(p, result) {
					return workbench.RecordChange(ctx, tx, p.id)
				}
				return nil
			}
			if frame.Type == proto.TaskHeartbeat {
				return workbench.ApplyTaskHeartbeat(ctx, tx, p.id, frame.Payload, now)
			}
			var state proto.DeviceState
			if err := json.Unmarshal(frame.Payload, &state); err != nil || !validState(state) {
				return ErrInvalid
			}
			connection := proto.Online
			if frame.Type == proto.StateSleeping {
				connection = proto.Sleeping
			}
			return updateDevice(ctx, tx, p.id, connection, state, now, false)
		}
		if frame.Seq <= agentAck {
			return nil
		}
		if frame.Seq != agentAck+1 {
			return fmt.Errorf("noncontiguous agent sequence")
		}
		entry, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO device_inbox(device_id,id,seq) VALUES (?,?,?)", p.id, frame.ID, frame.Seq)
		if err != nil {
			return err
		}
		newID, err := entry.RowsAffected()
		if err != nil {
			return err
		}
		if newID != 0 {
			if err := s.applyReliable(ctx, tx, p.id, frame, now); err != nil {
				return err
			}
		}
		agentAck = frame.Seq
		if agentAck > 200 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM device_inbox WHERE device_id=? AND seq<=?", p.id, agentAck-200); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET agent_ack=?,last_seen_at=? WHERE id=?", agentAck, now, p.id); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, p.id)
	})
	if err == nil && frame.Seq == 0 && (frame.Type == proto.Ping || frame.Type == proto.StateUpdate || frame.Type == proto.StateSleeping) {
		p.asleep = frame.Type == proto.StateSleeping
	}
	return agentAck, err
}

func (s *Service) applyReliable(ctx context.Context, tx *sql.Tx, id string, frame proto.Envelope, now int64) error {
	switch frame.Type {
	case proto.TaskAccepted, proto.TaskProgress, proto.TaskHeartbeat, proto.TaskResult, proto.UploadRequest, proto.MCPToolResult:
		return workbench.ApplyTaskReport(ctx, tx, id, frame, now)
	}
	if frame.Type == proto.CapsReport {
		var report proto.CapabilitiesReport
		if err := json.Unmarshal(frame.Payload, &report); err != nil || len(report.Hash) > 128 {
			return ErrInvalid
		}
		caps, err := json.Marshal(report.Capabilities)
		if err != nil {
			return err
		}
		tools, err := json.Marshal(workbench.CanonicalTools(report.Tools))
		if err != nil {
			return err
		}
		resources, err := json.Marshal(workbench.CanonicalResources(report.Resources))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE devices SET caps_hash=?,caps_json=?,mcp_tools=?,mcp_resources=? WHERE id=?",
			report.Hash, string(caps), string(tools), string(resources), id)
		return err
	}
	var outcome proto.OperationOutcome
	if err := json.Unmarshal(frame.Payload, &outcome); err != nil || len(outcome.ID) != 26 || len(outcome.Detail) > 4096 {
		return ErrInvalid
	}
	status := "failed"
	if outcome.Success {
		status = "succeeded"
	}
	_, err := tx.ExecContext(ctx, `UPDATE device_operations SET state=?,detail=?,finished_at=?,ticket_hash=NULL,ticket_expiry=NULL
		WHERE id=? AND device_id=? AND (state='queued' OR (kind='agent.uninstall' AND state='succeeded' AND ?=0))
		AND (kind<>'log.collect' OR ?=0 OR uploaded_at IS NOT NULL)`,
		status, outcome.Detail, now, outcome.ID, id, outcome.Success, outcome.Success)
	return err
}

func (s *Service) markState(p *peer, state proto.ConnState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE devices SET conn_state=? WHERE id=? AND conn_gen=?
			AND conn_state NOT IN ('sleeping','offline')`, state, p.id, p.gen)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		return workbench.RecordChange(ctx, tx, p.id)
	})
	if err != nil && s.ctx.Err() == nil {
		slog.Error("mark connection state", "error", err)
	}
}

func (s *Service) detach(p *peer, reason string) {
	_ = p.conn.CloseNow()
	p.detachTerminals()
	s.connectionMu.Lock()
	if s.connections[p.id] == p {
		delete(s.connections, p.id)
	}
	s.connectionMu.Unlock()
	go s.detachHostAgentSSH(p.id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM device_sessions WHERE device_id=? AND conn_gen=?`, p.id, p.gen)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if err := workbench.RequeueUnacknowledged(ctx, tx, p.id); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, p.id)
	})
	if err != nil && s.ctx.Err() == nil {
		slog.Error("close device session", "error", err)
	}
	if reason == "heartbeat_timeout" {
		s.markState(p, proto.Offline)
	} else {
		time.AfterFunc(10*time.Second, func() {
			if s.ctx.Err() == nil {
				s.markState(p, proto.Offline)
			}
		})
	}
}
