package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
	"ghosthand/proto/sshrunner"
)

var (
	ErrTerminalOffline     = errors.New("terminal device is offline")
	ErrTerminalUnsupported = errors.New("device does not support remote terminals")
	ErrTerminalBusy        = errors.New("device terminal limit reached")
)

type TerminalEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

type TerminalSession struct {
	peer           *peer
	service        *Service
	id             string
	targetDeviceID string
	events         chan TerminalEvent
	done           chan struct{}
	opened         bool

	// Direct server-side SSH terminal fields (used when peer == nil).
	localRunner *sshrunner.Manager
	localCmds   chan sshrunner.TerminalCommand
	localCancel context.CancelFunc
	localOnce   sync.Once
}

// ReserveTerminal binds the browser session to a live native Agent (Windows/Linux),
// a host Agent managing a delegated SSH device, or the server's direct sshrunner.
func (s *Service) ReserveTerminal(ctx context.Context, device string) (*TerminalSession, error) {
	if device == workbench.FreeChatDeviceID {
		return nil, ErrTerminalUnsupported
	}
	var platform proto.Platform
	var state proto.ConnState
	var demo bool
	var gen uint64
	err := s.store.QueryRowContext(ctx, `SELECT d.platform,d.is_demo,d.conn_state,d.conn_gen FROM devices d
		LEFT JOIN device_credentials c ON c.device_id=d.id WHERE d.id=? AND c.revoked_at IS NULL`, device).
		Scan(&platform, &demo, &state, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, err
	}
	if (platform != proto.Windows && platform != proto.Linux) || demo {
		return nil, ErrTerminalUnsupported
	}
	if state != proto.Online && state != proto.Unstable {
		return nil, ErrTerminalOffline
	}

	// Check whether this Linux device is an SSH-managed node.
	var hostAgentID sql.NullString
	var sshEnabled bool
	sshErr := s.store.QueryRowContext(ctx, `SELECT host_agent_id, enabled=1 FROM device_ssh_credentials WHERE device_id=?`, device).
		Scan(&hostAgentID, &sshEnabled)
	if sshErr == nil {
		if !sshEnabled {
			return nil, ErrTerminalOffline
		}
		if hostAgentID.Valid && hostAgentID.String != "" {
			// Mode 2A: Delegated SSH — route terminal frames through the selected host Agent.
			return s.reservePeerTerminal(hostAgentID.String, device, 0)
		}
		// Mode 2B: Direct server SSH — open PTY directly on s.sshRunner.
		if s.sshRunner == nil {
			return nil, ErrTerminalOffline
		}
		id, err := proto.NewID()
		if err != nil {
			return nil, err
		}
		s.sshLocalMu.Lock()
		defer s.sshLocalMu.Unlock()
		if len(s.sshLocalSess) >= proto.MaxTerminalSessions*4 {
			return nil, ErrTerminalBusy
		}
		sess := &TerminalSession{
			service:        s,
			id:             id,
			targetDeviceID: device,
			events:         make(chan TerminalEvent, 32),
			done:           make(chan struct{}),
			localRunner:    s.sshRunner,
			localCmds:      make(chan sshrunner.TerminalCommand, 32),
		}
		s.sshLocalSess[id] = sess
		return sess, nil
	}
	if !errors.Is(sshErr, sql.ErrNoRows) {
		return nil, sshErr
	}

	// Mode 1: Native Windows or Linux Agent connected directly via WebSocket.
	return s.reservePeerTerminal(device, "", gen)
}

func (s *Service) reservePeerTerminal(peerDeviceID, targetDeviceID string, expectedGen uint64) (*TerminalSession, error) {
	s.connectionMu.Lock()
	p := s.connections[peerDeviceID]
	s.connectionMu.Unlock()
	if p == nil || (expectedGen != 0 && p.gen != expectedGen) {
		return nil, ErrTerminalOffline
	}
	if p.terminalFrames == nil {
		return nil, ErrTerminalUnsupported
	}
	id, err := proto.NewID()
	if err != nil {
		return nil, err
	}
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	select {
	case <-p.terminalDone:
		return nil, ErrTerminalOffline
	default:
	}
	if len(p.terminalSessions) >= proto.MaxTerminalSessions {
		return nil, ErrTerminalBusy
	}
	session := &TerminalSession{
		peer:           p,
		id:             id,
		targetDeviceID: targetDeviceID,
		events:         make(chan TerminalEvent, 32),
		done:           make(chan struct{}),
	}
	p.terminalSessions[id] = session
	return session, nil
}

func (s *TerminalSession) Events() <-chan TerminalEvent { return s.events }
func (s *TerminalSession) Done() <-chan struct{}        { return s.done }

func (s *TerminalSession) send(ctx context.Context, kind proto.MessageType, payload proto.TerminalFrame) error {
	payload.SessionID = s.id
	frame, err := proto.NewEnvelope(kind, payload)
	if err != nil {
		return err
	}
	select {
	case <-s.peer.terminalDone:
		return ErrTerminalOffline
	case <-ctx.Done():
		return ctx.Err()
	case s.peer.terminalFrames <- frame:
		return nil
	}
}

