//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"

	"ghosthand/proto"
)

func osVersion() string          { return runtime.GOOS }
func powerStatus() (*int, *bool) { return nil, nil }
func keepAwake() func()          { return func() {} }

func deviceMemory() *memoryStatus { return nil }

func readSystemMetrics(context.Context, proto.GUIArgs) systemMetrics { return systemMetrics{} }

func listProcesses(context.Context, proto.GUIArgs) (json.RawMessage, error) {
	return nil, errors.New("E_CAP_UNSUPPORTED")
}

func manageProcess(context.Context, proto.GUIArgs) (json.RawMessage, error) {
	return nil, errors.New("E_CAP_UNSUPPORTED")
}

func execShell(context.Context, proto.GUIArgs) (json.RawMessage, error) {
	return nil, errors.New("E_CAP_UNSUPPORTED")
}

func runScript(context.Context, proto.GUIArgs) (json.RawMessage, error) {
	return nil, errors.New("E_CAP_UNSUPPORTED")
}

func listFS(context.Context, proto.GUIArgs) (json.RawMessage, error) {
	return nil, errors.New("E_CAP_UNSUPPORTED")
}

func readFS(context.Context, proto.GUIArgs) (json.RawMessage, error) {
	return nil, errors.New("E_CAP_UNSUPPORTED")
}

func writeFS(context.Context, proto.GUIArgs) (json.RawMessage, error) {
	return nil, errors.New("E_CAP_UNSUPPORTED")
}

func corePowerScreen(context.Context, proto.GUIArgs) (json.RawMessage, bool, error) {
	return nil, false, errors.New("E_CAP_UNSUPPORTED")
}

func startUninstallWorker(context.Context) (uninstallWorker, error) {
	return uninstallWorker{}, errors.New("remote uninstall is only available on Windows")
}

func startPlatform(_ context.Context, _ context.CancelFunc, _ string, _ bool) (*host, <-chan struct{}) {
	done := make(chan struct{})
	close(done)
	return newHost("headless", "none"), done
}
