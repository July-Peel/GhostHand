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
	lockDesktop   bool
	session       uint32
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
	p.overlayMu.Lock()
	defer p.overlayMu.Unlock()
	if update.Visible {
		if p.taskOverlay.TaskID == update.TaskID && p.taskOverlay.State == proto.Cancelling && update.State != proto.Cancelling {
			return
		}
		p.taskOverlay = update
	} else if p.taskOverlay.TaskID == update.TaskID {
		p.taskOverlay = taskOverlay{}
	}
	if ipc := p.helperLink(); ipc != nil {
		_ = ipc.send(0, "task.overlay", update, nil)
	}
}

func (p *host) markOverlayCancelling(task string) {
	p.overlayMu.Lock()
	defer p.overlayMu.Unlock()
	if p.taskOverlay.TaskID == task {
		p.taskOverlay.State = proto.Cancelling
		if ipc := p.helperLink(); ipc != nil {
			_ = ipc.send(0, "task.overlay", p.taskOverlay, nil)
		}
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
	return p.callHelperSession(ctx, 0, task, method, params)
}

func (p *host) callHelperSession(ctx context.Context, session uint32, task, method string, params any) (captureReply, error) {
	if err := ctx.Err(); err != nil {
		return captureReply{}, err
	}
	snapshot := p.snapshot()
	if snapshot.DesktopState != "unlocked" {
		return captureReply{}, taskFailure{proto.ErrDesktopLocked, errors.New("desktop is locked or unavailable")}
	}
	ipc := p.helperLink()
	if ipc == nil || (session != 0 && ipc.session != session) {
		return captureReply{}, taskFailure{proto.ErrHelperUnavailable, errors.New("Helper unavailable")}
	}
	if method == "mcp.call" && !ipc.mcp {
		return captureReply{}, taskFailure{proto.ErrCapUnsupported, errors.New("Windows Helper 需要升级以执行 AI 工具")}
	}
	if strings.HasPrefix(method, "browser.") && !ipc.browserBridge {
		return captureReply{}, taskFailure{proto.ErrCapUnsupported, errors.New("Windows Helper 需要升级以管理浏览器扩展")}
	}
	if method == "desktop.lock" && !ipc.lockDesktop {
		return captureReply{}, taskFailure{proto.ErrHelperUnavailable, errors.New("Windows Helper 需要升级以锁定桌面")}
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
			return captureReply{}, ctx.Err()
		case <-ipc.done:
		case <-timeout.C:
			_ = ipc.conn.Close()
		}
		ipc.waitStopped()
		return captureReply{}, ctx.Err()
	case <-ipc.done:
		ipc.waitStopped()
		if err := ctx.Err(); err != nil {
			return captureReply{}, err
		}
		return captureReply{}, taskFailure{proto.ErrHelperUnavailable, errors.New("Helper disconnected")}
	case result := <-reply:
		if err := ctx.Err(); err != nil {
			return captureReply{}, err
		}
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
			case "E_CANCELLED":
				return captureReply{}, context.Canceled
			}
			return captureReply{}, taskFailure{code, errors.New(result.Error)}
		}
		return result, nil
	}
}

func (l *helperLink) waitStopped() {
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	select {
	case <-l.stopped:
	case <-timeout.C:
	}
}
