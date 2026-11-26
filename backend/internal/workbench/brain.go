package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"ghosthand/backend/internal/brain"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

var errBrainInactive = errors.New("AI 任务已结束")
var errBrainWait = errors.New("AI 任务暂时无法继续")
var errBrainDenied = errors.New("管理员已拒绝该操作，任务已停止。")

var brainCommand = Command{Title: "AI 任务", Capability: proto.BrainTask, Lane: proto.UILane}

type brainTask struct{ id, device, prompt, title, source string }

func CanonicalTools(tools []proto.MCPTool) []proto.MCPTool {
	result := make([]proto.MCPTool, 0, len(proto.DeviceTools))
	for _, tool := range proto.DeviceTools {
		for _, advertised := range tools {
			if tool.Name == advertised.Name {
				result = append(result, tool)
				break
			}
		}
	}
	return result
}

func CanonicalResources(resources []proto.MCPResource) []proto.MCPResource {
	result := make([]proto.MCPResource, 0, len(proto.DeviceResources))
	for _, resource := range proto.DeviceResources {
		for _, advertised := range resources {
			if resource.URI == advertised.URI {
				result = append(result, resource)
				break
			}
		}
	}
	return result
}

func (s *Service) queueBrain(ctx context.Context, tx *sql.Tx, device Device, user int64, prompt string,
	config settings.Configuration, now time.Time) error {
	if config.Endpoint == "" || config.Label == "" {
		return addReply(ctx, tx, device.ID, "", "尚未配置 AI 模型接口。请在「系统设置 → 模型接口」填写服务地址并选择模型，或使用下方快捷指令。", now.UnixMilli())
	}
	if device.Demo || device.Platform != proto.Windows {
		return addReply(ctx, tx, device.ID, "", "AI 任务需要已配对的 Windows 执行器。", now.UnixMilli())
	}
	return createTask(ctx, tx, user, device, brainCommand, prompt, now)
}

func addReply(ctx context.Context, tx *sql.Tx, device, task, content string, now int64) error {
	var taskID any
	if task != "" {
		taskID = task
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,text,task_id,created_at)
		VALUES (?,'agent','text',?,?,?)`, device, content, taskID, now)
	return err
}

func (s *Service) RunBrain(ctx context.Context) {
	var workers sync.WaitGroup
	defer workers.Wait()
	if err := s.recoverBrain(ctx); err != nil && ctx.Err() == nil {
		slog.Error("recover AI tasks", "error", err)
	}
	slots := make(chan struct{}, 8)
	slotFreed := make(chan struct{}, 1)
	for ctx.Err() == nil {
		changed := s.store.Changed()
		retry := false
		if len(slots) < cap(slots) {
			var task brainTask
			err := s.store.QueryRowContext(ctx, `SELECT id,device_id,prompt,title,source FROM tasks
				WHERE source IN ('brain','mcp') AND state='running' AND control='' AND brain_started_at IS NULL
				ORDER BY created_at,id LIMIT 1`).Scan(&task.id, &task.device, &task.prompt, &task.title, &task.source)
			if err == nil {
				claimed, err := s.claimBrain(ctx, task.id)
				if err != nil {
					slog.Error("claim AI task", "error", err)
					retry = true
				} else if claimed {
					slots <- struct{}{}
					workers.Add(1)
					go func() {
						defer workers.Done()
						defer func() {
							<-slots
							select {
							case slotFreed <- struct{}{}:
							default:
							}
						}()
						if task.source == "mcp" {
							s.runMCP(ctx, task)
						} else {
							s.runBrain(ctx, task)
						}
					}()
					continue
				}
			} else if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
				slog.Error("read AI task queue", "error", err)
				retry = true
			}
		}
		var timer *time.Timer
		var due <-chan time.Time
		if retry {
			timer = time.NewTimer(time.Second)
			due = timer.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-slotFreed:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Service) claimBrain(ctx context.Context, id string) (bool, error) {
	var claimed bool
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET brain_started_at=? WHERE id=? AND state='running'
			AND control='' AND brain_started_at IS NULL`, time.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		claimed = n == 1
		return err
	})
	return claimed, err
}

