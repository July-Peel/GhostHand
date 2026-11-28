//go:build !windows && !linux

package main

import (
	"errors"
	"os/exec"
)

func supportsCoreUpdate(string) bool { return false }

func quietCoreUpdateCheck(*exec.Cmd) {}

func coreUpdateWorker(string, string) (*exec.Cmd, error) {
	return nil, errors.New("remote Core update is unavailable on this platform")
}
