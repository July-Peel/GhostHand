package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
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
	peer   *peer
	id     string
	events chan TerminalEvent
	done   chan struct{}
	opened bool
}

// ReserveTerminal binds the browser session to a single live connection generation.
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
	if platform != proto.Windows || demo {
		return nil, ErrTerminalUnsupported
	}
	if state != proto.Online && state != proto.Unstable {
		return nil, ErrTerminalOffline
	}
	s.connectionMu.Lock()
	p := s.connections[device]
	s.connectionMu.Unlock()
	if p == nil || p.gen != gen {
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
	session := &TerminalSession{peer: p, id: id, events: make(chan TerminalEvent, 32), done: make(chan struct{})}
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
	err := s.send(ctx, proto.TerminalOpenMessage, proto.TerminalFrame{Shell: shell, Cols: cols, Rows: rows})
	s.opened = err == nil
	return err
}

func (s *TerminalSession) Input(ctx context.Context, data string) error {
	return s.send(ctx, proto.TerminalInputMessage, proto.TerminalFrame{Data: data})
}

func (s *TerminalSession) Resize(ctx context.Context, cols, rows int) error {
	return s.send(ctx, proto.TerminalResizeMessage, proto.TerminalFrame{Cols: cols, Rows: rows})
}

func (s *TerminalSession) Close() {
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
