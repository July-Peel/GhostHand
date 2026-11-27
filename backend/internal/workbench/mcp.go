package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

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
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	State proto.ConnState `json:"conn_state"`
}

type mcpCommand struct {
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"arguments"`
	Quiet bool            `json:"quiet,omitempty"`
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
	rows, err := s.store.QueryContext(ctx, `SELECT d.id,d.name,d.conn_state FROM devices d
		JOIN device_credentials c ON c.device_id=d.id
		WHERE d.is_demo=0 AND d.task_protocol>=2 AND c.revoked_at IS NULL
		AND d.conn_state IN ('online','unstable') AND json_array_length(d.mcp_tools)>0
		ORDER BY d.name,d.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := []MCPDevice{}
	for rows.Next() {
		var device MCPDevice
		if err := rows.Scan(&device.ID, &device.Name, &device.State); err != nil {
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
		WHERE d.id=? AND d.is_demo=0 AND c.revoked_at IS NULL`, device).Scan(&toolsJSON, &resourcesJSON)
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
	return s.callMCP(ctx, user, device, tool, args, "")
}

func (s *Service) CallBrowserExtension(ctx context.Context, user int64, device, action string) (proto.MCPResult, error) {
	if action != "install" && action != "uninstall" {
		return proto.MCPResult{}, ErrUnsupported
	}
	args, _ := json.Marshal(map[string]string{"action": action, "browser": "chrome"})
	return s.callMCP(ctx, user, device, proto.ToolBrowserManage, args, "browser_ext."+action)
}

func (s *Service) callMCP(ctx context.Context, user int64, device, tool string, args json.RawMessage, auditAction string) (proto.MCPResult, error) {
	var result proto.MCPResult
	if !IsMCPToolAllowed(tool) {
		return result, ErrUnsupported
	}
	if _, err := proto.ValidateTool(tool, args); err != nil {
		return result, err
	}
	quiet := auditAction != ""
	ttl := mcpTTL(tool, args, quiet)
	var taskID string
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var available, busy bool
		err := tx.QueryRowContext(ctx, `SELECT d.task_protocol>=2 AND d.conn_state IN ('online','unstable')
			AND EXISTS (SELECT 1 FROM json_each(d.mcp_tools) WHERE json_extract(value,'$.name')=?),
			EXISTS (SELECT 1 FROM tasks WHERE device_id=d.id AND (source='mcp' OR (? AND lane='ui'))
				AND state IN ('queued','dispatched','running','paused','waiting_user','cancelling'))
			FROM devices d JOIN device_credentials c ON c.device_id=d.id
			WHERE d.id=? AND d.is_demo=0 AND c.revoked_at IS NULL`, tool, quiet, device).Scan(&available, &busy)
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
		command, err := json.Marshal(mcpCommand{Tool: tool, Args: args, Quiet: quiet})
		if err != nil {
			return err
		}
		taskID, err = insertTask(ctx, tx, user, Device{ID: device}, brainCommand, string(command), "mcp",
			"MCP · "+toolLabel(tool), time.Now(), ttl, quiet)
		if err != nil {
			return err
		}
		if quiet {
			if err := auditTask(ctx, tx, user, device, taskID, auditAction, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return RecordChange(ctx, tx, device)
	})
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
	state, message := proto.Failed, "外部 MCP 调用失败。"
	if valid && IsMCPToolAllowed(command.Tool) {
		attempt := 1
		result, err := s.callDevice(ctx, task, 1, attempt, command.Tool, command.Args, toolLabel(command.Tool), "", true)
		if err == nil {
			result, err = unlock.retry(ctx, result, 1, &attempt, command.Tool, command.Args, toolLabel(command.Tool), "", true)
		}
		if err == nil && result.Error == "" {
			state, message = proto.Succeeded, "外部 MCP 调用已完成。"
		}
	}
	if parent.Err() != nil {
		return
	}
	if err := s.completeBrain(ctx, parent, task, message, state); err != nil {
		slog.Error("finish MCP task", "task", task.id, "error", err)
	}
}

func (s *Service) MCPImage(ctx context.Context, attachmentID string) ([]byte, error) {
	return s.loadModelImage(ctx, attachmentID)
}
