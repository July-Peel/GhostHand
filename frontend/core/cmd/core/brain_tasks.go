package main

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"time"

	"ghosthand/proto"
)

type memoryStatus struct {
	Total     uint64 `json:"total_bytes"`
	Available uint64 `json:"available_bytes"`
}

type systemMetrics struct {
	CPU         *float64     `json:"cpu_pct,omitempty"`
	Disks       []diskStatus `json:"disks,omitempty"`
	Addresses   []string     `json:"local_ips,omitempty"`
	CoreAccount string       `json:"core_privilege,omitempty"`
}

type diskStatus struct {
	Drive string `json:"drive"`
	Total uint64 `json:"total_bytes"`
	Free  uint64 `json:"free_bytes"`
}

func (a *agent) executeBrainTask(ctx, process context.Context, assigned localTask, wake <-chan struct{}) error {
	release := keepAwake()
	defer release()
	defer a.platform.overlay(taskOverlay{TaskDisplay: proto.TaskDisplay{TaskID: assigned.TaskID}})
	var shown proto.TaskDisplay
	for ctx.Err() == nil {
		var record localTask
		var call *proto.MCPCall
		err := a.local.changeTask(assigned.TaskID, func(t *localTask) error {
			if t.Finish == nil {
				if err := a.local.applyLocalControl(t); err != nil {
					return err
				}
			}
			if t.Finish == nil && t.State == proto.Running && t.Call != nil && !t.CallStarted {
				t.CallStarted = true
				t.Step = t.Call.Display.StepIndex
				call = t.Call
			}
			record = *t
			return nil
		})
		if errors.Is(err, context.Canceled) {
			break
		}
		if err != nil {
			return err
		}
		if record.Finish != nil {
			return a.local.finishTask(assigned.TaskID, *record.Finish)
		}
		display := record.Display
		if display.TaskID == "" {
			display = proto.TaskDisplay{TaskID: assigned.TaskID, Title: assigned.Title, StepName: "等待模型返回"}
		}
		if record.State == proto.Paused || display.State != proto.WaitingUser {
			display.State = record.State
		}
		if display != shown {
			a.platform.overlay(taskOverlay{TaskDisplay: display, Visible: true})
			shown = display
		}
		if call != nil {
			result := a.executeTool(ctx, *call, wake)
			if ctx.Err() != nil {
				break
			}
			err := a.local.changeTask(assigned.TaskID, func(t *localTask) error {
				t.LastCall, t.Call, t.CallStarted = call.CallID, nil, false
				t.TicketRequest, t.Ticket = "", proto.ScreenshotTicket{}
				return a.local.enqueueInLock(proto.MCPToolResult, result)
			})
			if err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
		case <-wake:
		}
	}
	if process.Err() != nil {
		return nil
	}
	return a.local.finishTask(assigned.TaskID, proto.TaskResultPayload{State: proto.Cancelled, ErrorCode: proto.ErrCancelled})
}

func (a *agent) executeTool(ctx context.Context, call proto.MCPCall, wake <-chan struct{}) proto.MCPResult {
	result := proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID}
	if proto.IsBrowserTool(call.Tool) {
		return a.executeBrowserTool(ctx, call, wake)
	}
	args, err := proto.ValidateTool(call.Tool, call.Arguments)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if call.Tool == proto.ToolProcesses {
		data, err := listProcesses(ctx, args)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Data = data
		}
		return result
	}
	if call.Tool == proto.ToolProcessManage {
		data, err := manageProcess(ctx, args)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Data = data
		}
		return result
	}
	if call.Tool == proto.ToolShellExec {
		data, err := execShell(ctx, args)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Data = data
		}
		return result
	}
	if call.Tool == proto.ToolScriptRun {
		data, err := runScript(ctx, args)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Data = data
		}
		return result
	}
	if call.Tool == proto.ToolFSList {
		data, err := listFS(ctx, args)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Data = data
		}
		return result
	}
	if call.Tool == proto.ToolFSRead {
		data, err := readFS(ctx, args)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Data = data
		}
		return result
	}
	if call.Tool == proto.ToolFSWrite {
		data, err := writeFS(ctx, args)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Data = data
		}
		return result
	}
	if call.Tool == proto.ToolPowerScreen {
		if data, handled, err := corePowerScreen(ctx, args); handled {
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Data = data
			}
			return result
		}
	}
	if call.Tool == proto.ToolStatus {
		snapshot := a.platform.snapshot()
		var foreground json.RawMessage
		if snapshot.DesktopState == "unlocked" {
			observe, cancel := context.WithTimeout(ctx, 2*time.Second)
			if reply, err := a.platform.callHelper(observe, call.TaskID, "mcp.call", call); err == nil {
				foreground = reply.Data
			}
			cancel()
		}
		result.Data, _ = json.Marshal(struct {
			proto.DeviceState
			systemMetrics
			Platform   string          `json:"platform"`
			Arch       string          `json:"arch"`
			OS         string          `json:"os_version"`
			CPUs       int             `json:"logical_cpu_count"`
			Memory     *memoryStatus   `json:"memory,omitempty"`
			Foreground json.RawMessage `json:"foreground,omitempty"`
		}{snapshot, readSystemMetrics(ctx, args), runtime.GOOS, runtime.GOARCH, osVersion(), runtime.NumCPU(), deviceMemory(), foreground})
		return result
	}
	timeout := 20 * time.Second
	if call.Tool == proto.ToolWait {
		timeout = max(timeout, (time.Duration(args.TimeoutMs)+5000)*time.Millisecond)
	}
	if call.Tool == proto.ToolTerminalOpen || call.Tool == proto.ToolLaunch {
		timeout = 25 * time.Second
	}
	helper, cancel := context.WithTimeout(ctx, timeout)
	reply, err := a.platform.callHelper(helper, call.TaskID, "mcp.call", call)
	cancel()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if len(reply.Data) > proto.MaxToolDataSize {
		result.Error = "工具结果超出大小上限"
		return result
	}
	result.Data = reply.Data
	if call.Tool == proto.ToolObserve {
		if len(reply.JPEG) == 0 || len(reply.JPEG) > proto.MaxScreenshotSize {
			result.Error = "截图格式无效"
			return result
		}
		name := "上传观察截图"
		if args.Annotate {
			name = "上传标注截图"
		}
		display := call.Display
		display.StepName = name
		a.platform.overlay(taskOverlay{TaskDisplay: display, Visible: true})
		upload, cancel := context.WithTimeout(ctx, 55*time.Second)
		defer cancel()
		attachment, err := a.uploadScreenshot(upload, call.TaskID, call.CallID, reply.JPEG, wake)
		if err != nil {
			result.Error = "截图上传失败"
			return result
		}
		result.AttachmentID = attachment.AttachmentID
	}
	return result
}
