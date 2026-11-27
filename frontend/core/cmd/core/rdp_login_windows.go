package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/pe"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"ghosthand/proto"
)

const (
	maxFreeRDPArchiveSize   = 64 << 20
	maxFreeRDPExtractedSize = 128 << 20
	maxFreeRDPArchiveFiles  = 32
)

//go:embed rdp_preflight_windows.ps1
var rdpPreflightScript string

type rdpNetwork struct {
	Fingerprint string `json:"fingerprint"`
	InboundOpen *bool  `json:"inbound_open"`
}

func rdpNetworkPolicy(ctx context.Context, port int, probeFirewall bool) rdpNetwork {
	var result rdpNetwork
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return result
	}
	path := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	command := "$Port = " + strconv.Itoa(port) + "\n$ProbeFirewall = $" + strconv.FormatBool(probeFirewall) + "\n" + rdpPreflightScript
	ctx, cancel := context.WithTimeout(ctx, 9*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	// The script emits the certificate before the advisory firewall probe.
	data, _ := cmd.Output()
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&result) != nil {
		return rdpNetwork{}
	}
	if hash, err := hex.DecodeString(result.Fingerprint); err != nil || len(hash) != sha256.Size {
		result.Fingerprint = ""
	}
	if probeFirewall {
		var firewall rdpNetwork
		if decoder.Decode(&firewall) == nil {
			result.InboundOpen = firewall.InboundOpen
		}
	}
	return result
}

func freeRDPDirs() []string {
	var dirs []string
	if dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0); err == nil && dir != "" {
		dirs = append(dirs, filepath.Join(dir, "GhostHand", "FreeRDP"))
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		alt := filepath.Join(filepath.Dir(exe), "FreeRDP")
		if len(dirs) == 0 || !strings.EqualFold(dirs[0], alt) {
			dirs = append(dirs, alt)
		}
	}
	return dirs
}

func freeRDPClient() string {
	for _, dir := range freeRDPDirs() {
		client := filepath.Join(dir, "wfreerdp.exe")
		if info, err := os.Lstat(client); err == nil && info.Mode().IsRegular() && ordinaryFreeRDPPath(dir) && ordinaryFreeRDPPath(client) {
			return client
		}
	}
	return ""
}

func ordinaryFreeRDPPath(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attr, err := windows.GetFileAttributes(p)
	return err == nil && attr&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func freeRDPInstallRoot(corePath string) (string, error) {
	var candidates []string
	if dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0); err == nil && dir != "" {
		if ordinaryFreeRDPPath(dir) {
			candidates = append(candidates, filepath.Join(dir, "GhostHand"))
		}
	}
	if corePath != "" {
		alt := filepath.Dir(corePath)
		if len(candidates) == 0 || !strings.EqualFold(candidates[0], alt) {
			candidates = append(candidates, alt)
		}
	}
	for _, root := range candidates {
		if err := os.MkdirAll(root, 0755); err != nil {
			continue
		}
		if ordinaryFreeRDPPath(root) {
			return root, nil
		}
	}
	return "", errors.New("no writable GhostHand installation directory for FreeRDP")
}

