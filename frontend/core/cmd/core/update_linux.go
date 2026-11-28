package main

import (
	_ "embed"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed update_linux.sh
var linuxCoreUpdateScript string

func supportsCoreUpdate(core string) bool {
	if os.Geteuid() != 0 || core != "/opt/ghosthand/ghosthand-core" || !regularCoreFile(core) {
		return false
	}
	_, _, err := linuxCoreServiceManager()
	return err == nil
}

func quietCoreUpdateCheck(*exec.Cmd) {}

func linuxCoreServiceManager() (string, string, error) {
	if hasSystemd() {
		runner, err := exec.LookPath("systemd-run")
		return "systemd", runner, err
	}
	if _, err := os.Stat("/etc/init.d/ghosthand-core"); err == nil {
		_, err := exec.LookPath("rc-service")
		return "openrc", "", err
	}
	return "", "", errors.New("找不到 GhostHand systemd 或 OpenRC 服务管理器")
}

func coreUpdateWorker(core, stage string) (*exec.Cmd, error) {
	manager, runner, err := linuxCoreServiceManager()
	if err != nil {
		return nil, err
	}
	script := filepath.Join(stage, "apply.sh")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(linuxCoreUpdateScript, "\r\n", "\n")), 0600); err != nil {
		return nil, err
	}
	if manager == "systemd" {
		// A transient service survives stopping the Core service and its cgroup.
		return exec.Command(runner, "--quiet", "--wait", "--collect", "--property=RuntimeMaxSec=5min",
			"--", "/bin/sh", script, manager, core, stage), nil
	}
	cmd := exec.Command("/bin/sh", script, manager, core, stage)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}
