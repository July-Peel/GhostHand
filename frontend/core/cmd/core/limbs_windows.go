//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"ghosthand/proto"
)

const (
	maxShellOutputBytes = 180 << 10
	maxFSReadBytes      = 180 << 10
)

func execShell(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	shell := args.Shell
	if shell == "" {
		shell = "powershell"
	}
	timeout := 60 * time.Second
	if args.TimeoutSec > 0 {
		timeout = time.Duration(args.TimeoutSec) * time.Second
	}
	cwd := expandWinPath(args.Cwd)
	if cwd != "" {
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("工作目录不存在: %s", cwd)
		}
	}
	exe, argv, rawCmdLine, err := buildShellInvocation(shell, args.Command)
	if err != nil {
		return nil, err
	}
	res := runWinProcess(ctx, timeout, exe, argv, rawCmdLine, cwd, args.RunAs)
	if res.launchErr != nil {
		return nil, res.launchErr
	}
	return marshalShellResult(res, shell, "")
}

func runScript(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	interp := args.Interpreter
	if interp == "" {
		interp = "auto"
	}
	scriptPath := expandWinPath(args.Path)
	hasContent := args.Content != nil && strings.TrimSpace(*args.Content) != ""
	cwd := expandWinPath(args.Cwd)
	if cwd != "" {
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("工作目录不存在: %s", cwd)
		}
	}
	timeout := 120 * time.Second
	if args.TimeoutSec > 0 {
		timeout = time.Duration(args.TimeoutSec) * time.Second
	}

	if scriptPath == "" {
		if !hasContent {
			return nil, errors.New("未提供脚本路径或脚本内容")
		}
		if interp == "auto" {
			interp = "powershell"
		}
		var exe string
		var argv []string
		var rawCmdLine string
		var err error
		switch interp {
		case "powershell", "pwsh":
			wrapped := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8;\n" + *args.Content
			if len(args.ArgsList) > 0 {
				var b strings.Builder
				b.WriteString("[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8; & {\n")
				b.WriteString(*args.Content)
				b.WriteString("\n}")
				for _, a := range args.ArgsList {
					b.WriteByte(' ')
					b.WriteByte('\'')
					b.WriteString(strings.ReplaceAll(a, "'", "''"))
					b.WriteByte('\'')
				}
				wrapped = b.String()
			}
			exe, argv, err = buildPSEncodedInvocation(interp, wrapped)
		case "cmd":
			exe, argv, rawCmdLine, err = buildShellInvocation("cmd", *args.Content)
		case "python":
			exe, err = resolveExe("python.exe", "python3.exe")
			if err != nil {
				return nil, errors.New("未找到 Python 解释器")
			}
			argv = append([]string{"-c", *args.Content}, args.ArgsList...)
		default:
			return nil, fmt.Errorf("不支持的脚本解释器: %s", interp)
		}
		if err != nil {
			return nil, err
		}
		res := runWinProcess(ctx, timeout, exe, argv, rawCmdLine, cwd, args.RunAs)
		if res.launchErr != nil {
			return nil, res.launchErr
		}
		return marshalShellResult(res, interp, "")
	}

	if hasContent {
		if err := os.MkdirAll(filepath.Dir(scriptPath), 0755); err != nil {
			return nil, fmt.Errorf("创建脚本目录失败: %w", err)
		}
		data := []byte(*args.Content)
		ext := strings.ToLower(filepath.Ext(scriptPath))
		if ext == ".ps1" && !bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
			data = append([]byte{0xEF, 0xBB, 0xBF}, data...)
		}
		if err := os.WriteFile(scriptPath, data, 0644); err != nil {
			return nil, fmt.Errorf("写入脚本失败: %w", err)
		}
	} else {
		if _, err := os.Stat(scriptPath); err != nil {
			return nil, fmt.Errorf("脚本文件不存在: %s", scriptPath)
		}
	}

	if interp == "auto" {
		switch strings.ToLower(filepath.Ext(scriptPath)) {
		case ".bat", ".cmd":
			interp = "cmd"
		case ".py":
			interp = "python"
		default:
			interp = "powershell"
		}
	}

	var exe string
	var argv []string
	var rawCmdLine string
	var err error
	switch interp {
	case "powershell", "pwsh":
		cmd := buildPSFileWrapper(scriptPath, args.ArgsList)
		exe, argv, rawCmdLine, err = buildShellInvocation(interp, cmd)
	case "cmd":
		var b strings.Builder
		b.WriteString(`call "`)
		b.WriteString(strings.ReplaceAll(scriptPath, `"`, `""`))
		b.WriteByte('"')
		for _, a := range args.ArgsList {
			b.WriteByte(' ')
			b.WriteString(syscall.EscapeArg(a))
		}
		exe, argv, rawCmdLine, err = buildShellInvocation("cmd", b.String())
	case "python":
		exe, err = resolveExe("python.exe", "python3.exe")
		if err != nil {
			return nil, errors.New("未找到 Python 解释器")
		}
		argv = append([]string{scriptPath}, args.ArgsList...)
	default:
		return nil, fmt.Errorf("不支持的脚本解释器: %s", interp)
	}
	if err != nil {
		return nil, err
	}
	res := runWinProcess(ctx, timeout, exe, argv, rawCmdLine, cwd, args.RunAs)
	if res.launchErr != nil {
		return nil, res.launchErr
	}
	return marshalShellResult(res, interp, scriptPath)
}

