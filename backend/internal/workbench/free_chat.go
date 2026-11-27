package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"ghosthand/backend/internal/brain"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

const FreeChatDeviceID = "__chat__"

const freeChatInstructions = `当前频道是「随意闲聊」，没有绑定设备。普通问答、代码、文本和图片请求可直接回答。
仅在用户要求查看或操作设备时调用设备工具。用 devices_list 确认目标，然后为设备工具传入 device_id。目标无法唯一确定时向用户询问。`

var (
	ErrFreeChatDisabled = errors.New("请先在「系统设置 → 模型接口」配置服务地址、API Key 和模型。")
	ErrFreeChatBusy     = errors.New("上一条消息仍在处理，请稍后再发送。")
)

type FreeChatStatus struct {
	Enabled   bool   `json:"enabled"`
	Model     string `json:"model"`
	TaskState string `json:"task_state"`
}

func (s *Service) FreeChatStatus(ctx context.Context) (FreeChatStatus, error) {
	configs, err := s.config.Read(ctx)
	if err != nil {
		return FreeChatStatus{}, err
	}
	config := configs[settings.LLM]
	status := FreeChatStatus{Enabled: config.FreeChatEnabled(), Model: config.Label, TaskState: "idle"}
	if !status.Enabled {
		return status, nil
	}
	err = s.store.QueryRowContext(ctx, `SELECT COALESCE((SELECT state FROM tasks
		WHERE device_id=? AND source='chat' AND state IN ('running','waiting_user') LIMIT 1), 'idle')`,
		FreeChatDeviceID).Scan(&status.TaskState)
	return status, err
}

