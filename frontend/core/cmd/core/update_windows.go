package main

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"ghosthand/launcher/state"
)

//go:embed update_windows.ps1
var windowsCoreUpdateScript string

func supportsCoreUpdate(core string) bool {
	if runtime.GOARCH != "amd64" || !regularCoreFile(core) {
		return false
	}
	launcher, err := installedLauncher()
	return err == nil && strings.EqualFold(core, filepath.Join(filepath.Dir(launcher), state.CoreName))
}

func quietCoreUpdateCheck(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

func coreUpdateWorker(core, stage string) (*exec.Cmd, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	powershell := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsCoreUpdateScript)
	cmd.Dir = system
	cmd.Env = append(os.Environ(), "GH_CORE_UPDATE_BIN="+core, "GH_CORE_UPDATE_STAGE="+stage)
	// The Launcher closes Core's job when its service stops.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB}
	return cmd, nil
}
