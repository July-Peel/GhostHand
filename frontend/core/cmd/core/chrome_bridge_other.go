//go:build !windows

package main

import (
	"context"

	"ghosthand/proto"
)

type browserBridge struct{}

func runBrowserProcess([]string) (bool, error) { return false, nil }

func startBrowserBridge(context.Context, *host, string) *browserBridge { return nil }

func (a *agent) executeBrowserTool(_ context.Context, call proto.MCPCall, _ <-chan struct{}) proto.MCPResult {
	return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "浏览器扩展桥接仅支持 Windows 节点"}
}

func (a *agent) cleanupBrowserExtension(context.Context) error { return nil }
