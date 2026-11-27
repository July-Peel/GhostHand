package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"ghosthand/launcher/state"
	"ghosthand/proto"
	"ghosthand/proto/sshrunner"
)

type terminalEvent struct {
	kind  proto.MessageType
	frame proto.TerminalFrame
}

type terminalSession struct {
	cancel   context.CancelCauseFunc
	commands chan terminalEvent
}

type terminalManager struct {
	ctx      context.Context
	ssh      *sshrunner.Manager
	events   chan terminalEvent
	mu       sync.Mutex
	sessions map[string]*terminalSession
	workers  sync.WaitGroup
}

var errTerminalBacklog = errors.New("终端输入积压过多，会话已结束。")

func newTerminalManager(ctx context.Context, sshMgr *sshrunner.Manager) *terminalManager {
	return &terminalManager{ctx: ctx, ssh: sshMgr, events: make(chan terminalEvent, 32), sessions: make(map[string]*terminalSession)}
}

func isTerminalCommand(kind proto.MessageType) bool {
	switch kind {
	case proto.TerminalOpenMessage, proto.TerminalInputMessage, proto.TerminalResizeMessage, proto.TerminalCloseMessage:
		return true
	}
	return false
}

func (m *terminalManager) receive(envelope proto.Envelope) error {
	var request proto.TerminalFrame
	if envelope.Version != proto.Version || envelope.Ack != 0 || !state.IDPattern.MatchString(envelope.ID) ||
		len(envelope.Payload) > proto.MaxTerminalFrameSize || json.Unmarshal(envelope.Payload, &request) != nil ||
		!state.IDPattern.MatchString(request.SessionID) || len(request.Data) > proto.TerminalChunkSize {
		return errors.New("invalid terminal message")
	}
	if envelope.Type == proto.TerminalOpenMessage {
		if !request.Shell.Valid() || (request.TargetDeviceID != "" && !state.IDPattern.MatchString(request.TargetDeviceID)) {
			return errors.New("invalid terminal shell")
		}
		m.open(request)
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[request.SessionID]
	if session == nil {
		return nil
	}
	if envelope.Type == proto.TerminalCloseMessage {
		session.cancel(context.Canceled)
		return nil
	}
	select {
	case session.commands <- terminalEvent{envelope.Type, request}:
	default:
		session.cancel(errTerminalBacklog)
	}
	return nil
}

func (m *terminalManager) open(request proto.TerminalFrame) {
	m.mu.Lock()
	if m.ctx.Err() != nil || m.sessions[request.SessionID] != nil {
		m.mu.Unlock()
		return
	}
	if len(m.sessions) >= proto.MaxTerminalSessions {
		m.mu.Unlock()
		m.emit(m.ctx, proto.TerminalExitMessage, proto.TerminalFrame{SessionID: request.SessionID, Error: "设备已达到终端会话上限。"})
		return
	}
	ctx, cancel := context.WithCancelCause(m.ctx)
	session := &terminalSession{cancel: cancel, commands: make(chan terminalEvent, 32)}
	m.sessions[request.SessionID] = session
	m.workers.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		defer cancel(context.Canceled)
		defer func() {
			m.mu.Lock()
			delete(m.sessions, request.SessionID)
			m.mu.Unlock()
		}()
		outputFn := func(text string) {
			m.emit(ctx, proto.TerminalOutputMessage, proto.TerminalFrame{SessionID: request.SessionID, Data: text})
		}
		var result proto.TerminalFrame
		if request.TargetDeviceID != "" {
			if m.ssh == nil {
				result = proto.TerminalFrame{Error: "当前节点未启用 SSH 终端代理。"}
			} else {
				sshCmds := make(chan sshrunner.TerminalCommand, 32)
				go func() {
					defer close(sshCmds)
					for {
						select {
						case <-ctx.Done():
							return
						case ev := <-session.commands:
							select {
							case sshCmds <- sshrunner.TerminalCommand{Kind: ev.kind, Frame: ev.frame}:
							case <-ctx.Done():
								return
							}
						}
					}
				}()
				result = m.ssh.OpenTerminal(ctx, request, sshCmds, outputFn)
			}
		} else {
			result = runTerminal(ctx, request, session.commands, outputFn)
		}
		if cause := context.Cause(ctx); errors.Is(cause, errTerminalBacklog) {
			result.Error = cause.Error()
		} else if cause != nil {
			return
		}
		result.SessionID = request.SessionID
		m.emit(m.ctx, proto.TerminalExitMessage, result)
	}()
}

func (m *terminalManager) emit(ctx context.Context, kind proto.MessageType, frame proto.TerminalFrame) {
	if ctx.Err() != nil {
		return
	}
	select {
	case m.events <- terminalEvent{kind, frame}:
	case <-ctx.Done():
	}
}

func (m *terminalManager) closeAll() {
	m.mu.Lock()
	for _, session := range m.sessions {
		session.cancel(context.Canceled)
	}
	m.mu.Unlock()
	m.workers.Wait()
}