func (a *agent) ensureFreeRDPClient(ctx context.Context) string {
	if client := freeRDPClient(); client != "" {
		return client
	}
	cfg := a.cfg.Load()
	if cfg == nil || cfg.Server == "" || cfg.DeviceID == "" || cfg.Token == "" {
		return ""
	}
	root, err := freeRDPInstallRoot(a.corePath)
	if err != nil {
		slog.Warn("prepare FreeRDP directory failed", "error", err)
		return ""
	}
	reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cfg.Server+"/api/agent/components/freerdp", nil)
	if err != nil {
		return ""
	}
	authorized(req, *cfg)
	resp, err := a.client.Do(req)
	if err != nil {
		slog.Warn("download FreeRDP archive failed", "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("controller returned non-OK for FreeRDP archive", "status", resp.StatusCode)
		return ""
	}
	expectedHash := strings.ToLower(strings.TrimSpace(resp.Header.Get("X-GhostHand-SHA256")))
	if raw, err := hex.DecodeString(expectedHash); err != nil || len(raw) != sha256.Size {
		slog.Warn("missing or invalid FreeRDP archive checksum header")
		return ""
	}
	archiveFile, err := os.CreateTemp(root, ".freerdp-download-*.zip")
	if err != nil {
		slog.Warn("create FreeRDP temporary archive failed", "error", err)
		return ""
	}
	archivePath := archiveFile.Name()
	defer os.Remove(archivePath)
	defer archiveFile.Close()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(archiveFile, hash), io.LimitReader(resp.Body, maxFreeRDPArchiveSize+1))
	if err != nil || size == 0 || size > maxFreeRDPArchiveSize {
		slog.Warn("read FreeRDP archive failed", "size", size, "error", err)
		return ""
	}
	if actualHash := hex.EncodeToString(hash.Sum(nil)); actualHash != expectedHash {
		slog.Warn("FreeRDP archive checksum mismatch", "expected", expectedHash, "actual", actualHash)
		return ""
	}
	stageDir, err := os.MkdirTemp(root, ".freerdp-stage-*")
	if err != nil {
		slog.Warn("create FreeRDP staging directory failed", "error", err)
		return ""
	}
	defer os.RemoveAll(stageDir)
	if err := extractFreeRDPArchive(archiveFile, size, stageDir); err != nil {
		slog.Warn("extract FreeRDP archive failed", "error", err)
		return ""
	}
	_ = archiveFile.Close()
	targetDir := filepath.Join(root, "FreeRDP")
	if _, err := os.Lstat(targetDir); err == nil {
		if !ordinaryFreeRDPPath(targetDir) {
			slog.Warn("refusing to replace non-ordinary FreeRDP directory", "path", targetDir)
			return ""
		}
		if err := os.RemoveAll(targetDir); err != nil {
			slog.Warn("remove existing FreeRDP directory failed", "error", err)
			return ""
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err := os.Rename(stageDir, targetDir); err != nil {
		slog.Warn("activate FreeRDP directory failed", "error", err)
		return ""
	}
	client := freeRDPClient()
	if client != "" {
		slog.Info("FreeRDP client automatically deployed", "path", client)
	}
	return client
}

func extractFreeRDPArchive(archiveFile *os.File, size int64, stageDir string) error {
	reader, err := zip.NewReader(archiveFile, size)
	if err != nil {
		return err
	}
	if len(reader.File) == 0 || len(reader.File) > maxFreeRDPArchiveFiles {
		return errors.New("unexpected number of files in FreeRDP archive")
	}
	var totalBytes int64
	var hasClient bool
	for _, entry := range reader.File {
		rel := filepath.Clean(filepath.FromSlash(entry.Name))
		if rel == "." || !filepath.IsLocal(rel) {
			return errors.New("invalid path in FreeRDP archive")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(stageDir, rel), 0755); err != nil {
				return err
			}
			continue
		}
		if !entry.Mode().IsRegular() || entry.UncompressedSize64 > maxFreeRDPExtractedSize {
			return errors.New("invalid entry in FreeRDP archive")
		}
		if strings.EqualFold(rel, "wfreerdp.exe") {
			rel = "wfreerdp.exe"
		}
		dest := filepath.Join(stageDir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		src, err := entry.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0755)
		if err != nil {
			src.Close()
			return err
		}
		remaining := maxFreeRDPExtractedSize - totalBytes
		written, copyErr := io.Copy(out, io.LimitReader(src, remaining+1))
		src.Close()
		totalBytes += written
		if copyErr != nil || totalBytes > maxFreeRDPExtractedSize {
			out.Close()
			return errors.New("FreeRDP archive extraction exceeded size limit")
		}
		if rel == "wfreerdp.exe" {
			if !isFreeRDPAMD64Executable(out) {
				out.Close()
				return errors.New("wfreerdp.exe is not a valid Windows x64 executable")
			}
			hasClient = true
		}
		if err := errors.Join(out.Sync(), out.Close()); err != nil {
			return err
		}
	}
	if !hasClient {
		return errors.New("FreeRDP archive does not contain wfreerdp.exe")
	}
	return nil
}