func (s *Service) recoverBrain(ctx context.Context) error {
	rows, err := s.store.QueryContext(ctx, `SELECT id,device_id,source,created_by FROM tasks
		WHERE (source='brain' AND brain_started_at IS NOT NULL AND state IN ('running','paused','waiting_user'))
		OR (source='mcp' AND state IN ('queued','dispatched','running','paused','waiting_user') AND created_at<?)`, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	type recoveryTask struct {
		brainTask
		user int64
	}
	var tasks []recoveryTask
	for rows.Next() {
		var t recoveryTask
		if err := rows.Scan(&t.id, &t.device, &t.source, &t.user); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range tasks {
		if t.source == "mcp" {
			if err := s.Control(ctx, t.user, t.device, t.id, "cancel"); err != nil && !errors.Is(err, ErrTaskControl) {
				return err
			}
			continue
		}
		if err := s.finishBrain(ctx, t.brainTask, "控制端重启，任务已停止。请核对设备上的实际结果。", proto.Failed); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) registerBrainSession(deviceID, taskID string, cancel context.CancelFunc) {
	s.brainMu.Lock()
	defer s.brainMu.Unlock()
	if s.brainSessions == nil {
		s.brainSessions = make(map[string]map[string]context.CancelFunc)
	}
	tasks := s.brainSessions[deviceID]
	if tasks == nil {
		tasks = make(map[string]context.CancelFunc)
		s.brainSessions[deviceID] = tasks
	}
	tasks[taskID] = cancel
}

func (s *Service) unregisterBrainSession(deviceID, taskID string) {
	s.brainMu.Lock()
	defer s.brainMu.Unlock()
	if tasks := s.brainSessions[deviceID]; tasks != nil {
		delete(tasks, taskID)
		if len(tasks) == 0 {
			delete(s.brainSessions, deviceID)
		}
	}
}

func (s *Service) cancelDeviceBrainSessions(deviceID string) {
	s.brainMu.Lock()
	tasks := s.brainSessions[deviceID]
	delete(s.brainSessions, deviceID)
	s.brainMu.Unlock()
	for _, cancel := range tasks {
		cancel()
	}
}

func (s *Service) runBrain(parent context.Context, task brainTask) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Minute)
	defer cancel()
	s.registerBrainSession(task.device, task.id, cancel)
	defer s.unregisterBrainSession(task.device, task.id)
	go s.watchBrain(ctx, cancel, task.id)
	reply, err := s.runLoop(ctx, task)
	if parent.Err() != nil {
		return
	}
	state := proto.Succeeded
	if err != nil {
		if errors.Is(err, errBrainInactive) || errors.Is(err, context.Canceled) {
			return
		}
		reply, state = err.Error(), proto.Failed
		if errors.Is(err, errBrainDenied) {
			state = proto.Cancelled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			reply = "AI 任务超时。请核对设备上的实际结果。"
		}
	}
	if strings.TrimSpace(reply) == "" {
		reply = "模型未返回总结。"
	}
	finish, cancelFinish := context.WithTimeout(parent, 8*time.Second)
	defer cancelFinish()
	if err := s.finishBrain(finish, task, reply, state); err != nil {
		slog.Error("finish AI task", "task", task.id, "error", err)
	}
}

func (s *Service) watchBrain(ctx context.Context, cancel context.CancelFunc, id string) {
	for ctx.Err() == nil {
		changed := s.store.Changed()
		var state proto.TaskState
		err := s.store.QueryRowContext(ctx, "SELECT state FROM tasks WHERE id=?", id).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !brainActive(state)) {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
		case <-changed:
		}
	}
}

func brainActive(state proto.TaskState) bool {
	return state == proto.Running || state == proto.Paused || state == proto.WaitingUser
}

type brainStateQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkBrainReady(ctx context.Context, q brainStateQuery, id string) error {
	var state proto.TaskState
	var control string
	var ready bool
	err := q.QueryRowContext(ctx, `SELECT t.state,t.control,t.stalled=0 AND d.task_protocol>=2 AND d.conn_state IN ('online','unstable')
		AND EXISTS(SELECT 1 FROM device_sessions x WHERE x.device_id=d.id AND x.conn_gen=d.conn_gen AND x.disconnected_at IS NULL)
		FROM tasks t JOIN devices d ON d.id=t.device_id WHERE t.id=?`, id).Scan(&state, &control, &ready)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !brainActive(state)) {
		return errBrainInactive
	}
	if err != nil {
		return err
	}
	if state != proto.Running || control != "" || !ready {
		return errBrainWait
	}
	return nil
}

func (s *Service) awaitBrain(ctx context.Context, id string) error {
	for ctx.Err() == nil {
		changed := s.store.Changed()
		err := checkBrainReady(ctx, s.store, id)
		if !errors.Is(err, errBrainWait) {
			return err
		}
		select {
		case <-ctx.Done():
		case <-changed:
		}
	}
	return ctx.Err()
}

func (s *Service) deviceAutoApprove(ctx context.Context, device string) bool {
	var auto bool
	_ = s.store.QueryRowContext(ctx, "SELECT auto_approve=1 FROM devices WHERE id=?", device).Scan(&auto)
	return auto
}