func insertFreeChat(ctx context.Context, tx *sql.Tx, user int64, text string, images []storedImage, now time.Time) error {
	var enabled, busy bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM integration_settings WHERE kind='llm' AND endpoint<>'' AND label<>'' AND secret IS NOT NULL),
		EXISTS(SELECT 1 FROM tasks WHERE device_id=? AND source='chat' AND state IN ('running','waiting_user'))`,
		FreeChatDeviceID).Scan(&enabled, &busy); err != nil {
		return err
	}
	if !enabled {
		return ErrFreeChatDisabled
	}
	if busy {
		return ErrFreeChatBusy
	}
	title := limitText(text, 80)
	if title == "" {
		title = "图片分析"
	}
	command := brainCommand
	command.Lane = proto.BackgroundLane
	task, err := insertTask(ctx, tx, user, Device{ID: FreeChatDeviceID}, command, text, "chat", title, now, brainTaskTTL, true)
	if err != nil {
		return err
	}
	if err := insertUserMessage(ctx, tx, FreeChatDeviceID, task, text, images, now); err != nil {
		return err
	}
	return RecordChange(ctx, tx, FreeChatDeviceID)
}

func (s *Service) freeChatHistory(ctx context.Context, task string) ([]brain.Message, error) {
	rows, err := s.store.QueryContext(ctx, `SELECT CASE sender WHEN 'agent' THEN 'assistant' ELSE 'user' END,text
		FROM chat_messages WHERE device_id=? AND sender IN ('user','agent') AND type='text' AND text<>''
		AND id < (SELECT id FROM chat_messages WHERE task_id=? AND sender='user' AND type='text' LIMIT 1)
		ORDER BY id DESC LIMIT 20`, FreeChatDeviceID, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []brain.Message
	for rows.Next() {
		var message brain.Message
		if err := rows.Scan(&message.Role, &message.Text); err != nil {
			return nil, err
		}
		history = append(history, message)
	}
	slices.Reverse(history)
	return history, rows.Err()
}

func freeChatTools() []proto.MCPTool {
	tools := make([]proto.MCPTool, 0, len(proto.DeviceTools)+2)
	tools = append(tools, MCPDevicesTool, proto.ConfirmTool)
	for _, tool := range proto.DeviceTools {
		tools = append(tools, AddressedMCPTool(tool, true))
	}
	return tools
}

func (s *Service) runFreeChat(ctx context.Context, task brainTask) (string, error) {
	if err := s.awaitBrain(ctx, task.id); err != nil {
		return "", err
	}
	configs, err := s.config.Read(ctx)
	if err != nil {
		return "", err
	}
	config := configs[settings.LLM]
	if !config.FreeChatEnabled() {
		return "", ErrFreeChatDisabled
	}
	history, err := s.freeChatHistory(ctx, task.id)
	if err != nil {
		return "", err
	}
	images, err := s.loadTaskUserImages(ctx, task.id)
	if err != nil {
		return "", err
	}
	prompt := task.prompt
	if prompt == "" && len(images) > 0 {
		prompt = imageAnalysisPrompt
	}
	session, err := brain.NewWithImages(config.Endpoint, config.Secret, config.Label, prompt, images, freeChatTools())
	if err != nil {
		return "", err
	}
	defer session.Clear()
	session.SetInstructions(config.Instructions)
	session.PrependHistory(append([]brain.Message{{Role: "developer", Text: freeChatInstructions}}, history...))
	attempts := 0
	for {
		turn, err := session.Next(ctx)
		if err != nil || len(turn.Calls) == 0 {
			return turn.Text, err
		}
		attempts += len(turn.Calls)
		if attempts > brain.MaxSteps {
			return "", fmt.Errorf("已达到 %d 次工具调用上限。", brain.MaxSteps)
		}
		observations := make([]brain.Observation, 0, len(turn.Calls))
		for _, call := range turn.Calls {
			if err := s.awaitBrain(ctx, task.id); err != nil {
				return "", err
			}
			observation, err := s.freeChatTool(ctx, task, call, configs[settings.WebDAV])
			if err != nil {
				return "", err
			}
			observations = append(observations, observation)
		}
		session.Return(turn.Calls, observations)
	}
}

func (s *Service) freeChatTool(ctx context.Context, task brainTask, call brain.Call, remote settings.Configuration) (brain.Observation, error) {
	failure := func(message string) (brain.Observation, error) {
		return brain.Observation{Text: message, Error: true}, nil
	}
	switch call.Name {
	case MCPDevicesTool.Name:
		var args map[string]json.RawMessage
		if json.Unmarshal(call.Args, &args) != nil || args == nil || len(args) != 0 {
			return failure("devices_list 不接受参数。")
		}
		devices, err := s.MCPDevices(ctx)
		if err != nil {
			return failure("暂时无法获取设备列表。")
		}
		data, err := json.Marshal(map[string]any{"devices": devices})
		return brain.Observation{Text: string(data)}, err
	case proto.ToolConfirm:
		args, err := proto.ValidateTool(call.Name, call.Args)
		if err != nil {
			return failure(err.Error())
		}
		if err := s.confirmFreeChat(ctx, task, args.Summary, args.RiskAction, args.AttachmentID); err != nil {
			return brain.Observation{}, err
		}
		return brain.Observation{Text: "管理员已确认。"}, nil
	}
	if !IsMCPToolAllowed(call.Name) {
		return failure("未知的设备工具。")
	}
	target, args, err := MCPArguments(call.Args, "")
	if err != nil {
		return failure(err.Error())
	}
	device, err := s.MCPDeviceID(ctx, target)
	if err != nil {
		return failure(mcpDeviceError(err))
	}
	command := mcpCommand{Tool: call.Name, Args: args, FromChat: true}
	result, err := s.callMCP(ctx, task.user, device, command, "")
	if err != nil {
		return failure(mcpDeviceError(err))
	}
	if hint := confirmationHint(result.Error); hint != "" {
		summary := limitText(hint+"\n"+target+" ("+device+") · "+toolLabel(call.Name)+" "+string(args), 500)
		if err := s.confirmFreeChat(ctx, task, summary, call.Name, ""); err != nil {
			return brain.Observation{}, err
		}
		command.Approved = true
		result, err = s.callMCP(ctx, task.user, device, command, "")
		if err != nil {
			return failure(mcpDeviceError(err))
		}
	}
	if result.Error != "" {
		return failure(result.Error + "\n" + string(result.Data))
	}
	observation := brain.Observation{Text: string(result.Data)}
	if observation.Text == "" {
		observation.Text = "工具执行完成。"
	}
	if id := result.AttachmentID; id != "" {
		observation.JPEG, err = s.loadModelImage(ctx, id)
		if err != nil {
			observation.Text += " 截图暂时不可读取。"
		} else if err := s.addFreeChatMedia(ctx, task, observation.JPEG, remote); err != nil {
			observation.Text += " 截图未能保存到闲聊记录。"
		} else {
			observation.Text += " 截图已写入闲聊记录。"
		}
	}
	return observation, nil
}

func (s *Service) confirmFreeChat(ctx context.Context, task brainTask, summary, risk, attachment string) error {
	approved, err := s.requestConfirmation(ctx, task, summary, risk, attachment)
	if err != nil {
		return err
	}
	if !approved {
		return errBrainDenied
	}
	return nil
}

func (s *Service) addFreeChatMedia(ctx context.Context, task brainTask, data []byte, remote settings.Configuration) error {
	now := time.Now()
	image, err := s.saveUserAttachment(ctx, UploadImage{Name: "设备截图.jpg", Data: data}, remote, now)
	if err != nil {
		return err
	}
	err = s.writeBrainReady(ctx, task.id, func(tx *sql.Tx) error {
		if err := image.insert(ctx, tx, task.id, "chat_"+image.ID, now); err != nil {
			return err
		}
		body, _ := json.Marshal(MediaCard{ID: image.ID, Name: image.name, Size: int64(image.size)})
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,task_id,payload_json,created_at)
			VALUES (?,'agent','media',?,?,?)`, task.device, task.id, string(body), now.UnixMilli()); err != nil {
			return err
		}
		return RecordChange(ctx, tx, task.device)
	})
	if err != nil {
		s.deleteAttachmentFiles(ctx, image.AttachmentFiles)
	}
	return err
}

func mcpDeviceError(err error) string {
	switch {
	case errors.Is(err, ErrDeviceNotFound):
		return "设备不存在或已撤销。"
	case errors.Is(err, ErrUnsupported):
		return "设备离线或尚未提供此工具。"
	case errors.Is(err, ErrTaskControl):
		return "此设备已有一项 MCP 任务在执行。"
	case errors.Is(err, ErrMCPDeviceAmbiguous), errors.Is(err, proto.ErrToolArguments):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "等待设备任务结果超时。"
	case errors.Is(err, context.Canceled):
		return "设备任务已取消。"
	default:
		return "设备任务未完成。"
	}
}
