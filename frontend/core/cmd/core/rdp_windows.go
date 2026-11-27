package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"ghosthand/proto"
)

const terminalServerKey = `SYSTEM\CurrentControlSet\Control\Terminal Server`
const loopbackRDP = "127.0.0.1"

func (a *agent) rdpOperation(ctx context.Context, result proto.DesktopResult, request proto.DesktopRequest, progress func(string)) proto.DesktopResult {
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(request.ExpiresAt))
	defer cancel()
	result.Code = proto.ErrRDPTimeout
	if ctx.Err() != nil {
		return result
	}
	if !a.rdpMu.TryLock() {
		result.Code = proto.ErrBusy
		return result
	}
	defer a.rdpMu.Unlock()
	switch result.Kind {
	case proto.RDPPreflight:
		a.ensureFreeRDPClient(ctx)
		detail, _, _, code := rdpPreflight(ctx, request.Port, true)
		result.Code = code
		result.Detail, _ = json.Marshal(detail)
	case proto.RDPUnlock:
		release := keepAwake()
		defer release()
		detail, code := a.unlockConsole(ctx, request, progress)
		result.Code = code
		result.Detail, _ = json.Marshal(detail)
	case proto.RDPLock:
		result.Code = a.lockConsole(ctx, result.OperationID, request.SessionID)
	}
	result.OK = result.Code == ""
	return result
}

func rdpPreflight(ctx context.Context, port int, probeFirewall bool) (proto.RDPPreflightDetail, string, string, proto.ErrorCode) {
	detail := proto.RDPPreflightDetail{OSVersion: osVersion(), ConsoleSession: rdpConsole()}
	if edition, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		detail.Edition, _, _ = edition.GetStringValue("EditionID")
		edition.Close()
	}
	edition := strings.ToLower(detail.Edition)
	detail.Supported = edition != "" && !strings.HasPrefix(edition, "core") && !strings.HasPrefix(edition, "starter")
	detail.ServiceMode = currentProcessSessionID() == 0 && isCurrentSystem()
	if value, ok := rdpRegistryInt(terminalServerKey, "fDenyTSConnections"); ok {
		detail.Enabled = value == 0
	}
	nla, _ := rdpRegistryInt(terminalServerKey+`\WinStations\RDP-Tcp`, "UserAuthentication")
	detail.NLARequired = nla != 0
	detail.BlankPasswordRestricted = rdpBlankPasswordRestricted()
	detail.SingleSession = true
	if value, ok := rdpRegistryInt(terminalServerKey, "fSingleSessionPerUser"); ok {
		detail.SingleSession = value != 0
	}
	if policy, ok := rdpRegistryInt(`SOFTWARE\Policies\Microsoft\Windows NT\Terminal Services`, "fSingleSessionPerUser"); ok {
		detail.SingleSession = policy != 0
	}
	detail.TermServiceRunning = rdpServiceRunning()
	detail.SleepACMinutes, detail.SleepDCMinutes = sleepTimeouts()
	if port < 1 || port > 65535 {
		return detail, "", "", proto.ErrRDPPortClosed
	}
	ip := net.JoinHostPort(loopbackRDP, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: 1500 * time.Millisecond}
	if conn, err := dialer.DialContext(ctx, "tcp4", ip); err == nil {
		detail.PortReachable = true
		conn.Close()
	}
	program := freeRDPClient()
	detail.ClientAvailable = program != ""
	network := rdpNetworkPolicy(ctx, port, probeFirewall)
	detail.CertificateAvailable = network.Fingerprint != ""
	detail.FirewallInboundOpen = network.InboundOpen
	var code proto.ErrorCode
	switch {
	case !detail.Supported:
		code = proto.ErrRDPNotSupported
	case !detail.ServiceMode:
		code = proto.ErrRDPNotServiceMode
	case !detail.Enabled || !detail.TermServiceRunning:
		code = proto.ErrRDPDisabled
	case !detail.PortReachable:
		code = proto.ErrRDPPortClosed
	case !detail.ClientAvailable:
		code = proto.ErrRDPClientUnavailable
	case !detail.CertificateAvailable:
		code = proto.ErrRDPCertificate
	}
	return detail, program, network.Fingerprint, code
}

func rdpRegistryInt(path, name string) (uint64, bool) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	defer key.Close()
	value, _, err := key.GetIntegerValue(name)
	return value, err == nil
}

