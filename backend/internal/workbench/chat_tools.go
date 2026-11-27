package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"ghosthand/backend/internal/brain"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

type chatToolCall struct {
	name, device, problem string
	args                  json.RawMessage
}

func (s *Service) planChatCalls(ctx context.Context, task brainTask, calls []brain.Call, mentioned []MCPDevice) []chatToolCall {
	crossDevice := task.source == "chat" || len(mentioned) > 0
	allowed := make(map[string]bool, len(mentioned))
	for _, device := range mentioned {
		allowed[device.ID] = true
	}
	planned := make([]chatToolCall, len(calls))
	for i, call := range calls {
		p := chatToolCall{name: call.Name, args: call.Args}
		switch {
		case call.Name == MCPDevicesTool.Name && crossDevice:
		case call.Name != proto.ToolConfirm && !IsMCPToolAllowed(call.Name):
			p.problem = "未知的设备工具。"
		case !crossDevice:
			p.device = task.device
		default:
			defaultDevice := task.device
			if task.source == "chat" && call.Name != proto.ToolConfirm {
				defaultDevice = ""
			}
			target, args, err := MCPArguments(call.Args, defaultDevice)
			if err != nil {
				p.problem = err.Error()
				break
			}
			p.args, p.device = args, target
			if target == task.device {
				if target == FreeChatDeviceID && call.Name != proto.ToolConfirm {
					p.problem = "请选择实际的物理设备。"
				}
				break
			}
			p.device, err = s.MCPDeviceID(ctx, target)
			if err != nil {
				p.problem = mcpDeviceError(err)
			} else if task.source != "chat" && p.device != task.device && !allowed[p.device] {
				p.problem = "跨设备操作仅限于本条消息中通过 @ 引用的设备。"
			}
		}
		planned[i] = p
	}
	return planned
}