func (s *TerminalSession) Open(ctx context.Context, shell proto.TerminalShell, cols, rows int) error {
	if s.localRunner != nil {
		localCtx, cancel := context.WithCancel(s.service.ctx)
		s.localCancel = cancel
		s.opened = true
		req := proto.TerminalFrame{
			SessionID:      s.id,
			TargetDeviceID: s.targetDeviceID,
			Shell:          shell,
			Cols:           cols,
			Rows:           rows,
		}
		go func() {
			defer s.finishLocal()
			result := s.localRunner.OpenTerminal(localCtx, req, s.localCmds, func(text string) {
				select {
				case s.events <- TerminalEvent{Type: "output", Data: text}:
				case <-s.done:
				}
			})
			select {
			case s.events <- TerminalEvent{Type: "exit", ExitCode: result.ExitCode, Error: result.Error}:
			case <-s.done:
			}
		}()
		return nil
	}
	err := s.send(ctx, proto.TerminalOpenMessage, proto.TerminalFrame{
		TargetDeviceID: s.targetDeviceID,
		Shell:          shell,
		Cols:           cols,
		Rows:           rows,
	})
	s.opened = err == nil
	return err
}

func (s *TerminalSession) finishLocal() {
	s.localOnce.Do(func() {
		if s.localCancel != nil {
			s.localCancel()
		}
		s.service.sshLocalMu.Lock()
		delete(s.service.sshLocalSess, s.id)
		s.service.sshLocalMu.Unlock()
		close(s.done)
	})
}

func (s *TerminalSession) Input(ctx context.Context, data string) error {
	if s.localRunner != nil {
		select {
		case <-s.done:
			return ErrTerminalOffline
		case <-ctx.Done():
			return ctx.Err()
		case s.localCmds <- sshrunner.TerminalCommand{
			Kind:  proto.TerminalInputMessage,
			Frame: proto.TerminalFrame{SessionID: s.id, Data: data},
		}:
			return nil
		}
	}
	return s.send(ctx, proto.TerminalInputMessage, proto.TerminalFrame{Data: data})
}

func (s *TerminalSession) Resize(ctx context.Context, cols, rows int) error {
	if s.localRunner != nil {
		select {
		case <-s.done:
			return ErrTerminalOffline
		case <-ctx.Done():
			return ctx.Err()
		case s.localCmds <- sshrunner.TerminalCommand{
			Kind:  proto.TerminalResizeMessage,
			Frame: proto.TerminalFrame{SessionID: s.id, Cols: cols, Rows: rows},
		}:
			return nil
		}
	}
	return s.send(ctx, proto.TerminalResizeMessage, proto.TerminalFrame{Cols: cols, Rows: rows})
}

func (s *TerminalSession) Close() {
	if s.localRunner != nil {
		s.finishLocal()
		return
	}
	p := s.peer
	p.terminalMu.Lock()
	p.removeTerminal(s.id)
	p.terminalMu.Unlock()
	if s.opened {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.send(ctx, proto.TerminalCloseMessage, proto.TerminalFrame{}); err != nil && !errors.Is(err, ErrTerminalOffline) {
			// Losing the peer also closes its terminals on the agent.
			_ = p.conn.CloseNow()
		}
	}
}

func (p *peer) sendTerminals(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-p.terminalFrames:
			if writeFrame(ctx, p.conn, frame) != nil {
				_ = p.conn.CloseNow()
				return
			}
		}
	}
}

func (p *peer) deliverTerminal(frame proto.Envelope) error {
	if p.terminalFrames == nil || frame.Version != proto.Version || len(frame.ID) != 26 || frame.Ack != 0 ||
		len(frame.Payload) > proto.MaxTerminalFrameSize {
		return ErrInvalid
	}
	var payload proto.TerminalFrame
	if json.Unmarshal(frame.Payload, &payload) != nil || len(payload.SessionID) != 26 ||
		len(payload.Data) > proto.TerminalChunkSize || len(payload.Error) > 1024 {
		return ErrInvalid
	}
	event := TerminalEvent{Type: "output", Data: payload.Data}
	if frame.Type == proto.TerminalExitMessage {
		event = TerminalEvent{Type: "exit", ExitCode: payload.ExitCode, Error: payload.Error}
	}
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	if session := p.terminalSessions[payload.SessionID]; session != nil {
		select {
		case session.events <- event:
		default:
			// Close a slow browser's session rather than corrupting its VT stream.
			p.removeTerminal(payload.SessionID)
		}
	}
	return nil
}

// The caller holds terminalMu.
func (p *peer) removeTerminal(id string) {
	if session := p.terminalSessions[id]; session != nil {
		delete(p.terminalSessions, id)
		close(session.done)
		close(session.events)
	}
}

func (p *peer) detachTerminals() {
	if p.terminalDone == nil {
		return
	}
	p.terminalMu.Lock()
	defer p.terminalMu.Unlock()
	close(p.terminalDone)
	for id := range p.terminalSessions {
		p.removeTerminal(id)
	}
}
