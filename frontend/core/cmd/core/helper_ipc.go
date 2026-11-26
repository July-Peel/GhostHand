package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"ghosthand/proto"
)

type taskOverlay struct {
	proto.TaskDisplay
	Visible bool `json:"visible"`
}

type overlayControl struct {
	proto.TaskRef
	Action string `json:"action"`
}

type helperLink struct {
	conn          net.Conn
	stopped       <-chan struct{}
	done          chan struct{}
	writeMu       sync.Mutex
	mu            sync.Mutex
	next          int
	waiting       map[int]chan captureReply
	mcp           bool
	browserBridge bool
}

type captureReply struct {
	JPEG  []byte          `json:"jpeg,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

type helperFrame struct {
	Version string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
	Result  any    `json:"result,omitempty"`
}

func (l *helperLink) send(id int, method string, params, result any) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	_ = l.conn.SetWriteDeadline(time.Now().Add(time.Second))
	return json.NewEncoder(l.conn).Encode(helperFrame{Version: "2.0", ID: id, Method: method, Params: params, Result: result})
}

func (p *host) helperLink() *helperLink {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ipc
}

func (p *host) overlay(update taskOverlay) {
	if ipc := p.helperLink(); ipc != nil {
		_ = ipc.send(0, "task.overlay", update, nil)
	}
}

func (p *host) forceHelper() {
	if ipc := p.helperLink(); ipc != nil {
		// The supervisor treats a broken pipe as a failed Helper and terminates its process.
		_ = ipc.conn.Close()
	}
}

func (p *host) capture(ctx context.Context, task string) ([]byte, error) {
	result, err := p.callHelper(ctx, task, "screen.capture", proto.TaskRef{TaskID: task})
	if err != nil {
		return nil, err
	}
	if len(result.JPEG) == 0 || len(result.JPEG) > proto.MaxScreenshotSize {
		return nil, taskFailure{proto.ErrInternal, errors.New("invalid screenshot response")}
	}
	return result.JPEG, nil
}

func (p *host) callHelper(ctx context.Context, task, method string, params any) (captureReply, error) {
	if err := ctx.Err(); err != nil {
		return captureReply{}, err
	}
	snapshot := p.snapshot()
	if snapshot.DesktopState != "unlocked" {
		return captureReply{}, taskFailure{proto.ErrDesktopLocked, errors.New("desktop is locked or unavailable")}
	}
	ipc := p.helperLink()
	if ipc == nil {
		return captureReply{}, taskFailure{proto.ErrHelperUnavailable, errors.New("Helper unavailable")}
	}
	if method == "mcp.call" && !ipc.mcp {
		return captureReply{}, taskFailure{proto.ErrCapUnsupported, errors.New("Windows Helper 需要升级以执行 AI 工具")}
	}
	if strings.HasPrefix(method, "browser.") && !ipc.browserBridge {
		return captureReply{}, taskFailure{proto.ErrCapUnsupported, errors.New("Windows Helper 需要升级以管理浏览器扩展")}
	}
	reply := make(chan captureReply, 1)
	ipc.mu.Lock()
	ipc.next++
	id := ipc.next
	ipc.waiting[id] = reply
	ipc.mu.Unlock()
	defer func() {
		ipc.mu.Lock()
		delete(ipc.waiting, id)
		ipc.mu.Unlock()
	}()
	ref := proto.TaskRef{TaskID: task}
	if err := ipc.send(id, method, params, nil); err != nil {
		return captureReply{}, taskFailure{proto.ErrHelperUnavailable, err}
	}
	select {
	case <-ctx.Done():
		_ = ipc.send(0, "screen.cancel", ref, nil)
		timeout := time.NewTimer(10 * time.Second)
		defer timeout.Stop()
		select {
		case <-reply:
		case <-ipc.done:
			select {
			case <-ipc.stopped:
			case <-timeout.C:
			}
		case <-timeout.C:
			_ = ipc.conn.Close()
		}
		return captureReply{}, ctx.Err()
	case <-ipc.done:
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		select {
		case <-ipc.stopped:
		case <-deadline.C:
		}
		if err := ctx.Err(); err != nil {
			return captureReply{}, err
		}
		return captureReply{}, taskFailure{proto.ErrHelperUnavailable, errors.New("Helper disconnected")}
	case result := <-reply:
		if result.Error != "" {
			code := proto.ErrInternal
			switch result.Error {
			case "E_DESKTOP_LOCKED":
				code = proto.ErrDesktopLocked
			case "E_TIMEOUT":
				code = proto.ErrTimeout
			case "E_CAP_UNSUPPORTED":
				code = proto.ErrCapUnsupported
			case "E_BUSY":
				code = proto.ErrBusy
			}
			return captureReply{}, taskFailure{code, errors.New(result.Error)}
		}
		return result, nil
	}
}