func isFreeRDPAMD64Executable(file io.ReaderAt) bool {
	var dos [64]byte
	if _, err := file.ReadAt(dos[:], 0); err != nil || string(dos[:2]) != "MZ" {
		return false
	}
	offset := int64(binary.LittleEndian.Uint32(dos[60:]))
	var header [26]byte
	if _, err := file.ReadAt(header[:], offset); err != nil || string(header[:4]) != "PE\x00\x00" {
		return false
	}
	flags := binary.LittleEndian.Uint16(header[22:24])
	return binary.LittleEndian.Uint16(header[4:6]) == pe.IMAGE_FILE_MACHINE_AMD64 &&
		binary.LittleEndian.Uint16(header[24:26]) == 0x20b &&
		flags&pe.IMAGE_FILE_EXECUTABLE_IMAGE != 0 && flags&pe.IMAGE_FILE_DLL == 0
}

func isSDLFreeRDP(client string) bool {
	data, err := os.ReadFile(client)
	return err == nil && bytes.Contains(data, []byte("sdl-connection-dialog"))
}

func (a *agent) rdpCredentials(ctx context.Context, req proto.DesktopRequest) (proto.RDPCredential, proto.ErrorCode) {
	var credential proto.RDPCredential
	cfg := a.cfg.Load()
	if cfg == nil || !strings.HasPrefix(cfg.Server, "https://") || req.Ticket == "" {
		return credential, proto.ErrRDPConnection
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Server+"/api/agent/rdp/"+req.OperationID+"/credential", nil)
	if err != nil {
		return credential, proto.ErrRDPConnection
	}
	r.Header.Set("Authorization", "Bearer "+req.Ticket)
	r.Header.Set("X-GhostHand-Device", cfg.DeviceID)
	r.Header.Set("X-GhostHand-Token", cfg.Token)
	response, err := a.client.Do(r)
	if err != nil {
		return credential, proto.ErrRDPConnection
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusGone {
		return credential, proto.ErrRDPTicketExpired
	}
	if response.StatusCode != http.StatusOK {
		return credential, proto.ErrRDPConnection
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	defer clear(body)
	if err != nil || json.Unmarshal(body, &credential) != nil || credential.Username == "" || credential.Password == nil {
		clear(credential.Password)
		return proto.RDPCredential{}, proto.ErrRDPConnection
	}
	return credential, ""
}

func rdpLoginAndTransfer(ctx context.Context, client, fingerprint, account string, password []byte, port int,
	console proto.RDPConsoleSession, sid *windows.SID, singleSession bool, progress func(string)) (uint32, proto.ErrorCode) {
	destination := net.JoinHostPort(loopbackRDP, strconv.Itoa(port))
	clientCtx, cancelClient := context.WithCancel(ctx)
	defer cancelClient()
	args := []string{
		"/v:" + destination, "/u:" + account, "/sec:nla",
		"/cert:deny,fingerprint:sha256:" + fingerprint, "/proxy:ignore", "/log-level:ERROR",
		"/size:1024x768", "/bpp:16", "/audio-mode:2", "-clipboard", "-auto-reconnect",
	}
	if isSDLFreeRDP(client) {
		args = append(args, "-sdl-connection-dialog")
	}
	if len(password) == 0 {
		args = append(args, "/p")
	} else {
		args = append(args, "/from-stdin:force")
	}
	cmd := exec.CommandContext(clientCtx, client, args...)
	cmd.Dir = filepath.Dir(client)
	cmd.Env = append(os.Environ(), "SDL_VIDEODRIVER=dummy", "SDL_AUDIODRIVER=dummy")
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	var logs rdpClientOutput
	cmd.Stdout, cmd.Stderr = &logs, &logs
	var stdin io.WriteCloser
	if len(password) > 0 {
		var err error
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return 0, proto.ErrRDPClientUnavailable
		}
	}
	if err := cmd.Start(); err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		return 0, proto.ErrRDPClientUnavailable
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	defer func() {
		cancelClient()
		<-done
	}()
	if stdin != nil {
		_, writeErr := stdin.Write(password)
		if writeErr == nil {
			_, writeErr = io.WriteString(stdin, "\n")
		}
		clear(password)
		_ = stdin.Close()
		if writeErr != nil {
			return 0, proto.ErrRDPClientUnavailable
		}
	}
	login, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for login.Err() == nil {
		select {
		case <-done:
			return 0, logs.code()
		case <-login.Done():
			return 0, proto.ErrRDPTimeout
		case <-tick.C:
			session, code := activeRDPUser(console, sid, singleSession)
			if code != "" {
				return 0, code
			}
			if session == 0 {
				continue
			}
			// A reused session can be detached from the console before tscon.
			if current := rdpConsole(); current.ID != 0 && current.State != "none" {
				user, err := rdpSessionSID(current.ID)
				if err != nil {
					continue
				}
				if !user.Equals(sid) {
					return 0, proto.ErrRDPUserMismatch
				}
			}
			progress("transfer")
			if code := transferRDPConsole(ctx, session); code != "" {
				return 0, code
			}
			return session, ""
		}
	}
	return 0, proto.ErrRDPTimeout
}

func activeRDPUser(console proto.RDPConsoleSession, sid *windows.SID, singleSession bool) (uint32, proto.ErrorCode) {
	sessions, err := rdpSessions()
	if err != nil {
		return 0, proto.ErrRDPConnection
	}
	var found uint32
	for _, session := range sessions {
		id := session.SessionID
		if id == 0 || session.State != windows.WTSActive || rdpSessionProtocol(id) != 2 {
			continue
		}
		user, err := rdpSessionSID(id)
		if err != nil || !user.Equals(sid) {
			continue
		}
		if id == console.ID {
			return id, ""
		}
		if found != 0 {
			return 0, proto.ErrBusy
		}
		found = id
	}
	if found != 0 && console.State != "none" {
		if !singleSession {
			return 0, proto.ErrRDPAccessDenied
		}
		// An existing console user's session must be reused to preserve its windows.
		return 0, ""
	}
	return found, ""
}

func transferRDPConsole(ctx context.Context, session uint32) proto.ErrorCode {
	path := tsconPath()
	if path == "" {
		return proto.ErrRDPTsconFailed
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, strconv.FormatUint(uint64(session), 10), "/dest:console")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Run(); err != nil {
		return proto.ErrRDPTsconFailed
	}
	return ""
}

// FreeRDP output is reduced to known error codes in memory.
type rdpClientOutput struct {
	tail   string
	result proto.ErrorCode
}

func (out *rdpClientOutput) Write(data []byte) (int, error) {
	text := out.tail + string(data)
	out.tail = strings.Clone(text[max(0, len(text)-80):])
	switch {
	case strings.Contains(text, "ERRCONNECT_LOGON_FAILURE"), strings.Contains(text, "ERRCONNECT_WRONG_PASSWORD"),
		strings.Contains(text, "ERRCONNECT_AUTHENTICATION_FAILED"), strings.Contains(text, "STATUS_LOGON_FAILURE"):
		if out.result == "" {
			out.result = proto.ErrRDPAuthFailed
		}
	case strings.Contains(text, "ERRCONNECT_ACCOUNT_"), strings.Contains(text, "ERRCONNECT_PASSWORD_"),
		strings.Contains(text, "ERRCONNECT_CLIENT_REVOKED"):
		out.result = proto.ErrRDPAccountLocked
	case strings.Contains(text, "ERRCONNECT_LOGON_TYPE_NOT_GRANTED"), strings.Contains(text, "ERRCONNECT_ACCESS_DENIED"),
		strings.Contains(text, "ERRCONNECT_INSUFFICIENT_PRIVILEGES"):
		out.result = proto.ErrRDPAccessDenied
	case strings.Contains(text, "ERRCONNECT_TLS_CONNECT_FAILED"):
		out.result = proto.ErrRDPCertificate
	}
	return len(data), nil
}

func (out *rdpClientOutput) code() proto.ErrorCode {
	if out.result != "" {
		return out.result
	}
	return proto.ErrRDPConnection
}
