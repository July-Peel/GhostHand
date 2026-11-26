package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"ghosthand/launcher/state"
	"ghosthand/proto"
)

type helperProcess struct {
	session  uint32
	waitFn   func() error
	killFn   func() error
	listener net.Listener
	nonce    string
}

func (p *host) superviseHelper(ctx context.Context, path string, events <-chan struct{}) {
	var failuresAt time.Time
	var requested chan error
	defer func() {
		if requested != nil {
			requested <- errors.New("helper supervisor stopped")
		}
	}()
	inDesktopSession := currentProcessSessionID() > 0
	for ctx.Err() == nil {
		var session uint32
		var token windows.Token
		var hasSession bool
		if inDesktopSession {
			session = currentProcessSessionID()
			hasSession = true
		} else {
			session, token, hasSession = interactiveSession()
		}
		if !hasSession {
			p.set("no_session", "unavailable")
			if requested != nil {
				requested <- errors.New("no interactive desktop session")
				requested = nil
			}
			select {
			case <-ctx.Done():
			case <-events:
			case request := <-p.restart:
				request <- errors.New("no interactive desktop session")
			}
			continue
		}
		health := "restarting"
		if !failuresAt.IsZero() && time.Since(failuresAt) >= 10*time.Minute {
			health = "unavailable"
		}
		p.set(desktopState(session), health)
		var helper *helperProcess
		var err error
		if inDesktopSession {
			helper, err = launchHelperInCurrentSession(session, path)
		} else {
			helper, err = launchHelper(session, token, path)
			token.Close()
		}
		if err == nil {
			ready, request, runErr := p.watchHelper(ctx, helper, events, requested, inDesktopSession)
			requested = request
			err = runErr
			if ready {
				failuresAt = time.Time{}
			}
		}
		if err == nil || ctx.Err() != nil {
			continue
		}
		slog.Warn("helper exited", "error", err)
		if failuresAt.IsZero() {
			failuresAt = time.Now()
		}
		if requested != nil {
			requested <- err
			requested = nil
		}
		p.set("", "restarting")
		delay := 2 * time.Second
		if time.Since(failuresAt) >= 10*time.Minute {
			p.set("", "unavailable")
			delay = 15 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-events:
		case requested = <-p.restart:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func launchHelper(session uint32, token windows.Token, path string) (*helperProcess, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("Helper path must be absolute")
	}
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	nonce, err := proto.NewToken()
	if err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(`\\.\pipe\`+state.PipeName, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;0x12019b;;;" + user.User.Sid.String() + ")",
		InputBufferSize:    4096, OutputBufferSize: 4096,
	})
	if err != nil {
		return nil, err
	}
	handle, err := launchElevatedInSession(token, path, nonce)
	if err != nil {
		listener.Close()
		return nil, err
	}
	slog.Info("helper launched", "session", session)
	waitFn := func() error {
		_, err := windows.WaitForSingleObject(handle, windows.INFINITE)
		windows.CloseHandle(handle)
		return err
	}
	killFn := func() error {
		return windows.TerminateProcess(handle, 1)
	}
	return &helperProcess{session: session, waitFn: waitFn, killFn: killFn, listener: listener, nonce: nonce}, nil
}

func launchHelperInCurrentSession(session uint32, path string) (*helperProcess, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("Helper path must be absolute")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	nonce, err := proto.NewToken()
	if err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(`\\.\pipe\`+state.PipeName, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;0x12019b;;;" + user.User.Sid.String() + ")",
		InputBufferSize:    4096, OutputBufferSize: 4096,
	})
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, "--service", "--pipe", state.PipeName, "--nonce", nonce)
	cmd.Dir = filepath.Dir(path)
	if err := cmd.Start(); err != nil {
		listener.Close()
		return nil, err
	}
	slog.Info("helper launched in current session", "session", session)
	return &helperProcess{
		session:  session,
		waitFn:   cmd.Wait,
		killFn:   func() error { return cmd.Process.Kill() },
		listener: listener,
		nonce:    nonce,
	}, nil
}

func launchElevatedInSession(userToken windows.Token, path, nonce string) (windows.Handle, error) {
	if elevated, ok := elevateUserToken(userToken, true); ok {
		handle, err := startInSession(elevated, path, nonce)
		elevated.Close()
		if err == nil {
			return handle, nil
		}
		slog.Warn("launch helper with UIAccess failed, retrying elevated token without UIAccess", "error", err)
		if plainElevated, ok2 := elevateUserToken(userToken, false); ok2 {
			handle, err = startInSession(plainElevated, path, nonce)
			plainElevated.Close()
			if err == nil {
				return handle, nil
			}
			slog.Warn("launch helper with elevated token failed, falling back to session token", "error", err)
		}
	}
	return startInSession(userToken, path, nonce)
}

func startInSession(token windows.Token, path, nonce string) (windows.Handle, error) {
	var environment *uint16
	if err := windows.CreateEnvironmentBlock(&environment, token, false); err != nil {
		return 0, err
	}
	defer windows.DestroyEnvironmentBlock(environment)
	exe, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	cwd, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	args, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{path, "--service", "--pipe", state.PipeName, "--nonce", nonce}))
	if err != nil {
		return 0, err
	}
	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	info := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Desktop: desktop}
	var process windows.ProcessInformation
	err = windows.CreateProcessAsUser(token, exe, args, nil, nil, false, windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_BREAKAWAY_FROM_JOB,
		environment, cwd, &info, &process)
	if err != nil {
		return 0, err
	}
	windows.CloseHandle(process.Thread)
	return process.Process, nil
}

func (p *host) watchHelper(ctx context.Context, h *helperProcess, events <-chan struct{}, request chan error, inDesktopSession bool) (bool, chan error, error) {
	ctx, cancel := context.WithCancel(ctx)
	exit := make(chan error, 1)
	go func() {
		exit <- h.waitFn()
	}()
	waited := false
	stopped := make(chan struct{})
	defer func() {
		defer close(stopped)
		cancel()
		h.listener.Close()
		if !waited {
			_ = h.killFn()
			<-exit
		}
	}()
	registered := make(chan struct{}, 1)
	pipeDone := make(chan error, 1)
	go func() { pipeDone <- h.serve(ctx, p, registered, stopped) }()
	first := time.NewTimer(12 * time.Second)
	defer first.Stop()

	ready := false
	for {
		select {
		case <-ctx.Done():
			return ready, request, nil
		case err := <-exit:
			waited = true
			if err == nil {
				err = errors.New("helper process stopped")
			}
			return ready, request, err
		case err := <-pipeDone:
			return ready, request, err
		case <-first.C:
			return ready, request, errors.New("helper did not register")
		case <-registered:
			first.Stop()
			ready = true
			p.set("", "ready")
			if request != nil {
				request <- nil
				request = nil
			}
		case <-events:
			if inDesktopSession {
				p.set(desktopState(h.session), "")
				continue
			}
			session, token, active := interactiveSession()
			if active {
				token.Close()
			}
			if !active || session != h.session {
				return ready, request, nil
			}
			p.set(desktopState(session), "")
		case next := <-p.restart:
			if request != nil {
				request <- errors.New("helper restart already in progress")
			}
			return ready, next, nil
		}
	}
}

type rpcRequest struct {
	Version string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		Nonce         string `json:"nonce"`
		Tasks         bool   `json:"tasks"`
		MCP           bool   `json:"mcp"`
		BrowserBridge bool   `json:"browser_bridge"`
	} `json:"params"`
}

func (h *helperProcess) serve(ctx context.Context, p *host, registered chan struct{}, stopped <-chan struct{}) error {
	for ctx.Err() == nil {
		conn, err := h.listener.Accept()
		if err != nil {
			return err
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		reader := bufio.NewScanner(conn)
		reader.Buffer(make([]byte, 1024), 3*proto.MaxScreenshotSize/2)
		var request rpcRequest
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		valid := reader.Scan() && json.Unmarshal(reader.Bytes(), &request) == nil && request.Version == "2.0" &&
			request.Method == "helper.register" && request.ID > 0 &&
			subtle.ConstantTimeCompare([]byte(request.Params.Nonce), []byte(h.nonce)) == 1
		if !valid {
			stop()
			conn.Close()
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := json.NewEncoder(conn).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": true}); err != nil {
			stop()
			conn.Close()
			return err
		}
		link := &helperLink{conn: conn, done: make(chan struct{}), stopped: stopped,
			waiting: make(map[int]chan captureReply), mcp: request.Params.MCP, browserBridge: request.Params.BrowserBridge}
		if request.Params.Tasks {
			p.mu.Lock()
			p.ipc = link
			p.mu.Unlock()
		}
		signal(registered)
		err = p.readHelper(link, reader)
		p.mu.Lock()
		if p.ipc == link {
			p.ipc = nil
		}
		p.mu.Unlock()
		close(link.done)
		stop()
		conn.Close()
		return err
	}
	return ctx.Err()
}

func (p *host) readHelper(link *helperLink, reader *bufio.Scanner) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		var frame struct {
			Version string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
		}
		_ = link.conn.SetReadDeadline(deadline)
		if !reader.Scan() {
			return errors.New("helper heartbeat stopped")
		}
		if json.Unmarshal(reader.Bytes(), &frame) != nil || frame.Version != "2.0" {
			return errors.New("invalid Helper message")
		}
		switch {
		case frame.Method == "helper.heartbeat" && frame.ID > 0:
			deadline = time.Now().Add(30 * time.Second)
			if err := link.send(frame.ID, "", nil, true); err != nil {
				return err
			}
		case frame.Method == "task.control" && frame.ID == 0:
			var control overlayControl
			if json.Unmarshal(frame.Params, &control) != nil || !proto.TaskIDPattern.MatchString(control.TaskID) ||
				(control.Action != "pause" && control.Action != "resume" && control.Action != "cancel") {
				return errors.New("invalid Helper task control")
			}
			select {
			case p.controls <- control:
			default:
			}
		case frame.Method == "" && frame.ID > 0:
			var reply captureReply
			if json.Unmarshal(frame.Result, &reply) != nil {
				return errors.New("invalid Helper capture response")
			}
			link.mu.Lock()
			if pending := link.waiting[frame.ID]; pending != nil {
				select {
				case pending <- reply:
				default:
				}
			}
			link.mu.Unlock()
		default:
			return errors.New("unexpected Helper message")
		}
	}
}
