package workbench

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"ghosthand/proto"
)

const mcpTaskTTL = 75 * time.Second

func mcpTTL(tool string, args json.RawMessage, quiet bool) time.Duration {
	timeout := toolTimeout(tool, args) + 10*time.Second
	if !quiet {
		timeout += toolTimeout(tool, args) + 45*time.Second
	}
	return max(mcpTaskTTL, timeout)
}

type MCPDevice struct {
	Device
	MCPReady bool `json:"mcp_ready"`
}

type mcpWindow struct {
	toolWindow
	generation int64
}

type mcpCommand struct {
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"arguments"`
	Quiet    bool            `json:"quiet,omitempty"`
	FromChat bool            `json:"from_chat,omitempty"`
	Approved bool            `json:"approved,omitempty"`
}

var MCPDevicesTool = proto.MCPTool{
	Name: "devices_list", Description: "列出所有已接入服务端的设备及其在线状态、系统版本、桌面锁屏状态与可执行能力。",
	InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`), RiskLevel: "read_only",
}

// AddressedMCPTool adds the server's routing field to the node's tool schema.
func AddressedMCPTool(tool proto.MCPTool, requireDevice bool) proto.MCPTool {
	return addressedMCPTool(tool, requireDevice, "设备 ID 或唯一设备名称")
}

func addressedMCPTool(tool proto.MCPTool, requireDevice bool, description string) proto.MCPTool {
	var schema struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required,omitempty"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		panic(err) // DeviceTools schemas are defined in the local protocol.
	}
	if schema.Properties == nil {
		schema.Properties = make(map[string]json.RawMessage)
	}
	schema.Properties["device_id"], _ = json.Marshal(map[string]any{
		"type": "string", "minLength": 1, "maxLength": 80, "description": description,
	})
	if requireDevice {
		schema.Required = append(schema.Required, "device_id")
	}
	tool.InputSchema, _ = json.Marshal(schema)
	return tool
}

var ErrMCPDeviceAmbiguous = errors.New("设备名称不唯一，请改用设备 ID。")
var errMCPDeviceArgument = errors.New("请填写有效的 device_id。")

func MCPArguments(raw json.RawMessage, defaultDevice string) (string, json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var args map[string]json.RawMessage
	if len(raw) > proto.MaxToolArgsSize+1024 || json.Unmarshal(raw, &args) != nil || args == nil {
		return "", nil, proto.ErrToolArguments
	}
	device := defaultDevice
	if value, exists := args["device_id"]; exists {
		delete(args, "device_id")
		if string(bytes.TrimSpace(value)) != "null" {
			var parsed string
			if json.Unmarshal(value, &parsed) != nil {
				return "", nil, errMCPDeviceArgument
			}
			if strings.TrimSpace(parsed) != "" || defaultDevice == "" {
				device = parsed
			}
		}
	}
	if strings.TrimSpace(device) == "" || utf8.RuneCountInString(device) > 80 {
		return "", nil, errMCPDeviceArgument
	}
	argsJSON, err := json.Marshal(args)
	return device, argsJSON, err
}

func IsMCPToolAllowed(tool string) bool {
	if tool == "" || tool == proto.ToolConfirm {
		return false
	}
	for _, entry := range proto.DeviceTools {
		if entry.Name == tool {
			return true
		}
	}
	return false
}

func IsMCPReadTool(tool string) bool {
	return IsMCPToolAllowed(tool)
}

