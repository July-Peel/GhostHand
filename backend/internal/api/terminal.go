package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
	"ghosthand/proto"
)

type terminalCommand struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request, login auth.Session, token string) {
	query := r.URL.Query()
	shell := proto.TerminalShell(query.Get("shell"))
	cols, rows := 120, 32
	var colsErr, rowsErr error
	if query.Has("cols") {
		cols, colsErr = strconv.Atoi(query.Get("cols"))
	}
	if query.Has("rows") {
		rows, rowsErr = strconv.Atoi(query.Get("rows"))
	}
	if !shell.Valid() || colsErr != nil || rowsErr != nil || cols < 1 || rows < 1 {
		fail(w, http.StatusBadRequest, "终端类型或尺寸无效。")
		return
	}
	cols, rows = proto.TerminalDimensions(cols, rows)
	terminal, err := s.agents.ReserveTerminal(r.Context(), r.PathValue("device"))
	if err != nil {
		switch {
		case errors.Is(err, agents.ErrMissing):
			fail(w, http.StatusNotFound, "设备不存在。")
		case errors.Is(err, agents.ErrTerminalOffline):
			fail(w, http.StatusConflict, "设备离线。")
		case errors.Is(err, agents.ErrTerminalUnsupported):
			fail(w, http.StatusConflict, "此设备不支持 Windows 远程终端。")
		case errors.Is(err, agents.ErrTerminalBusy):
			fail(w, http.StatusTooManyRequests, "设备已达到终端会话上限。")
		default:
			internalError(w, r, err)
		}
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		terminal.Close()
		return
	}
	defer conn.CloseNow()
	defer terminal.Close()
	conn.SetReadLimit(proto.MaxTerminalFrameSize)
	ctx, cancel := context.WithDeadline(r.Context(), login.ExpiresAt)
	defer cancel()
	openCtx, cancelOpen := context.WithTimeout(ctx, 5*time.Second)
	err = terminal.Open(openCtx, shell, cols, rows)
	cancelOpen()
	if err != nil {
		_ = writeTerminalEvent(ctx, conn, agents.TerminalEvent{Type: "exit", Error: "无法连接设备终端。"})
		return
	}
	go func() {
		defer cancel()
		for {
			var cmd terminalCommand
			if wsjson.Read(ctx, conn, &cmd) != nil {
				return
			}
			sendCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			var err error
			switch cmd.Type {
			case "input":
				if len(cmd.Data) > proto.TerminalChunkSize {
					err = agents.ErrInvalid
				} else if cmd.Data != "" {
					err = terminal.Input(sendCtx, cmd.Data)
				}
			case "resize":
				width, height := proto.TerminalDimensions(cmd.Cols, cmd.Rows)
				err = terminal.Resize(sendCtx, width, height)
			default:
				err = agents.ErrInvalid
			}
			stop()
			if err != nil {
				return
			}
		}
	}()
	checks := time.NewTicker(20 * time.Second)
	defer checks.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-terminal.Done():
			_ = writeTerminalEvent(ctx, conn, agents.TerminalEvent{Type: "exit", Error: "设备终端连接已中断。"})
			return
		case event, ok := <-terminal.Events():
			if !ok {
				_ = writeTerminalEvent(ctx, conn, agents.TerminalEvent{Type: "exit", Error: "设备终端连接已中断。"})
				return
			}
			if writeTerminalEvent(ctx, conn, event) != nil || event.Type == "exit" {
				return
			}
		case <-checks.C:
			checkCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			_, err := s.auth.Session(checkCtx, token)
			if err == nil {
				err = conn.Ping(checkCtx)
			}
			stop()
			if err != nil {
				return
			}
		}
	}
}

func writeTerminalEvent(ctx context.Context, conn *websocket.Conn, event agents.TerminalEvent) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wsjson.Write(ctx, conn, event)
}
