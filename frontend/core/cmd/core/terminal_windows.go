package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"

	"ghosthand/proto"
)

var createPseudoConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole")

type windowsTerminal struct {
	consoleMu sync.Mutex
	console   windows.Handle
	job       windows.Handle
	process   windows.Handle
	input     *os.File
	output    *os.File
}

func terminalCoord(cols, rows int) windows.Coord {
	cols, rows = proto.TerminalDimensions(cols, rows)
	return windows.Coord{X: int16(cols), Y: int16(rows)}
}

func startWindowsTerminal(ctx context.Context, request proto.TerminalFrame) (_ *windowsTerminal, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := createPseudoConsole.Find(); err != nil {
		return nil, errors.New("此 Windows 版本不支持 ConPTY，需要 Windows 10 1809 或更新版本。")
	}
	t := &windowsTerminal{}
	defer func() {
		if err == nil {
			return
		}
		if t.input != nil {
			_ = t.input.Close()
		}
		if t.output != nil {
			_ = t.output.Close()
		}
		if t.job != 0 {
			_ = windows.CloseHandle(t.job)
		}
		if t.console != 0 {
			windows.ClosePseudoConsole(t.console)
		}
	}()
	var inputRead, inputWrite, outputRead, outputWrite windows.Handle
	if err := windows.CreatePipe(&inputRead, &inputWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("创建终端输入管道：%w", err)
	}
	defer windows.CloseHandle(inputRead)
	t.input = os.NewFile(uintptr(inputWrite), "conpty-input")
	if err := windows.CreatePipe(&outputRead, &outputWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("创建终端输出管道：%w", err)
	}
	defer windows.CloseHandle(outputWrite)
	t.output = os.NewFile(uintptr(outputRead), "conpty-output")
	if err := windows.CreatePseudoConsole(terminalCoord(request.Cols, request.Rows), inputRead, outputWrite, 0, &t.console); err != nil {
		return nil, fmt.Errorf("创建 ConPTY：%w", err)
	}
	t.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("创建终端进程组：%w", err)
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(t.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, fmt.Errorf("设置终端进程组：%w", err)
	}
	if err := t.startShell(ctx, request.Shell); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *windowsTerminal) startShell(ctx context.Context, shell proto.TerminalShell) error {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	system32 := filepath.Join(root, "System32")
	exe := filepath.Join(system32, "cmd.exe")
	args := ""
	if shell == proto.TerminalShellPowerShell {
		exe = filepath.Join(system32, "WindowsPowerShell", "v1.0", "powershell.exe")
		args = " -NoLogo -ExecutionPolicy Bypass"
	}
	app, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	commandLine, err := windows.UTF16PtrFromString(`"` + exe + `"` + args)
	if err != nil {
		return err
	}
	cwd := os.Getenv("USERPROFILE")
	if isCurrentSystem() {
		if _, user, ok := interactiveSession(); ok {
			if dir, err := user.GetUserProfileDirectory(); err == nil && dir != "" {
				cwd = dir
			}
			user.Close()
		}
	}
	if cwd == "" {
		if dir, err := windows.GetCurrentProcessToken().GetUserProfileDirectory(); err == nil && dir != "" {
			cwd = dir
		}
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		cwd = system32
	}
	directory, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return err
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("创建 Shell 进程属性：%w", err)
	}
	defer attrs.Delete()
	// The pseudoconsole attribute takes the handle value itself.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(t.console), unsafe.Sizeof(t.console)); err != nil {
		return fmt.Errorf("关联 Shell 与 ConPTY：%w", err)
	}
	startup := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES
	var pi windows.ProcessInformation
	const flags = windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED
	if err := ctx.Err(); err != nil {
		return err
	}
	err = windows.CreateProcess(app, commandLine, nil, nil, false, flags, nil, directory, &startup.StartupInfo, &pi)
	if err != nil {
		return fmt.Errorf("启动 Shell：%w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	// Assign the suspended process before it can create children.
	if err := windows.AssignProcessToJobObject(t.job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = windows.CloseHandle(pi.Process)
		return fmt.Errorf("关联 Shell 与终端进程组：%w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = windows.CloseHandle(pi.Process)
		return err
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.CloseHandle(pi.Process)
		return fmt.Errorf("恢复 Shell 线程：%w", err)
	}
	t.process = pi.Process
	return nil
}

func runTerminal(ctx context.Context, request proto.TerminalFrame, commands <-chan terminalEvent, output func(string)) proto.TerminalFrame {
	t, err := startWindowsTerminal(ctx, request)
	if err != nil {
		return proto.TerminalFrame{Error: err.Error()}
	}
	defer windows.CloseHandle(t.process)
	defer t.input.Close()
	outputDone := make(chan error, 1)
	go func() {
		err := t.readOutput(output)
		_ = t.output.Close()
		outputDone <- err
	}()
	controlCtx, stopControls := context.WithCancel(ctx)
	controlDone := make(chan struct{})
	controlErrors := make(chan error, 1)
	go func() {
		defer close(controlDone)
		if err := t.control(controlCtx, commands); err != nil {
			controlErrors <- err
		}
	}()
	processDone := make(chan proto.TerminalFrame, 1)
	go func() { processDone <- t.wait() }()
	var result proto.TerminalFrame
	var processExited bool
	var outputClosed bool
	select {
	case result = <-processDone:
		processExited = true
	case err := <-outputDone:
		outputClosed = true
		// EOF and the process-exit signal can arrive in either order.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case result = <-processDone:
			processExited = true
		case <-ctx.Done():
		case <-timer.C:
			result.Error = "终端输出管道已断开。"
			if !errors.Is(err, io.EOF) && !errors.Is(err, windows.ERROR_BROKEN_PIPE) {
				result.Error = fmt.Sprintf("读取终端输出：%v", err)
			}
		}
		timer.Stop()
	case err := <-controlErrors:
		result.Error = err.Error()
	case <-ctx.Done():
	}
	stopControls()
	_ = windows.CloseHandle(t.job)

	// Close can produce a final VT frame on older Windows builds. Keep draining
	// output even after the browser disconnects, until ConPTY closes the pipe.
	t.consoleMu.Lock()
	windows.ClosePseudoConsole(t.console)
	t.consoleMu.Unlock()
	<-controlDone
	if !outputClosed {
		<-outputDone
	}
	if !processExited {
		<-processDone
	}
	return result
}

func (t *windowsTerminal) wait() proto.TerminalFrame {
	if _, err := windows.WaitForSingleObject(t.process, windows.INFINITE); err != nil {
		return proto.TerminalFrame{Error: fmt.Sprintf("等待 Shell 退出：%v", err)}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(t.process, &code); err != nil {
		return proto.TerminalFrame{Error: fmt.Sprintf("获取 Shell 退出码：%v", err)}
	}
	exitCode := int(code)
	return proto.TerminalFrame{ExitCode: &exitCode}
}

func (t *windowsTerminal) control(ctx context.Context, commands <-chan terminalEvent) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case cmd := <-commands:
			if ctx.Err() != nil {
				return nil
			}
			switch cmd.kind {
			case proto.TerminalInputMessage:
				if _, err := t.input.WriteString(cmd.frame.Data); err != nil {
					return fmt.Errorf("写入终端输入：%w", err)
				}
			case proto.TerminalResizeMessage:
				t.consoleMu.Lock()
				var err error
				if ctx.Err() == nil {
					err = windows.ResizePseudoConsole(t.console, terminalCoord(cmd.frame.Cols, cmd.frame.Rows))
				}
				t.consoleMu.Unlock()
				if err != nil {
					return fmt.Errorf("调整终端尺寸：%w", err)
				}
			}
		}
	}
}

func (t *windowsTerminal) readOutput(emit func(string)) error {
	var buffer [proto.TerminalChunkSize]byte
	var pending int
	for {
		n, err := t.output.Read(buffer[pending:])
		total := pending + n
		end := total
		if err == nil {
			for start := total - 1; start >= max(0, total-utf8.UTFMax+1); start-- {
				if utf8.RuneStart(buffer[start]) {
					if !utf8.FullRune(buffer[start:total]) {
						end = start
					}
					break
				}
			}
		}
		if end != 0 {
			emit(strings.ToValidUTF8(string(buffer[:end]), "?"))
		}
		if err != nil {
			return err
		}
		pending = copy(buffer[:], buffer[end:total])
	}
}