func buildPSEncodedInvocation(shell, script string) (string, []string, error) {
	var exe string
	var err error
	switch shell {
	case "powershell":
		exe, err = resolveExe("powershell.exe", filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"))
		if err != nil {
			return "", nil, errors.New("未找到 powershell.exe")
		}
	case "pwsh":
		exe, err = resolveExe("pwsh.exe")
		if err != nil {
			return "", nil, errors.New("未找到 pwsh.exe，请安装 PowerShell 7 或改用 powershell")
		}
	default:
		return "", nil, fmt.Errorf("不支持的 shell: %s", shell)
	}
	encoded := encodeUTF16LEBase64(script)
	argv := []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded}
	return exe, argv, nil
}

func encodeUTF16LEBase64(s string) string {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[i*2:], u)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func buildPSFileWrapper(scriptPath string, scriptArgs []string) string {
	quotePS := func(s string) string {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	var b strings.Builder
	b.WriteString("& ")
	b.WriteString(quotePS(scriptPath))
	for _, a := range scriptArgs {
		b.WriteByte(' ')
		b.WriteString(quotePS(a))
	}
	b.WriteString("; if ($null -ne $LASTEXITCODE) { exit $LASTEXITCODE }")
	return b.String()
}

func scriptExtension(interp string) string {
	switch interp {
	case "cmd":
		return ".bat"
	case "python":
		return ".py"
	default:
		return ".ps1"
	}
}

func buildShellInvocation(shell, command string) (exe string, argv []string, rawCmdLine string, err error) {
	switch shell {
	case "powershell":
		exe, err = resolveExe("powershell.exe", filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"))
		if err != nil {
			return "", nil, "", errors.New("未找到 powershell.exe")
		}
		wrapped := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8; " + command
		argv = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", wrapped}
	case "pwsh":
		exe, err = resolveExe("pwsh.exe")
		if err != nil {
			return "", nil, "", errors.New("未找到 pwsh.exe，请安装 PowerShell 7 或改用 powershell")
		}
		wrapped := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8; " + command
		argv = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", wrapped}
	case "cmd":
		comspec := os.Getenv("ComSpec")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		exe, err = resolveExe(comspec, "cmd.exe")
		if err != nil {
			return "", nil, "", errors.New("未找到 cmd.exe")
		}
		rawCmdLine = fmt.Sprintf(`"%s" /d /s /c "chcp 65001 >nul & %s"`, exe, command)
	default:
		return "", nil, "", fmt.Errorf("不支持的 shell: %s", shell)
	}
	return exe, argv, rawCmdLine, nil
}

func resolveExe(candidates ...string) (string, error) {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}
	return "", errors.New("executable not found")
}

type winProcResult struct {
	exitCode  int
	stdout    string
	stderr    string
	duration  int64
	truncated bool
	timedOut  bool
	runAs     string
	launchErr error
}

func runWinProcess(ctx context.Context, timeout time.Duration, exe string, argv []string, rawCmdLine, cwd, requestedRunAs string) winProcResult {
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if requestedRunAs != "system" && isCurrentSystem() {
		if _, userToken, ok := interactiveSession(); ok {
			elevated, okElev := elevateUserToken(userToken, false)
			userToken.Close()
			if okElev {
				res, tried := runWithToken(runCtx, exe, argv, rawCmdLine, cwd, elevated)
				elevated.Close()
				if tried {
					res.duration = time.Since(started).Milliseconds()
					res.runAs = "admin"
					return res
				}
			}
		}
	}

	cmd := exec.Command(exe, argv...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	attr := &syscall.SysProcAttr{HideWindow: true}
	if rawCmdLine != "" {
		attr.CmdLine = rawCmdLine
	}
	cmd.SysProcAttr = attr
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")

	var stdoutBuf, stderrBuf cappedBuffer
	stdoutBuf.limit = maxShellOutputBytes
	stderrBuf.limit = maxShellOutputBytes / 2
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := runCtx.Err(); err != nil {
		return winProcResult{launchErr: err}
	}
	if err := cmd.Start(); err != nil {
		return winProcResult{launchErr: fmt.Errorf("启动进程失败: %w", err)}
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	var timedOut bool
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		killProcessTree(uint32(cmd.Process.Pid))
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
		}
	}

	exitCode := exitCodeFromError(waitErr)
	stdout, stdoutTruncated := stdoutBuf.output()
	stderr, stderrTruncated := stderrBuf.output()
	return winProcResult{
		exitCode:  exitCode,
		stdout:    stdout,
		stderr:    stderr,
		duration:  time.Since(started).Milliseconds(),
		truncated: stdoutTruncated || stderrTruncated,
		timedOut:  timedOut,
		runAs:     currentPrivilegeLabel(),
	}
}

func runWithToken(ctx context.Context, exe string, argv []string, rawCmdLine, cwd string, token windows.Token) (winProcResult, bool) {
	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, token, false); err != nil {
		return winProcResult{}, false
	}
	defer windows.DestroyEnvironmentBlock(envBlock)

	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var outR, outW, errR, errW windows.Handle
	if err := windows.CreatePipe(&outR, &outW, &sa, 0); err != nil {
		return winProcResult{}, false
	}
	defer windows.CloseHandle(outR)
	if err := windows.CreatePipe(&errR, &errW, &sa, 0); err != nil {
		windows.CloseHandle(outW)
		return winProcResult{}, false
	}
	defer windows.CloseHandle(errR)

	_ = windows.SetHandleInformation(outR, windows.HANDLE_FLAG_INHERIT, 0)
	_ = windows.SetHandleInformation(errR, windows.HANDLE_FLAG_INHERIT, 0)

	cmdLine := rawCmdLine
	if cmdLine == "" {
		cmdLine = windows.ComposeCommandLine(append([]string{exe}, argv...))
	}
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		windows.CloseHandle(outW)
		windows.CloseHandle(errW)
		return winProcResult{}, false
	}
	cmdPtr, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		windows.CloseHandle(outW)
		windows.CloseHandle(errW)
		return winProcResult{}, false
	}
	var cwdPtr *uint16
	if cwd != "" {
		cwdPtr, _ = windows.UTF16PtrFromString(cwd)
	}

	si := windows.StartupInfo{
		Cb:         uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags:      windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW,
		ShowWindow: windows.SW_HIDE,
		StdOutput:  outW,
		StdErr:     errW,
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_NO_WINDOW | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_BREAKAWAY_FROM_JOB)
	if err := ctx.Err(); err != nil {
		windows.CloseHandle(outW)
		windows.CloseHandle(errW)
		return winProcResult{launchErr: err}, true
	}
	err = windows.CreateProcessAsUser(token, exePtr, cmdPtr, nil, nil, true, flags, envBlock, cwdPtr, &si, &pi)
	windows.CloseHandle(outW)
	windows.CloseHandle(errW)
	if err != nil {
		return winProcResult{}, false
	}
	windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)

	var stdoutBuf, stderrBuf cappedBuffer
	stdoutBuf.limit = maxShellOutputBytes
	stderrBuf.limit = maxShellOutputBytes / 2

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		readHandleInto(outR, &stdoutBuf)
	}()
	go func() {
		defer readers.Done()
		readHandleInto(errR, &stderrBuf)
	}()

	waitCh := make(chan struct{})
	go func() {
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		close(waitCh)
	}()

	var timedOut bool
	select {
	case <-waitCh:
	case <-ctx.Done():
		timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		killProcessTree(pi.ProcessId)
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
			_ = windows.TerminateProcess(pi.Process, 1)
		}
	}

	readersDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readersDone)
	}()
	select {
	case <-readersDone:
	case <-time.After(2 * time.Second):
	}

	var exitCode uint32
	_ = windows.GetExitCodeProcess(pi.Process, &exitCode)
	stdout, stdoutTruncated := stdoutBuf.output()
	stderr, stderrTruncated := stderrBuf.output()
	return winProcResult{
		exitCode:  int(exitCode),
		stdout:    stdout,
		stderr:    stderr,
		truncated: stdoutTruncated || stderrTruncated,
		timedOut:  timedOut,
	}, true
}