func (s *Service) runLoop(ctx context.Context, task brainTask) (string, error) {
	var raw string
	var autoApprove bool
	if err := s.store.QueryRowContext(ctx, "SELECT mcp_tools, auto_approve=1 FROM devices WHERE id=?", task.device).Scan(&raw, &autoApprove); err != nil {
		return "", err
	}
	var advertised []proto.MCPTool
	if err := json.Unmarshal([]byte(raw), &advertised); err != nil {
		return "", err
	}
	tools := CanonicalTools(advertised)
	allowed := make(map[string]proto.MCPTool, len(tools)+1)
	for _, tool := range tools {
		allowed[tool.Name] = tool
	}
	allowed[proto.ToolConfirm] = proto.ConfirmTool
	configs, err := s.config.Read(ctx)
	if err != nil {
		return "", err
	}
	config := configs[settings.LLM]
	modelTools := tools
	if !autoApprove {
		modelTools = append(slicesClone(tools), proto.ConfirmTool)
	}
	session, err := brain.New(config.Endpoint, config.Secret, config.Label, task.prompt, modelTools)
	if err != nil {
		return "", err
	}
	defer session.Clear()
	step, attempts := 0, 0
	var observedWindow toolWindow
	for round := 0; round <= brain.MaxSteps; round++ {
		if err := s.awaitBrain(ctx, task.id); err != nil {
			return "", err
		}
		if err := s.showBrain(ctx, task, step, "等待模型返回"); err != nil && !errors.Is(err, errBrainWait) {
			return "", err
		}
		turn, err := session.Next(ctx)
		if err != nil {
			return "", err
		}
		if len(turn.Calls) == 0 {
			if err := s.awaitBrain(ctx, task.id); err != nil {
				return "", err
			}
			return turn.Text, nil
		}
		if attempts+len(turn.Calls) > brain.MaxSteps {
			break
		}
		attempts += len(turn.Calls)
		observations := make([]brain.Observation, 0, len(turn.Calls))
		for _, call := range turn.Calls {
			tool, ok := allowed[call.Name]
			args, invalid := proto.ValidateTool(call.Name, call.Args)
			if !ok || invalid != nil {
				observations = append(observations, brain.Observation{Text: "工具不在当前设备的允许列表中，或参数无效。", Error: true})
				continue
			}
			if err := s.awaitBrain(ctx, task.id); err != nil {
				return "", err
			}
			autoApprove = s.deviceAutoApprove(ctx, task.device)
			if call.Name == proto.ToolConfirm {
				if autoApprove {
					observations = append(observations, brain.Observation{Text: "当前设备已开启免确认自动执行模式，操作已自动批准，请直接继续执行。"})
					continue
				}
				approved, err := s.requestConfirmation(ctx, task, args.Summary, args.RiskAction, args.AttachmentID)
				if err != nil {
					return "", err
				}
				if !approved {
					return "", errBrainDenied
				}
				observations = append(observations, brain.Observation{Text: "管理员已确认。具体交互操作仍需要与目标和参数匹配。"})
				continue
			}
			label := toolLabel(call.Name)
			windowID := ""
			needsObservedWindow := (call.Name == proto.ToolClick || call.Name == proto.ToolInput) && args.ElementID != nil
			if needsObservedWindow {
				windowID = observedWindow.ID
				if windowID == "" {
					observations = append(observations, brain.Observation{Text: "使用 element_id 操作前请先用 ui_inspect 或 screen_observe 获取目标窗口元素。", Error: true})
					continue
				}
			}
			step++
			result, err := s.callDevice(ctx, task, step, 1, call.Name, call.Args, label, windowID, autoApprove)
			if err != nil {
				return "", err
			}
			if hint := confirmationHint(result.Error); (tool.RiskLevel == "interactive" || tool.RiskLevel == "system") && hint != "" {
				if s.deviceAutoApprove(ctx, task.device) {
					result, err = s.callDevice(ctx, task, step, 2, call.Name, call.Args, label, windowID, true)
					if err != nil {
						return "", err
					}
				} else {
					justification := label + " " + string(call.Args)
					if windowID != "" && observedWindow.Title != "" {
						justification = limitText(observedWindow.Title, 120) + " · " + justification
					}
					approved, err := s.requestConfirmation(ctx, task, hint+"\n"+justification, call.Name, "")
					if err != nil {
						return "", err
					}
					if !approved {
						return "", errBrainDenied
					}
					result, err = s.callDevice(ctx, task, step, 2, call.Name, call.Args, label, windowID, true)
					if err != nil {
						return "", err
					}
				}
			}
			observedWindow = nextToolWindow(observedWindow, call.Name, result)
			observation := brain.Observation{Text: string(result.Data)}
			if result.Error != "" {
				observation.Text, observation.Error = result.Error, true
				if call.Name == proto.ToolBrowserAct && len(result.Data) > 0 {
					observation.Text += "\n" + string(result.Data)
				}
			}
			if result.AttachmentID != "" {
				observation.Text += " 截图已写入聊天，附件 ID：" + result.AttachmentID
				if data, err := s.loadModelImage(ctx, result.AttachmentID); err == nil {
					observation.JPEG = data
				}
			}
			if observation.Text == "" {
				observation.Text = "工具执行完成。"
			}
			observations = append(observations, observation)
		}
		session.Return(turn.Calls, observations)
	}
	return "", fmt.Errorf("AI 任务已达到 %d 次工具调用上限。请核对设备上的实际结果。", brain.MaxSteps)
}