func (s *Service) MCPDevices(ctx context.Context) ([]MCPDevice, error) {
	rows, err := s.store.QueryContext(ctx, `SELECT `+deviceFields+`,
		d.task_protocol>=2 AND d.conn_state IN ('online','unstable') AND json_array_length(d.mcp_tools)>0
		FROM devices d
		JOIN device_credentials c ON c.device_id=d.id
		WHERE d.is_demo=0 AND d.id<>? AND c.revoked_at IS NULL
		ORDER BY CASE d.conn_state WHEN 'online' THEN 0 WHEN 'unstable' THEN 1 WHEN 'sleeping' THEN 2 ELSE 3 END,
			d.group_name,d.name,d.id`, FreeChatDeviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := []MCPDevice{}
	for rows.Next() {
		var device MCPDevice
		if err := scanDeviceFields(rows, &device.Device, &device.MCPReady); err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (s *Service) MCPCapabilities(ctx context.Context, device string) ([]proto.MCPTool, []proto.MCPResource, error) {
	var toolsJSON, resourcesJSON string
	err := s.store.QueryRowContext(ctx, `SELECT d.mcp_tools,d.mcp_resources FROM devices d
		JOIN device_credentials c ON c.device_id=d.id
		WHERE d.id=? AND d.id<>? AND d.is_demo=0 AND c.revoked_at IS NULL`, device, FreeChatDeviceID).Scan(&toolsJSON, &resourcesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	var tools []proto.MCPTool
	var resources []proto.MCPResource
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(resourcesJSON), &resources); err != nil {
		return nil, nil, err
	}
	return CanonicalTools(tools), CanonicalResources(resources), nil
}

// CallMCP uses the same serialized desktop lane, local stop controls and audit trail as chat.
func (s *Service) CallMCP(ctx context.Context, user int64, device, tool string, args json.RawMessage) (proto.MCPResult, error) {
	return s.callMCP(ctx, user, device, mcpCommand{Tool: tool, Args: args}, "", nil)
}

func (s *Service) CallBrowserExtension(ctx context.Context, user int64, device, action string) (proto.MCPResult, error) {
	if action != "install" && action != "uninstall" {
		return proto.MCPResult{}, ErrUnsupported
	}
	args, _ := json.Marshal(map[string]string{"action": action, "browser": "chrome"})
	return s.callMCP(ctx, user, device, mcpCommand{Tool: proto.ToolBrowserManage, Args: args, Quiet: true}, "browser_ext."+action, nil)
}

func resolveMCPDevice(ctx context.Context, q brainStateQuery, target string) (string, error) {
	var device string
	var matches int
	err := q.QueryRowContext(ctx, `SELECT d.id,COUNT(*) OVER () FROM devices d
		JOIN device_credentials c ON c.device_id=d.id
		WHERE d.id<>? AND d.is_demo=0 AND c.revoked_at IS NULL
		AND (d.id=? OR (d.name=? AND NOT EXISTS(SELECT 1 FROM devices WHERE id=?)))
		ORDER BY d.id LIMIT 1`, FreeChatDeviceID, target, target, target).Scan(&device, &matches)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrDeviceNotFound
	}
	if err != nil {
		return "", err
	}
	if matches > 1 {
		return "", ErrMCPDeviceAmbiguous
	}
	return device, nil
}

func (s *Service) MCPDeviceID(ctx context.Context, target string) (string, error) {
	return resolveMCPDevice(ctx, s.store, target)
}

func (s *Service) callMCP(ctx context.Context, user int64, target string, command mcpCommand, auditAction string, origin *brainTask) (proto.MCPResult, error) {
	var result proto.MCPResult
	tool, args, quiet := command.Tool, command.Args, command.Quiet
	if !IsMCPToolAllowed(tool) {
		return result, ErrUnsupported
	}
	if _, err := proto.ValidateTool(tool, args); err != nil {
		return result, err
	}
	ttl := mcpTTL(tool, args, quiet)
	device := target
	var taskID string
	queue := func(tx *sql.Tx) error {
		var err error
		if !command.FromChat && !quiet {
			device, err = resolveMCPDevice(ctx, tx, target)
			if err != nil {
				return err
			}
		}
		var available, busy bool
		// Another device chat can hold the target UI lane while waiting for this chat.
		fromDeviceChat := origin != nil && origin.source == "brain"
		err = tx.QueryRowContext(ctx, `SELECT d.task_protocol>=2 AND d.conn_state IN ('online','unstable')
			AND EXISTS (SELECT 1 FROM json_each(d.mcp_tools) WHERE json_extract(value,'$.name')=?),
			EXISTS (SELECT 1 FROM tasks WHERE device_id=d.id AND (source='mcp' OR (? AND lane='ui') OR (? AND source='brain'))
				AND state IN ('queued','dispatched','running','paused','waiting_user','cancelling'))
			FROM devices d JOIN device_credentials c ON c.device_id=d.id
			WHERE d.id=? AND d.id<>? AND d.is_demo=0 AND c.revoked_at IS NULL`, tool, quiet, fromDeviceChat, device, FreeChatDeviceID).Scan(&available, &busy)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeviceNotFound
		}
		if err != nil {
			return err
		}
		if !available {
			return ErrUnsupported
		}
		if busy {
			return ErrTaskControl
		}
		body, err := json.Marshal(command)
		if err != nil {
			return err
		}
		taskID, err = insertTask(ctx, tx, user, Device{ID: device}, brainCommand, string(body), "mcp",
			"MCP · "+toolLabel(tool), time.Now(), ttl, true)
		if err != nil {
			return err
		}
		if auditAction != "" {
			if err := auditTask(ctx, tx, user, device, taskID, auditAction, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return RecordChange(ctx, tx, device)
	}
	var err error
	if origin == nil {
		err = s.store.Write(ctx, queue)
	} else {
		err = s.writeBrainReady(ctx, origin.id, queue)
	}
	if err != nil {
		return result, err
	}
	wait, cancel := context.WithTimeout(ctx, brainTaskTTL)
	defer cancel()
	deadline := taskDeadline{expires: time.Now().Add(ttl)}
	for wait.Err() == nil {
		changed := s.store.Changed()
		var state proto.TaskState
		var raw sql.NullString
		err = s.store.QueryRowContext(wait, `SELECT state,(SELECT result_json FROM brain_calls WHERE task_id=tasks.id ORDER BY created_at DESC,id DESC LIMIT 1)
			FROM tasks WHERE id=?`, taskID).Scan(&state, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return result, context.Canceled
		}
		if err != nil {
			break
		}
		switch state {
		case proto.Cancelling, proto.Cancelled:
			return result, context.Canceled
		case proto.Failed, proto.Expired, proto.Unknown, proto.Succeeded:
			if raw.Valid && (state == proto.Succeeded || state == proto.Failed) {
				err := json.Unmarshal([]byte(raw.String), &result)
				return result, err
			}
			return result, errors.New("设备任务已结束，未返回诊断结果。")
		}
		if err = deadline.wait(wait, changed, state == proto.Paused); err != nil {
			break
		}
	}
	cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancelCleanup()
	_ = s.Control(cleanup, user, device, taskID, "cancel")
	if wait.Err() != nil {
		return result, wait.Err()
	}
	return result, err
}

func (s *Service) runMCP(parent context.Context, task brainTask) {
	var command mcpCommand
	valid := json.Unmarshal([]byte(task.prompt), &command) == nil
	task.quiet = command.Quiet
	unlock := taskUnlock{service: s, task: task}
	defer unlock.finish(parent)
	ctx, cancel := context.WithTimeout(parent, brainTaskTTL)
	defer cancel()
	s.registerBrainSession(task.device, task.id, cancel)
	defer s.unregisterBrainSession(task.device, task.id)
	go s.watchBrain(ctx, cancel, task.id)
	state := proto.Failed
	args, argsErr := proto.ValidateTool(command.Tool, command.Args)
	if valid && argsErr == nil && IsMCPToolAllowed(command.Tool) {
		window := s.mcpObservedWindow(ctx, task.device)
		expected := ""
		if (command.Tool == proto.ToolClick || command.Tool == proto.ToolInput) && args.ElementID != nil {
			expected = window.ID
			if expected == "" {
				// An element ID without an observation must never match a helper's cached elements.
				expected = "unobserved"
			}
		}
		approved := !command.FromChat || command.Approved || s.deviceAutoApprove(ctx, task.device)
		attempt := 1
		label := toolLabel(command.Tool)
		result, err := s.callDevice(ctx, task, 1, attempt, command.Tool, command.Args, label, expected, approved)
		if err == nil {
			result, err = unlock.retry(ctx, result, 1, &attempt, command.Tool, command.Args, label, expected, approved)
		}
		if err == nil {
			window.toolWindow = nextToolWindow(window.toolWindow, command.Tool, result)
			s.mcpWindowMu.Lock()
			if window.ID == "" {
				delete(s.mcpWindows, task.device)
			} else {
				if s.mcpWindows == nil {
					s.mcpWindows = make(map[string]mcpWindow)
				}
				s.mcpWindows[task.device] = window
			}
			s.mcpWindowMu.Unlock()
			if result.Error == "" {
				state = proto.Succeeded
			}
		}
	}
	if parent.Err() != nil {
		return
	}
	if err := s.completeBrain(ctx, parent, task, "", state); err != nil {
		slog.Error("finish MCP task", "task", task.id, "error", err)
	}
}

func (s *Service) mcpObservedWindow(ctx context.Context, device string) mcpWindow {
	var generation int64
	if err := s.store.QueryRowContext(ctx, "SELECT conn_gen FROM devices WHERE id=?", device).Scan(&generation); err != nil {
		return mcpWindow{}
	}
	s.mcpWindowMu.Lock()
	defer s.mcpWindowMu.Unlock()
	if window := s.mcpWindows[device]; window.generation == generation {
		return window
	}
	return mcpWindow{generation: generation}
}

func (s *Service) MCPImage(ctx context.Context, attachmentID string) ([]byte, error) {
	return s.loadModelImage(ctx, attachmentID)
}
