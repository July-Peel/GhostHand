//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"ghosthand/proto"
)

type linuxTerminal struct {
	mu     sync.Mutex
	master *os.File
	cmd    *exec.Cmd
}

func openLinuxPTY(cols, rows int) (*os.File, *os.File, error) {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 /dev/ptmx 失败: %w", err)
	}
	master := os.NewFile(uintptr(masterFD), "/dev/ptmx")
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("解锁 PTY 失败: %w", err)
	}
	ptyNum, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("获取 PTY 编号失败: %w", err)
	}
	ws := &unix.Winsize{Row: uint16(rows), Col: uint16(cols)}
	_ = unix.IoctlSetWinsize(masterFD, unix.TIOCSWINSZ, ws)

	slavePath := fmt.Sprintf("/dev/pts/%d", ptyNum)
	slaveFD, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("打开 %s 失败: %w", slavePath, err)
	}
	slave := os.NewFile(uintptr(slaveFD), slavePath)
	return master, slave, nil
}

func resolveInteractiveShell() string {
	if envShell := strings.TrimSpace(os.Getenv("SHELL")); envShell != "" {
		if _, err := os.Stat(envShell); err == nil {
			return envShell
		}
	}
	for _, candidate := range []string{"/bin/bash", "/bin/ash", "/bin/sh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "/bin/sh"
}

func runTerminal(ctx context.Context, request proto.TerminalFrame, commands <-chan terminalEvent, output func(string)) proto.TerminalFrame {
	if ctx.Err() != nil {
		return proto.TerminalFrame{}
	}
	cols, rows := proto.TerminalDimensions(request.Cols, request.Rows)
	master, slave, err := openLinuxPTY(cols, rows)
	if err != nil {
		return proto.TerminalFrame{Error: err.Error()}
	}

	shellPath := resolveInteractiveShell()
	cmd := exec.Command(shellPath)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cmd.Dir = home
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return proto.TerminalFrame{Error: fmt.Sprintf("启动 Shell 失败: %v", err)}
	}
	// Close the slave descriptor in the parent so reading master receives EOF when the shell exits.
	_ = slave.Close()

	t := &linuxTerminal{master: master, cmd: cmd}
	outputDone := make(chan error, 1)
	go func() {
		outputDone <- t.readOutput(output)
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
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else {
				code = 1
			}
		}
		processDone <- proto.TerminalFrame{ExitCode: &code}
	}()

	var result proto.TerminalFrame
	var processExited bool
	var outputClosed bool
	select {
	case result = <-processDone:
		processExited = true
	case err := <-outputDone:
		outputClosed = true
		timer := time.NewTimer(150 * time.Millisecond)
		select {
		case result = <-processDone:
			processExited = true
		case <-ctx.Done():
		case <-timer.C:
			result.Error = "终端输出管道已断开。"
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EIO) {
				result.Error = fmt.Sprintf("读取终端输出: %v", err)
			}
		}
		timer.Stop()
	case err := <-controlErrors:
		result.Error = err.Error()
	case <-ctx.Done():
	}

	stopControls()
	if !processExited && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-processDone:
			processExited = true
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		timer.Stop()
	}
	t.mu.Lock()
	_ = master.Close()
	t.mu.Unlock()
	<-controlDone
	if !outputClosed {
		<-outputDone
	}
	if !processExited {
		<-processDone
	}
	return result
}

func (t *linuxTerminal) control(ctx context.Context, commands <-chan terminalEvent) error {
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
				t.mu.Lock()
				_, err := t.master.WriteString(cmd.frame.Data)
				t.mu.Unlock()
				if err != nil {
					return fmt.Errorf("写入终端输入: %w", err)
				}
			case proto.TerminalResizeMessage:
				cols, rows := proto.TerminalDimensions(cmd.frame.Cols, cmd.frame.Rows)
				t.mu.Lock()
				_ = unix.IoctlSetWinsize(int(t.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
					Row: uint16(rows),
					Col: uint16(cols),
				})
				t.mu.Unlock()
			}
		}
	}
}

func (t *linuxTerminal) readOutput(emit func(string)) error {
	var buffer [proto.TerminalChunkSize]byte
	var pending int
	for {
		n, err := t.master.Read(buffer[pending:])
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