func slicesClone(tools []proto.MCPTool) []proto.MCPTool {
	out := make([]proto.MCPTool, len(tools))
	copy(out, tools)
	return out
}

func confirmationHint(code string) string {
	switch code {
	case "E_CONFIRM_REQUIRED":
		return "请审核这一次操作。"
	case "E_CONFIRM_EDIT_WINDOW_REQUIRED":
		return "允许此任务在当前窗口继续填写普通文本。提交和其他需确认的操作仍会单独询问。"
	default:
		return ""
	}
}

type toolWindow struct {
	ID    string `json:"window_id"`
	Title string `json:"window_title"`
}

func nextToolWindow(before toolWindow, tool string, result proto.MCPResult) toolWindow {
	if result.Error == "E_UI_INSPECT_AGAIN" {
		return toolWindow{}
	}
	if result.Error != "" {
		return before
	}
	switch tool {
	case proto.ToolInspect, proto.ToolObserve, proto.ToolClick, proto.ToolInput, proto.ToolKey, proto.ToolScroll:
		var current toolWindow
		if json.Unmarshal(result.Data, &current) != nil || current.ID == "" || len(current.ID) > 24 {
			return toolWindow{}
		}
		return current
	case proto.ToolStatus, proto.ToolProcesses, proto.ToolProcessManage, proto.ToolWindows, proto.ToolWait,
		proto.ToolShellExec, proto.ToolScriptRun, proto.ToolFSList, proto.ToolFSRead, proto.ToolFSWrite,
		proto.ToolClipboard, proto.ToolPowerScreen:
		return before
	default:
		return toolWindow{}
	}
}

func (s *Service) loadModelImage(ctx context.Context, id string) ([]byte, error) {
	body, _, err := s.Attachment(ctx, id, false)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, proto.MaxScreenshotSize))
}

func limitText(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	return string(runes[:min(n, len(runes))])
}

func toolLabel(tool string) string {
	switch tool {
	case proto.ToolStatus:
		return "读取设备状态"
	case proto.ToolProcesses:
		return "读取进程列表"
	case proto.ToolProcessManage:
		return "管理进程或服务"
	case proto.ToolWindows:
		return "查看桌面窗口"
	case proto.ToolWindowControl:
		return "控制指定窗口"
	case proto.ToolWait:
		return "等待窗口或控件"
	case proto.ToolInspect:
		return "检查前台窗口"
	case proto.ToolObserve:
		return "观察屏幕"
	case proto.ToolClick:
		return "点击控件或坐标"
	case proto.ToolInput:
		return "输入文字"
	case proto.ToolKey:
		return "发送按键"
	case proto.ToolScroll:
		return "滚动或拖拽"
	case proto.ToolLaunch:
		return "打开应用"
	case proto.ToolShellExec:
		return "执行终端命令"
	case proto.ToolScriptRun:
		return "运行脚本"
	case proto.ToolTerminalOpen:
		return "打开终端窗口"
	case proto.ToolFSList:
		return "列出目录文件"
	case proto.ToolFSRead:
		return "读取文件内容"
	case proto.ToolFSWrite:
		return "写入文件内容"
	case proto.ToolClipboard:
		return "读写剪贴板"
	case proto.ToolPowerScreen:
		return "控制电源或屏幕"
	case proto.ToolBrowserManage:
		return "管理浏览器扩展"
	case proto.ToolBrowserTabs:
		return "管理浏览器标签页"
	case proto.ToolBrowserInspect:
		return "读取网页 DOM"
	case proto.ToolBrowserAct:
		return "执行网页操作"
	case proto.ToolBrowserEval:
		return "执行网页 JavaScript"
	default:
		return "调用设备工具"
	}
}
