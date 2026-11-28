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
	sshAttempt     *sshAttempt
	events         chan TerminalEvent
	done           chan struct{}
	opened         bool

	// Direct server-side SSH terminal fields (used when peer == nil).
	localRunner *sshrunner.Manager
	localCmds   chan sshrunner.TerminalCommand
	localCtx    context.Context
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
	var demo, sshManaged bool
	var gen uint64
	err := s.store.QueryRowContext(ctx, `SELECT d.platform,d.is_demo,d.conn_state,d.conn_gen,
		EXISTS(SELECT 1 FROM device_ssh_credentials WHERE device_id=d.id) FROM devices d
		LEFT JOIN device_credentials c ON c.device_id=d.id WHERE d.id=? AND c.revoked_at IS NULL`, device).
		Scan(&platform, &demo, &state, &gen, &sshManaged)
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

	if sshManaged {
		s.sshMu.Lock()
		defer s.sshMu.Unlock()
		attempt := s.sshAttempts[device]
		if attempt == nil || !attempt.online {
			return nil, ErrTerminalOffline
		}
		if attempt.host != nil {
			return s.reservePeerTerminal(attempt.host, device, attempt)
		}
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
		localCtx, cancel := context.WithCancel(s.ctx)
		sess := &TerminalSession{
			service:        s,
			id:             id,
			targetDeviceID: device,
			sshAttempt:     attempt,
			events:         make(chan TerminalEvent, 32),
			done:           make(chan struct{}),
			localRunner:    s.sshRunner,
			localCmds:      make(chan sshrunner.TerminalCommand, 32),
			localCtx:       localCtx,
			localCancel:    cancel,
		}
		s.sshLocalSess[id] = sess
		return sess, nil
	}

	s.connectionMu.Lock()
	p := s.connections[device]
	s.connectionMu.Unlock()
	if p == nil || p.gen != gen {
		return nil, ErrTerminalOffline
	}
	return s.reservePeerTerminal(p, "", nil)
}

func (s *Service) reservePeerTerminal(p *peer, targetDeviceID string, attempt *sshAttempt) (*TerminalSession, error) {
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
		service:        s,
		id:             id,
		targetDeviceID: targetDeviceID,
		sshAttempt:     attempt,
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

func (s *TerminalSession) recordSSHActivity() error {
	if s.sshAttempt == nil {
		return nil
	}
	s.service.sshMu.Lock()
	defer s.service.sshMu.Unlock()
	if s.service.sshAttempts[s.targetDeviceID] != s.sshAttempt {
		return ErrTerminalOffline
	}
	s.sshAttempt.lastActivity = time.Now()
	return nil
}

func (s *TerminalSession) Open(ctx context.Context, shell proto.TerminalShell, cols, rows int) error {
	if err := s.recordSSHActivity(); err != nil {
		return err
	}
	if s.localRunner != nil {
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
			result := s.localRunner.OpenTerminal(s.localCtx, req, s.localCmds, func(text string) {
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
		s.localCancel()
		s.service.sshLocalMu.Lock()
		delete(s.service.sshLocalSess, s.id)
		s.service.sshLocalMu.Unlock()
		close(s.done)
	})
}

func (s *TerminalSession) Input(ctx context.Context, data string) error {
	if err := s.recordSSHActivity(); err != nil {
		return err
	}
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