func rdpBlankPasswordRestricted() bool {
	value, ok := rdpRegistryInt(`SYSTEM\CurrentControlSet\Control\Lsa`, "LimitBlankPasswordUse")
	return !ok || value != 0
}

func rdpServiceRunning() bool {
	manager, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer manager.Disconnect()
	service, err := manager.OpenService("TermService")
	if err != nil {
		return false
	}
	defer service.Close()
	state, err := service.Query()
	return err == nil && state.State == svc.Running
}

func rdpSessionValue[T any](id uint32, class uint32, read func(uintptr, uint32) T) (T, bool) {
	var buffer uintptr
	var size uint32
	ok, _, _ := querySession.Call(0, uintptr(id), uintptr(class), uintptr(unsafe.Pointer(&buffer)), uintptr(unsafe.Pointer(&size)))
	if ok == 0 || buffer == 0 {
		var zero T
		return zero, false
	}
	defer windows.WTSFreeMemory(buffer)
	return read(buffer, size), true
}

func rdpSessionString(id, class uint32) (string, bool) {
	return rdpSessionValue(id, class, func(buffer uintptr, size uint32) string {
		return windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(buffer)), size/2))
	})
}

func rdpConsole() proto.RDPConsoleSession {
	id := windows.WTSGetActiveConsoleSessionId()
	if id == 0xffffffff || id == 0 {
		return proto.RDPConsoleSession{State: "unknown"}
	}
	username, queried := rdpSessionString(id, 5) // WTSUserName
	if !queried {
		return proto.RDPConsoleSession{ID: id, State: "unknown"}
	}
	if username == "" {
		return proto.RDPConsoleSession{ID: id, State: "none"}
	}
	if domain, _ := rdpSessionString(id, 7); domain != "" { // WTSDomainName
		username = domain + `\` + username
	}
	return proto.RDPConsoleSession{ID: id, State: desktopState(id), Username: username}
}

func rdpSessionSID(id uint32) (*windows.SID, error) {
	var token windows.Token
	if err := windows.WTSQueryUserToken(id, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

func rdpSessionProtocol(id uint32) uint16 {
	value, _ := rdpSessionValue(id, 16, func(buffer uintptr, size uint32) uint16 { // WTSClientProtocolType
		if size < 2 {
			return 0
		}
		return *(*uint16)(unsafe.Pointer(buffer))
	})
	return value
}

func rdpSessions() ([]windows.WTS_SESSION_INFO, error) {
	var info *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &info, &count); err != nil {
		return nil, err
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(info)))
	sessions := make([]windows.WTS_SESSION_INFO, 0, count)
	for _, entry := range unsafe.Slice(info, count) {
		sessions = append(sessions, windows.WTS_SESSION_INFO{SessionID: entry.SessionID, State: entry.State})
	}
	return sessions, nil
}

func (a *agent) consoleReady(id uint32) bool {
	if desktopState(id) != "unlocked" {
		return false
	}
	p := a.platform
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.desktop == "unlocked" && p.helper == "ready" && p.ipc != nil && p.ipc.session == id
}

func (a *agent) unlockConsole(ctx context.Context, req proto.DesktopRequest, progress func(string)) (detail proto.RDPUnlockDetail, code proto.ErrorCode) {
	start := time.Now()
	defer func() { detail.ElapsedMS = time.Since(start).Milliseconds() }()
	console := rdpConsole()
	if console.State == "unlocked" && a.consoleReady(console.ID) {
		detail.SessionID, detail.AlreadyUnlocked = console.ID, true
		return detail, ""
	}
	a.ensureFreeRDPClient(ctx)
	preflight, client, fingerprint, code := rdpPreflight(ctx, req.Port, false)
	if code != "" {
		return detail, code
	}
	credential, code := a.rdpCredentials(ctx, req)
	if code != "" {
		return detail, code
	}
	defer clear(credential.Password)
	account := credential.Username
	if !strings.ContainsAny(account, `\@`) {
		domain := credential.Domain
		if domain == "" {
			domain = "."
		}
		account = domain + `\` + account
	}
	lookup := account
	if strings.HasPrefix(account, `.\`) {
		computer, err := windows.ComputerName()
		if err != nil {
			return detail, proto.ErrRDPUserMismatch
		}
		lookup = computer + account[1:]
	}
	sid, accountDomain, accountType, err := windows.LookupSID("", lookup)
	if err != nil && len(credential.Password) > 0 && strings.Contains(account, "@") && !strings.Contains(account, `\`) {
		account = `MicrosoftAccount\` + account
		sid, _, _, err = windows.LookupSID("", account)
	}
	if err != nil {
		return detail, proto.ErrRDPUserMismatch
	}
	console = rdpConsole()
	if !consoleUserMatches(console, sid) {
		return detail, proto.ErrRDPUserMismatch
	}
	if len(credential.Password) == 0 {
		computer, err := windows.ComputerName()
		if err != nil || accountType != windows.SidTypeUser || !strings.EqualFold(computer, accountDomain) ||
			strings.Contains(account, "@") || rdpBlankPasswordRestricted() {
			return detail, proto.ErrRDPBlankPasswordRestricted
		}
	}
	before, err := rdpSessions()
	if err != nil {
		return detail, proto.ErrRDPConnection
	}
	for _, session := range before {
		if session.SessionID == 0 || session.SessionID == console.ID || session.State != windows.WTSActive {
			continue
		}
		// Leave existing active Remote Desktop sessions under their users' control.
		if other, err := rdpSessionSID(session.SessionID); err == nil && other != nil {
			return detail, proto.ErrBusy
		}
	}
	if console.State == "unlocked" && a.consoleReady(console.ID) {
		detail.SessionID, detail.AlreadyUnlocked = console.ID, true
		return detail, ""
	}
	progress("login")
	session, code := rdpLoginAndTransfer(ctx, client, fingerprint, account, credential.Password, req.Port, console, sid, preflight.SingleSession, progress)
	if code != "" {
		return detail, code
	}
	detail.SessionID = session
	progress("helper")
	signal(a.platform.sessionEvents)
	ready, done := context.WithTimeout(ctx, 15*time.Second)
	defer done()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for ready.Err() == nil {
		if windows.WTSGetActiveConsoleSessionId() == session && a.consoleReady(session) && consoleUserMatches(rdpConsole(), sid) {
			return detail, ""
		}
		select {
		case <-ready.Done():
		case <-tick.C:
			signal(a.platform.sessionEvents)
		}
	}
	return detail, proto.ErrRDPTimeout
}

func consoleUserMatches(console proto.RDPConsoleSession, sid *windows.SID) bool {
	if console.State == "none" {
		return true
	}
	if console.State == "unknown" {
		return false
	}
	user, err := rdpSessionSID(console.ID)
	return err == nil && user.Equals(sid)
}

func (a *agent) lockConsole(ctx context.Context, id string, session uint32) proto.ErrorCode {
	console := rdpConsole()
	if console.State == "unknown" {
		return proto.ErrHelperUnavailable
	}
	if session != 0 && session != console.ID {
		return proto.ErrRDPUserMismatch
	}
	if console.State != "unlocked" {
		return ""
	}
	_, err := a.platform.callHelperSession(ctx, console.ID, id, "desktop.lock", proto.TaskRef{TaskID: id})
	if err != nil && !errors.Is(err, context.Canceled) {
		return proto.ErrHelperUnavailable
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		if current := rdpConsole(); current.ID == console.ID && current.State == "locked" {
			signal(a.platform.sessionEvents)
			return ""
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	return proto.ErrRDPTimeout
}

func sleepTimeouts() (*int, *int) {
	power := windows.NewLazySystemDLL("powrprof.dll")
	active := power.NewProc("PowerGetActiveScheme")
	var scheme *windows.GUID
	code, _, _ := active.Call(0, uintptr(unsafe.Pointer(&scheme)))
	if code != 0 || scheme == nil {
		return nil, nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(scheme)))
	sleep, _ := windows.GUIDFromString("{238C9FA8-0AAD-41ED-83F4-97BE242C8F20}")
	idle, _ := windows.GUIDFromString("{29F6C1DB-86DA-48C5-9FDB-F2B67B1F44DA}")
	read := func(name string) *int {
		var seconds uint32
		status, _, _ := power.NewProc(name).Call(0, uintptr(unsafe.Pointer(scheme)),
			uintptr(unsafe.Pointer(&sleep)), uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&seconds)))
		if status != 0 {
			return nil
		}
		minutes := int((seconds + 59) / 60)
		return &minutes
	}
	return read("PowerReadACValueIndex"), read("PowerReadDCValueIndex")
}

func tsconPath() string {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "tscon.exe")
}