func runChatCalls(ctx context.Context, calls []chatToolCall, execute func(context.Context, chatToolCall) (brain.Observation, error)) ([]brain.Observation, error) {
	observations := make([]brain.Observation, len(calls))
	groups := make(map[string][]int)
	serial := false
	for i, call := range calls {
		if call.name == proto.ToolConfirm {
			serial = true
		}
		device := call.device
		if call.problem != "" {
			device = ""
		}
		groups[device] = append(groups[device], i)
	}
	deviceCount := len(groups)
	if _, global := groups[""]; global {
		deviceCount--
	}
	if serial || deviceCount < 2 {
		for i, call := range calls {
			observation, err := execute(ctx, call)
			if err != nil {
				return nil, err
			}
			observations[i] = observation
		}
		return observations, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	var firstErr error
	var failed sync.Once
	for _, indices := range groups {
		workers.Go(func() {
			for _, i := range indices {
				if ctx.Err() != nil {
					return
				}
				observation, err := execute(ctx, calls[i])
				if err != nil {
					failed.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
				observations[i] = observation
			}
		})
	}
	workers.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return observations, ctx.Err()
}

type chatToolRunner struct {
	service   *Service
	task      brainTask
	remote    settings.Configuration
	confirmMu sync.Mutex
}

func (r *chatToolRunner) confirm(ctx context.Context, target, name, summary, risk, attachment string) error {
	// One parent task can only wait for one confirmation at a time.
	r.confirmMu.Lock()
	defer r.confirmMu.Unlock()
	approved, err := r.service.requestConfirmation(ctx, r.task, target, name, summary, risk, attachment)
	if err != nil {
		return err
	}
	if !approved {
		return errBrainDenied
	}
	return nil
}

func (r *chatToolRunner) execute(ctx context.Context, call chatToolCall) (brain.Observation, error) {
	s, task := r.service, r.task
	if err := s.awaitBrain(ctx, task.id); err != nil {
		return brain.Observation{}, err
	}
	if call.problem != "" {
		return brain.Observation{Text: call.problem, Error: true}, nil
	}
	if call.name == MCPDevicesTool.Name {
		var args map[string]json.RawMessage
		if json.Unmarshal(call.args, &args) != nil || args == nil || len(args) != 0 {
			return brain.Observation{Text: "devices_list 不接受参数。", Error: true}, nil
		}
		devices, err := s.MCPDevices(ctx)
		if err != nil {
			return brain.Observation{Text: "暂时无法获取设备列表。", Error: true}, nil
		}
		data, err := json.Marshal(map[string]any{"devices": devices})
		return brain.Observation{Text: string(data)}, err
	}
	var name string
	if call.device != FreeChatDeviceID {
		if err := s.store.QueryRowContext(ctx, "SELECT name FROM devices WHERE id=?", call.device).Scan(&name); err != nil {
			if err == sql.ErrNoRows {
				return brain.Observation{Text: mcpDeviceError(ErrDeviceNotFound), Error: true}, nil
			}
			return brain.Observation{}, err
		}
	}
	if call.name == proto.ToolConfirm {
		args, err := proto.ValidateTool(call.name, call.args)
		if err != nil {
			return brain.Observation{Text: err.Error(), Error: true}, nil
		}
		if err := r.confirm(ctx, call.device, name, args.Summary, args.RiskAction, args.AttachmentID); err != nil {
			return brain.Observation{}, err
		}
		return brain.Observation{Text: "操作已批准。具体交互操作仍需要与目标和参数匹配。"}, nil
	}
	return r.executeRemote(ctx, call, name)
}

func (r *chatToolRunner) executeRemote(ctx context.Context, call chatToolCall, name string) (brain.Observation, error) {
	s, task := r.service, r.task
	label := "[" + name + "] " + toolLabel(call.name)
	var seq int64
	err := s.writeBrainReady(ctx, task.id, func(tx *sql.Tx) error {
		now := time.Now()
		if task.source == "chat" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,task_id,created_at)
				VALUES (?,'agent','task_card',?,?) ON CONFLICT(task_id) WHERE type='task_card' DO NOTHING`, task.device, task.id, now.UnixMilli()); err != nil {
				return err
			}
		}
		// Remote events have their own sequence and do not advance the local agent's step counter.
		if err := tx.QueryRowContext(ctx, `INSERT INTO task_events(task_id,seq,step_key,step_name,status,detail,created_at)
			SELECT ?,COALESCE(MAX(seq),0)+1,'remote_tool',?,'running','',? FROM task_events WHERE task_id=? RETURNING seq`,
			task.id, label, now.UnixMilli(), task.id).Scan(&seq); err != nil {
			return err
		}
		return RecordChange(ctx, tx, task.device)
	})
	if err != nil {
		return brain.Observation{}, err
	}
	command := mcpCommand{Tool: call.name, Args: call.args, FromChat: true}
	result, err := s.callMCP(ctx, task.user, call.device, command, "", &task)
	if hint := confirmationHint(result.Error); err == nil && hint != "" {
		summary := limitText(hint+"\n"+toolLabel(call.name)+" "+string(call.args), 500)
		if err := r.confirm(ctx, call.device, name, summary, call.name, ""); err != nil {
			return brain.Observation{}, err
		}
		command.Approved = true
		result, err = s.callMCP(ctx, task.user, call.device, command, "", &task)
	}
	if ctx.Err() != nil {
		return brain.Observation{}, ctx.Err()
	}
	status, detail := "succeeded", ""
	observation := brain.Observation{Text: string(result.Data)}
	if err != nil {
		status, detail = "failed", mcpDeviceError(err)
		observation = brain.Observation{Text: detail, Error: true}
	} else if result.Error != "" {
		status, detail = "failed", result.Error
		observation = brain.Observation{Text: result.Error + "\n" + observation.Text, Error: true}
	}
	if observation.Text == "" {
		observation.Text = "工具执行完成。"
	}
	if id := result.AttachmentID; id != "" {
		data, err := s.loadModelImage(ctx, id)
		if err != nil {
			observation.Text += " 截图暂时不可读取。"
		} else {
			observation.JPEG, observation.ImageLabel = data, label
			if id, err := s.addChatToolMedia(ctx, task, name, data, r.remote); err != nil {
				observation.Text += " 截图未能保存到当前聊天记录。"
			} else {
				observation.Text += " 截图已写入当前聊天，附件 ID：" + id
			}
		}
	}
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE task_events SET status=?,detail=?,elapsed_ms=MAX(0,?-created_at)
			WHERE task_id=? AND seq=? AND status='running'`, status, limitText(detail, 512), time.Now().UnixMilli(), task.id, seq); err != nil {
			return err
		}
		return RecordChange(ctx, tx, task.device)
	})
	return observation, err
}