func readHandleInto(h windows.Handle, dst *cappedBuffer) {
	buf := make([]byte, 4096)
	for {
		var n uint32
		err := windows.ReadFile(h, buf, &n, nil)
		if n > 0 {
			_, _ = dst.Write(buf[:n])
		}
		if err != nil || n == 0 {
			return
		}
	}
}

func killProcessTree(pid uint32) {
	if pid == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill.exe", "/F", "/T", "/PID", strconv.FormatUint(uint64(pid), 10))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rem := b.limit - b.buf.Len()
	if rem <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > rem {
		b.buf.Write(p[:rem])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *cappedBuffer) output() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(b.buf.String(), "\uFFFD"), b.truncated
}

func marshalShellResult(res winProcResult, shell, scriptPath string) (json.RawMessage, error) {
	payload := struct {
		ScriptPath string `json:"script_path,omitempty"`
		ExitCode   int    `json:"exit_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		DurationMS int64  `json:"duration_ms"`
		Truncated  bool   `json:"truncated"`
		TimedOut   bool   `json:"timed_out,omitempty"`
		Shell      string `json:"shell"`
		RunAs      string `json:"run_as"`
	}{
		ScriptPath: scriptPath,
		ExitCode:   res.exitCode,
		Stdout:     res.stdout,
		Stderr:     res.stderr,
		DurationMS: res.duration,
		Truncated:  res.truncated,
		TimedOut:   res.timedOut,
		Shell:      shell,
		RunAs:      res.runAs,
	}
	if res.timedOut && payload.Stderr == "" {
		payload.Stderr = "命令执行超时已被终止"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(data) > proto.MaxToolDataSize-1024 {
		excess := len(data) - (proto.MaxToolDataSize - 2048)
		if len(payload.Stdout) > excess {
			payload.Stdout = trimUTF8Bytes(payload.Stdout, len(payload.Stdout)-excess)
		} else {
			payload.Stdout = ""
		}
		payload.Truncated = true
		data, err = json.Marshal(payload)
	}
	return data, err
}

func trimUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	b := []byte(s)[:maxBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func isCurrentSystem() bool {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	return err == nil && user.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

func currentPrivilegeLabel() string {
	token := windows.GetCurrentProcessToken()
	if user, err := token.GetTokenUser(); err == nil && user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return "system"
	}
	if token.IsElevated() {
		return "admin"
	}
	return "standard"
}

func manageProcess(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	switch args.Action {
	case "kill", "kill_tree":
		var targets []uint32
		if args.PID > 0 {
			targets = append(targets, uint32(args.PID))
		} else {
			pids, err := findPIDsByName(args.Name)
			if err != nil {
				return nil, err
			}
			targets = pids
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("未找到目标进程: %s", args.Name)
		}
		for _, pid := range targets {
			if args.Action == "kill_tree" {
				killProcessTree(pid)
				continue
			}
			h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
			if err != nil {
				return nil, fmt.Errorf("打开进程 %d 失败: %w", pid, err)
			}
			err = windows.TerminateProcess(h, 1)
			windows.CloseHandle(h)
			if err != nil {
				return nil, fmt.Errorf("结束进程 %d 失败: %w", pid, err)
			}
		}
		return json.Marshal(map[string]any{
			"status": "ok", "action": args.Action, "pids": targets,
			"detail": fmt.Sprintf("已终止 %d 个进程", len(targets)),
		})
	case "service_status", "service_start", "service_stop", "service_restart":
		m, err := mgr.Connect()
		if err != nil {
			return nil, fmt.Errorf("连接服务管理器失败: %w", err)
		}
		defer m.Disconnect()
		s, err := m.OpenService(args.Name)
		if err != nil {
			return nil, fmt.Errorf("打开服务 %s 失败: %w", args.Name, err)
		}
		defer s.Close()

		switch args.Action {
		case "service_start":
			if err := s.Start(); err != nil {
				return nil, fmt.Errorf("启动服务失败: %w", err)
			}
		case "service_stop":
			if err := stopServiceAndWait(ctx, s); err != nil {
				return nil, err
			}
		case "service_restart":
			st, err := s.Query()
			if err == nil && st.State != svc.Stopped {
				if err := stopServiceAndWait(ctx, s); err != nil {
					return nil, err
				}
			}
			if err := s.Start(); err != nil {
				return nil, fmt.Errorf("重启服务时启动失败: %w", err)
			}
		}
		st, err := s.Query()
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"status": "ok", "action": args.Action, "service": args.Name,
			"state": svcStateName(st.State), "pid": st.ProcessId,
		})
	default:
		return nil, errors.New("E_CAP_UNSUPPORTED")
	}
}

func stopServiceAndWait(ctx context.Context, s *mgr.Service) error {
	st, err := s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("停止服务失败: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return errors.New("等待服务停止超时")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		st, err = s.Query()
		if err != nil {
			return err
		}
	}
	return nil
}

func svcStateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start_pending"
	case svc.StopPending:
		return "stop_pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue_pending"
	case svc.PausePending:
		return "pause_pending"
	case svc.Paused:
		return "paused"
	default:
		return "unknown"
	}
}

func findPIDsByName(name string) ([]uint32, error) {
	needle := strings.ToLower(strings.TrimSpace(name))
	if needle == "" {
		return nil, nil
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	var pids []uint32
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		exeName := strings.ToLower(windows.UTF16ToString(entry.ExeFile[:]))
		if exeName == needle || strings.TrimSuffix(exeName, ".exe") == strings.TrimSuffix(needle, ".exe") {
			pids = append(pids, entry.ProcessID)
		}
	}
	return pids, nil
}

func listFS(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	root := expandWinPath(args.Path)
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}
	pattern := strings.ToLower(strings.TrimSpace(args.Pattern))
	type entry struct {
		Name    string `json:"name"`
		Path    string `json:"path,omitempty"`
		IsDir   bool   `json:"is_dir"`
		Size    int64  `json:"size"`
		ModTime string `json:"mod_time"`
	}
	var entries []entry
	truncated := false

	if !args.Recursive {
		items, err := os.ReadDir(root)
		if err != nil {
			return nil, fmt.Errorf("读取目录失败: %w", err)
		}
		for _, item := range items {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if pattern != "" {
				matched, _ := filepath.Match(pattern, strings.ToLower(item.Name()))
				if !matched {
					continue
				}
			}
			if len(entries) >= limit {
				truncated = true
				break
			}
			info, err := item.Info()
			var size int64
			var modTime string
			if err == nil {
				size = info.Size()
				modTime = info.ModTime().Format(time.RFC3339)
			}
			entries = append(entries, entry{
				Name:    item.Name(),
				IsDir:   item.IsDir(),
				Size:    size,
				ModTime: modTime,
			})
		}
	} else {
		errStop := errors.New("limit reached")
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil || p == root {
				return nil
			}
			if pattern != "" {
				matched, _ := filepath.Match(pattern, strings.ToLower(d.Name()))
				if !matched {
					return nil
				}
			}
			if len(entries) >= limit {
				truncated = true
				return errStop
			}
			rel, _ := filepath.Rel(root, p)
			info, err := d.Info()
			var size int64
			var modTime string
			if err == nil {
				size = info.Size()
				modTime = info.ModTime().Format(time.RFC3339)
			}
			entries = append(entries, entry{
				Name:    rel,
				Path:    p,
				IsDir:   d.IsDir(),
				Size:    size,
				ModTime: modTime,
			})
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return nil, fmt.Errorf("遍历目录失败: %w", err)
		}
	}

	return json.Marshal(map[string]any{
		"path":      root,
		"entries":   entries,
		"count":     len(entries),
		"truncated": truncated,
	})
}

func readFS(_ context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	target := expandWinPath(args.Path)
	f, err := os.Open(target)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	offset := args.OffsetLine
	if offset <= 0 {
		offset = 1
	}
	maxLines := args.MaxLines
	if maxLines <= 0 {
		maxLines = 500
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	var out strings.Builder
	totalLines := 0
	linesReturned := 0
	truncated := false
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			totalLines++
			if totalLines >= offset && linesReturned < maxLines {
				if out.Len()+len(line) > maxFSReadBytes {
					truncated = true
				} else {
					out.WriteString(strings.ToValidUTF8(line, "\uFFFD"))
					linesReturned++
				}
			} else if totalLines >= offset+maxLines {
				truncated = true
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("读取文件失败: %w", readErr)
		}
	}

	return json.Marshal(map[string]any{
		"path":           target,
		"content":        out.String(),
		"offset_line":    offset,
		"lines_returned": linesReturned,
		"total_lines":    totalLines,
		"truncated":      truncated,
	})
}

func writeFS(_ context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	target := expandWinPath(args.Path)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return nil, fmt.Errorf("创建父目录失败: %w", err)
	}
	mode := args.Mode
	if mode == "" {
		mode = "overwrite"
	}
	flags := os.O_CREATE | os.O_WRONLY
	if mode == "append" {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(target, flags, 0644)
	if err != nil {
		return nil, fmt.Errorf("打开写入文件失败: %w", err)
	}
	defer f.Close()
	n, err := f.WriteString(*args.Content)
	if err != nil {
		return nil, fmt.Errorf("写入文件失败: %w", err)
	}
	return json.Marshal(map[string]any{
		"status":        "ok",
		"path":          target,
		"bytes_written": n,
		"mode":          mode,
	})
}

func corePowerScreen(_ context.Context, args proto.GUIArgs) (json.RawMessage, bool, error) {
	if args.Action != "keep_awake" {
		return nil, false, nil
	}
	dur := args.DurationSec
	if dur <= 0 {
		dur = 300
	}
	if dur > 86400 {
		dur = 86400
	}
	go func() {
		release := keepAwake()
		timer := time.NewTimer(time.Duration(dur) * time.Second)
		<-timer.C
		release()
	}()
	data, err := json.Marshal(map[string]any{"status": "ok", "action": "keep_awake", "duration_sec": dur})
	return data, true, err
}

func expandWinPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.ContainsRune(raw, '%') {
		if in, err := windows.UTF16PtrFromString(raw); err == nil {
			buf := make([]uint16, 4096)
			if isCurrentSystem() {
				if _, token, ok := interactiveSession(); ok {
					r1, _, _ := expandEnvStringsForUser.Call(uintptr(token), uintptr(unsafe.Pointer(in)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
					token.Close()
					if r1 != 0 {
						return windows.UTF16ToString(buf)
					}
				}
			}
			r1, _, _ := expandEnvStrings.Call(uintptr(unsafe.Pointer(in)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			if r1 > 0 && int(r1) <= len(buf) {
				return windows.UTF16ToString(buf)
			}
		}
	}
	return raw
}
