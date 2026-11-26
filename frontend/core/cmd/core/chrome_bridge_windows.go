package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"ghosthand/proto"
)

const (
	browserPipe          = `\\.\pipe\GhostHandChromeBridge`
	browserHostName      = "com.ghosthand.nativemessage"
	browserExtensionID   = "mfbgehccogcjllkhnmhdkhfmbfbgjjnh"
	browserOrigin        = "chrome-extension://" + browserExtensionID + "/"
	browserPipeAccess    = 0x12019b // Read/write and synchronize, without permission to create pipe instances.
	browserMaxRequest    = 512 << 10
	browserMaxResponse   = 8 << 20
	browserHeartbeatWait = 50 * time.Second
)

type browserRequest struct {
	ID      string          `json:"requestId,omitempty"`
	Code    string          `json:"code"`
	TabID   *int            `json:"tabId,omitempty"`
	FrameID *int            `json:"frameId,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type browserResponse struct {
	ID     string          `json:"requestId"`
	Code   string          `json:"Code"`
	Status bool            `json:"Status"`
	TabID  *int            `json:"TabId,omitempty"`
	URL    string          `json:"Url,omitempty"`
	Title  string          `json:"Title,omitempty"`
	Data   json.RawMessage `json:"Data,omitempty"`
	Error  string          `json:"Error,omitempty"`
}

type browserDesktop struct {
	session uint32
	sid     string
}

type browserSessionKey struct {
	browserDesktop
	browser string
}

type browserBridge struct {
	host      *host
	core      string
	startErr  error
	mu        sync.Mutex
	sessions  map[browserSessionKey][]*browserPeer
	preferred map[browserDesktop]string
	change    chan struct{}
	lifecycle chan struct{}
}

type browserPeer struct {
	conn    net.Conn
	done    chan struct{}
	writeMu sync.Mutex
	mu      sync.Mutex
	waiting map[string]chan browserResponse
}

func readBrowserFrame(reader io.Reader, maxSize int) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(header[:])
	if length == 0 || length > uint32(maxSize) {
		return nil, errors.New("invalid Native Messaging frame length")
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	return data, nil
}

func writeBrowserFrame(writer io.Writer, data []byte, maxSize int) error {
	if len(data) == 0 || len(data) > maxSize {
		return errors.New("Native Messaging frame size exceeds limit")
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(data)))
	for _, part := range [][]byte{header[:], data} {
		if n, err := writer.Write(part); err != nil {
			return err
		} else if n != len(part) {
			return io.ErrShortWrite
		}
	}
	return nil
}

func runBrowserProcess(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case browserOrigin, "chrome-extension://" + browserExtensionID, "--native-host":
		return true, runNativeMessagingHost()
	case "--cleanup-browser":
		if len(args) != 1 {
			return true, errors.New("invalid browser cleanup arguments")
		}
		return true, runLocalBrowserCleanup()
	}
	return false, nil
}

func runNativeMessagingHost() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pipe, err := dialBrowserPipe(ctx)
	if err != nil {
		return err
	}
	defer pipe.Close()
	done := make(chan error, 1)
	go func() { done <- copyBrowserFrames(pipe, os.Stdin, browserMaxResponse) }()
	out := make(chan error, 1)
	go func() { out <- copyBrowserFrames(os.Stdout, pipe, browserMaxRequest) }()
	select {
	case err = <-done:
	case err = <-out:
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func copyBrowserFrames(dst io.Writer, src io.Reader, limit int) error {
	for {
		data, err := readBrowserFrame(src, limit)
		if err != nil {
			return err
		}
		if err := writeBrowserFrame(dst, data, limit); err != nil {
			return err
		}
	}
}

func dialBrowserPipe(ctx context.Context) (net.Conn, error) {
	conn, err := winio.DialPipeAccess(ctx, browserPipe, browserPipeAccess)
	if err != nil {
		return nil, err
	}
	handle, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		conn.Close()
		return nil, errors.New("missing browser pipe handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(handle.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		sd, err = windows.GetSecurityInfo(windows.Handle(handle.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	}
	if err == nil {
		if owner, _, ownerErr := sd.Owner(); ownerErr == nil {
			user, userErr := windows.GetCurrentProcessToken().GetTokenUser()
			if owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
				(userErr == nil && owner.Equals(user.User.Sid)) {
				return conn, nil
			}
		}
	}
	conn.Close()
	return nil, errors.New("untrusted GhostHand browser pipe owner")
}

func startBrowserBridge(ctx context.Context, host *host, core string) *browserBridge {
	bridge := &browserBridge{host: host, core: core, sessions: make(map[browserSessionKey][]*browserPeer),
		preferred: make(map[browserDesktop]string), change: make(chan struct{}), lifecycle: make(chan struct{}, 1)}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		bridge.startErr = err
		return bridge
	}
	listener, err := winio.ListenPipe(browserPipe, &winio.PipeConfig{
		SecurityDescriptor: "O:" + user.User.Sid.String() + "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12019b;;;IU)",
		InputBufferSize:    65536, OutputBufferSize: 65536,
	})
	if err != nil {
		bridge.startErr = err
		slog.Warn("browser bridge pipe unavailable", "error", err)
		return bridge
	}
	go func() {
		stop := context.AfterFunc(ctx, func() { listener.Close() })
		defer stop()
		defer listener.Close()
		for ctx.Err() == nil {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("browser bridge stopped listening", "error", err)
				}
				return
			}
			go bridge.accept(ctx, conn)
		}
	}()
	return bridge
}

func (bridge *browserBridge) accept(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	handle, ok := conn.(interface{ Fd() uintptr })
	var pid, session uint32
	if !ok || windows.GetNamedPipeClientProcessId(windows.Handle(handle.Fd()), &pid) != nil ||
		windows.ProcessIdToSessionId(pid, &session) != nil {
		return
	}
	path, sid, elevated, err := browserProcessIdentity(pid)
	if err != nil || !strings.EqualFold(filepath.Clean(path), filepath.Clean(bridge.core)) {
		return
	}
	conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	raw, err := readBrowserFrame(conn, 4096)
	if err != nil {
		return
	}
	var hello browserResponse
	if json.Unmarshal(raw, &hello) != nil || !hello.Status {
		return
	}
	if hello.Code == "Browser.cleanup" && elevated {
		conn.SetReadDeadline(time.Time{})
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := bridge.cleanup(ctx)
		reply := browserResponse{Code: hello.Code, Status: err == nil}
		if err != nil {
			reply.Error = err.Error()
		}
		data, _ := json.Marshal(reply)
		conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = writeBrowserFrame(conn, data, 4096)
		return
	}

	var data struct {
		Extension string `json:"extensionId"`
		Browser   string `json:"browser"`
	}
	if hello.Code != "Browser.connected" || hello.ID != "" || session == 0 ||
		json.Unmarshal(hello.Data, &data) != nil || data.Extension != browserExtensionID ||
		!nativeHostParent(pid, session, sid, data.Browser) {
		return
	}
	key := browserSessionKey{browserDesktop{session, sid}, data.Browser}
	peer := &browserPeer{conn: conn, done: make(chan struct{}), waiting: make(map[string]chan browserResponse)}
	bridge.mu.Lock()
	bridge.sessions[key] = append(bridge.sessions[key], peer)
	bridge.notify()
	bridge.mu.Unlock()
	defer func() {
		bridge.mu.Lock()
		peers := bridge.sessions[key]
		for i, item := range peers {
			if item == peer {
				peers = append(peers[:i], peers[i+1:]...)
				break
			}
		}
		if len(peers) == 0 {
			delete(bridge.sessions, key)
		} else {
			bridge.sessions[key] = peers
		}
		bridge.notify()
		bridge.mu.Unlock()
		close(peer.done)
	}()
	_ = peer.send(browserRequest{Code: "Browser.ping"})
	peer.read()
}

func browserProcessIdentity(pid uint32) (path, sid string, elevated bool, err error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", "", false, err
	}
	defer windows.CloseHandle(process)
	buffer := make([]uint16, 32768)
	length := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &length); err != nil {
		return "", "", false, err
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return "", "", false, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", "", false, err
	}
	return windows.UTF16ToString(buffer[:length]), user.User.Sid.String(), token.IsElevated(), nil
}

func nativeHostParent(pid, session uint32, sid, browser string) bool {
	exe := "chrome.exe"
	switch browser {
	case "edge":
		exe = "msedge.exe"
	case "chrome":
	default:
		return false
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	parents := make(map[uint32]uint32)
	for err := windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		parents[entry.ProcessID] = entry.ParentProcessID
	}
	// Some Chromium versions launch stdio hosts through the Windows command interpreter.
	for depth := 0; depth < 2; depth++ {
		pid = parents[pid]
		var parentSession uint32
		if pid == 0 || windows.ProcessIdToSessionId(pid, &parentSession) != nil || parentSession != session {
			return false
		}
		path, parentSID, _, err := browserProcessIdentity(pid)
		if err != nil || sid != parentSID {
			return false
		}
		name := filepath.Base(path)
		if strings.EqualFold(name, exe) {
			return true
		}
		if !strings.EqualFold(name, "cmd.exe") {
			return false
		}
	}
	return false
}

func browserDesktopNow() (browserDesktop, error) {
	session := currentProcessSessionID()
	token := windows.GetCurrentProcessToken()
	if session == 0 {
		var ok bool
		session, token, ok = interactiveSession()
		if !ok {
			return browserDesktop{}, errors.New("没有可用的 Windows 桌面会话")
		}
		defer token.Close()
	}
	user, err := token.GetTokenUser()
	if err != nil {
		return browserDesktop{}, err
	}
	return browserDesktop{session, user.User.Sid.String()}, nil
}

func (bridge *browserBridge) notify() {
	close(bridge.change)
	bridge.change = make(chan struct{})
}

func (bridge *browserBridge) peer(key browserSessionKey) (*browserPeer, <-chan struct{}) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	peers := bridge.sessions[key]
	if len(peers) == 0 {
		return nil, bridge.change
	}
	return peers[len(peers)-1], bridge.change
}

func (bridge *browserBridge) connectedPeers(key browserSessionKey) []*browserPeer {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return append([]*browserPeer(nil), bridge.sessions[key]...)
}

func (bridge *browserBridge) waitConnected(ctx context.Context, key browserSessionKey) (*browserPeer, error) {
	for {
		peer, changed := bridge.peer(key)
		if peer != nil {
			return peer, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("等待 %s 扩展连接失败: %w", key.browser, ctx.Err())
		case <-changed:
		}
	}
}

func (peer *browserPeer) read() {
	for {
		peer.conn.SetReadDeadline(time.Now().Add(browserHeartbeatWait))
		raw, err := readBrowserFrame(peer.conn, browserMaxResponse)
		if err != nil {
			return
		}
		var reply browserResponse
		if json.Unmarshal(raw, &reply) != nil {
			return
		}
		if reply.ID == "" && reply.Code == "Browser.ping" {
			if err := peer.send(browserRequest{Code: "Browser.ping"}); err != nil {
				return
			}
			continue
		}
		peer.mu.Lock()
		wait := peer.waiting[reply.ID]
		if wait != nil {
			select {
			case wait <- reply:
			default:
			}
		}
		peer.mu.Unlock()
	}
}

func (peer *browserPeer) send(request browserRequest) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	peer.writeMu.Lock()
	defer peer.writeMu.Unlock()
	peer.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return writeBrowserFrame(peer.conn, data, browserMaxRequest)
}

func (peer *browserPeer) call(ctx context.Context, request browserRequest) (browserResponse, error) {
	if err := ctx.Err(); err != nil {
		return browserResponse{}, err
	}
	id, err := proto.NewID()
	if err != nil {
		return browserResponse{}, err
	}
	request.ID = id
	wait := make(chan browserResponse, 1)
	peer.mu.Lock()
	peer.waiting[id] = wait
	peer.mu.Unlock()
	defer func() {
		peer.mu.Lock()
		delete(peer.waiting, id)
		peer.mu.Unlock()
	}()
	if err := peer.send(request); err != nil {
		return browserResponse{}, err
	}
	select {
	case <-ctx.Done():
		_ = peer.send(browserRequest{ID: id, Code: "Browser.cancel"})
		return browserResponse{}, ctx.Err()
	case <-peer.done:
		select {
		case reply := <-wait:
			return checkedBrowserReply(request.Code, reply)
		default:
			return browserResponse{}, errors.New("浏览器扩展连接已断开")
		}
	case reply := <-wait:
		return checkedBrowserReply(request.Code, reply)
	}
}

func checkedBrowserReply(code string, reply browserResponse) (browserResponse, error) {
	if reply.Code != code {
		return browserResponse{}, errors.New("浏览器返回了不匹配的操作结果")
	}
	if !reply.Status {
		if reply.Error == "" {
			reply.Error = "浏览器操作失败"
		}
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}
