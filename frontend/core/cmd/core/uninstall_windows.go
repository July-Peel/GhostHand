package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

//go:embed uninstall_windows.ps1
var uninstallScript string

func (a *agent) cleanupBrowserExtension(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return a.browser.cleanup(ctx)
}

func startUninstallWorker(ctx context.Context) (uninstallWorker, error) {
	launcher, err := installedLauncher()
	if err != nil {
		return uninstallWorker{}, err
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return uninstallWorker{}, err
	}
	powershell := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", uninstallScript)
	cmd.Dir = system
	cmd.Env = append(os.Environ(), "GHOSTHAND_UNINSTALL_LAUNCHER="+launcher, "GHOSTHAND_UNINSTALL_DATA=")
	// The cleanup process has to outlive the job that the service uses to supervise Core.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB}
	input, err := cmd.StdinPipe()
	if err != nil {
		return uninstallWorker{}, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return uninstallWorker{}, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return uninstallWorker{}, err
	}

	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(output)
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "ready" {
			ready <- errors.New("Windows 卸载程序未通过安装路径检查")
		} else {
			ready <- nil
		}
		_, _ = io.Copy(io.Discard, reader)
		done <- cmd.Wait()
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case err = <-ready:
	case <-timer.C:
		err = errors.New("Windows 卸载程序启动超时")
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		_ = input.Close()
		_ = cmd.Process.Kill()
		return uninstallWorker{}, err
	}
	return uninstallWorker{input: input, done: done}, nil
}

func installedLauncher() (string, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService("GhostHandSvc")
	if err != nil {
		return "", err
	}
	defer service.Close()
	config, err := service.Config()
	if err != nil {
		return "", err
	}
	return parseLauncherBinaryPath(config.BinaryPathName)
}

func parseLauncherBinaryPath(binaryPathName string) (string, error) {
	args, err := windows.DecomposeCommandLine(binaryPathName)
	if err != nil {
		return "", fmt.Errorf("read GhostHandSvc executable path: %w", err)
	}
	if len(args) < 1 || !localUninstallPath(args[0]) ||
		!strings.EqualFold(filepath.Base(args[0]), "ghosthand-launcher.exe") {
		return "", errors.New("unexpected GhostHandSvc executable path")
	}
	return filepath.Clean(args[0]), nil
}

func localUninstallPath(path string) bool {
	volume := filepath.VolumeName(path)
	return filepath.IsAbs(path) && len(volume) == 2 && volume[1] == ':'
}
