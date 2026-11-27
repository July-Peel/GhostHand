package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

const BrainTask CapabilityID = "brain.react"

const (
	ToolStatus        = "device.get_status"
	ToolInspect       = "ui.inspect"
	ToolObserve       = "screen.observe"
	ToolClick         = "ui.click"
	ToolInput         = "ui.input_text"
	ToolKey           = "ui.press_key"
	ToolScroll        = "ui.scroll_swipe"
	ToolLaunch        = "app.launch"
	ToolProcesses     = "process.list"
	ToolProcessManage = "process.manage"
	ToolWindows       = "window.list"
	ToolWindowControl = "window.control"
	ToolWait          = "ui.wait_for"
	ToolShellExec     = "shell.exec"
	ToolScriptRun     = "script.run"
	ToolTerminalOpen  = "terminal.open"
	ToolFSList        = "fs.list"
	ToolFSRead        = "fs.read"
	ToolFSWrite       = "fs.write"
	ToolClipboard     = "clipboard.manage"
	ToolPowerScreen   = "device.power_screen"
	ToolConfirm       = "human.confirm"
)

const (
	MaxToolArgsSize = 128 << 10
	MaxToolDataSize = 256 << 10
	maxElementID    = 1<<31 - 1
)

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	RiskLevel   string          `json:"riskLevel"`
}

type MCPResource struct {
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MIMEType string `json:"mimeType"`
}

type TaskDisplay struct {
	TaskID    string    `json:"task_id"`
	Title     string    `json:"title"`
	StepIndex int       `json:"step_index"`
	StepTotal int       `json:"step_total"`
	StepName  string    `json:"step_name"`
	State     TaskState `json:"state"`
}

type MCPCall struct {
	CallID    string          `json:"call_id"`
	TaskID    string          `json:"task_id"`
	Tool      string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
	Display   TaskDisplay     `json:"display"`
	Approved  bool            `json:"approved,omitempty"`
	Window    string          `json:"window_id,omitempty"`
}

type MCPResult struct {
	CallID       string          `json:"call_id"`
	TaskID       string          `json:"task_id"`
	Data         json.RawMessage `json:"data,omitempty"`
	AttachmentID string          `json:"attachment_id,omitempty"`
	Error        string          `json:"error,omitempty"`
}

var DeviceTools = append([]MCPTool{
	{ToolStatus, "读取设备电量、桌面、Core/Helper 权限级别、CPU、内存、磁盘、网络和显示器状态。", toolSchema(`"include_disks":{"type":"boolean"},"include_network":{"type":"boolean"}`, ""), "read_only"},
	{ToolInspect, "首选观察工具：通过 Microsoft UI Automation 提取前台窗口、指定 window_id、桌面(desktop)或任务栏(taskbar)的结构化控件树与文本，并赋予 element_id。", toolSchema(`"max_depth":{"type":"integer","minimum":1,"maximum":12},"interactive_only":{"type":"boolean"},"window_id":{"type":"string","maxLength":24},"filter_text":{"type":"string","maxLength":120}`, ""), "read_only"},
	{ToolObserve, "视觉兜底截图工具：仅在 ui_inspect 无法识别自绘界面或用户明确要求截图时调用，截取屏幕或窗口画面；annotate 可绘制 SoM 编号框。", toolSchema(`"annotate":{"type":"boolean"},"window_only":{"type":"boolean"},"window_id":{"type":"string","maxLength":24},"monitor":{"type":"integer","minimum":0,"maximum":31}`, ""), "read_only"},
	{ToolClick, "按 element_id 或虚拟桌面像素坐标 (x,y) 点击、右击、中击、双击或悬停，支持跨窗口、右键菜单、桌面和任务栏。", toolSchema(`"element_id":{"type":"integer","minimum":1,"maximum":2147483647},"x":{"type":"integer","minimum":-32768,"maximum":32767},"y":{"type":"integer","minimum":-32768,"maximum":32767},"button":{"type":"string","enum":["left","right","middle"]},"double":{"type":"boolean"},"hover_only":{"type":"boolean"},"modifiers":{"type":"array","items":{"type":"string","enum":["Ctrl","Control","Alt","Shift","Win","LWin","RWin"]},"maxItems":4}`, ""), "interactive"},
	{ToolInput, "在指定控件或当前焦点位置输入文本，支持 auto/paste/type 模式、清空后输入与回车提交。", toolSchema(`"element_id":{"type":"integer","minimum":1,"maximum":2147483647},"text":{"type":"string","maxLength":20000},"mode":{"type":"string","enum":["auto","paste","type"]},"clear_first":{"type":"boolean"},"submit":{"type":"boolean"}`, `"text"`), "interactive"},
	{ToolKey, "发送单个按键或组合快捷键（如 Win+R、Ctrl+Shift+Esc、Alt+F4、Enter），支持重复次数。", toolSchema(`"keys":{"type":"array","items":{"type":"string","maxLength":24},"minItems":1,"maxItems":5},"repeat":{"type":"integer","minimum":1,"maximum":50}`, `"keys"`), "interactive"},
	{ToolScroll, "在当前窗口或指定坐标处滚动滚轮，或使用 from_xy 与 to_xy 跨坐标拖拽。", toolSchema(`"direction":{"type":"string","enum":["up","down","left","right"]},"distance":{"type":"integer","minimum":1,"maximum":5000},"x":{"type":"integer","minimum":-32768,"maximum":32767},"y":{"type":"integer","minimum":-32768,"maximum":32767},"from_xy":{"type":"array","items":{"type":"integer"},"minItems":2,"maxItems":2},"to_xy":{"type":"array","items":{"type":"integer"},"minItems":2,"maxItems":2}`, ""), "interactive"},
	{ToolLaunch, "全能应用、系统工具、绝对路径、快捷方式、文件夹与 URI 启动器，支持启动参数和管理员提权。", toolSchema(`"name_or_path":{"type":"string","maxLength":512},"args":{"type":"string","maxLength":2000},"cwd":{"type":"string","maxLength":512},"run_as_admin":{"type":"boolean"}`, `"name_or_path"`), "interactive"},
	{ToolProcesses, "直接读取系统进程列表，支持过滤和按内存、CPU、PID、名称排序，锁屏时可用。", toolSchema(`"filter":{"type":"string","maxLength":120},"sort_by":{"type":"string","enum":["memory","cpu","pid","name"]},"limit":{"type":"integer","minimum":1,"maximum":300}`, ""), "read_only"},
	{ToolProcessManage, "终止进程或进程树，或查询/启停/重启系统服务（Windows 服务 / Linux systemd / Alpine OpenRC）。", toolSchema(`"action":{"type":"string","enum":["kill","kill_tree","service_status","service_start","service_stop","service_restart"]},"pid":{"type":"integer","minimum":1},"name":{"type":"string","maxLength":256}`, `"action"`), "system"},
	{ToolWindows, "枚举当前桌面所有顶层可见窗口（含句柄 ID、标题、进程名、PID、区域和窗口状态）。", toolSchema(`"filter":{"type":"string","maxLength":120},"include_minimized":{"type":"boolean"}`, ""), "read_only"},
	{ToolWindowControl, "激活、最小化、最大化、还原、关闭或调整任意窗口位置大小（可按 window_id、title_contains 或 process_name 指定）。", toolSchema(`"window_id":{"type":"string","maxLength":24},"title_contains":{"type":"string","maxLength":120},"process_name":{"type":"string","maxLength":120},"action":{"type":"string","enum":["focus","minimize","maximize","restore","close","move_resize"]},"bounds":{"type":"array","items":{"type":"integer"},"minItems":4,"maxItems":4}`, `"action"`), "interactive"},
	{ToolWait, "等待指定窗口标题或 UI 控件出现或消失。", toolSchema(`"window_title_contains":{"type":"string","maxLength":120},"text_contains":{"type":"string","maxLength":120},"role":{"type":"string","maxLength":64},"state":{"type":"string","enum":["appear","disappear"]},"timeout_ms":{"type":"integer","minimum":500,"maximum":30000}`, ""), "read_only"},
	{ToolShellExec, "直接执行终端命令并返回 UTF-8 输出（Windows 支持 powershell/pwsh/cmd，Linux 支持 sh/bash/ash）。", toolSchema(`"command":{"type":"string","maxLength":32000},"shell":{"type":"string","enum":["powershell","pwsh","cmd","sh","bash","ash"]},"cwd":{"type":"string","maxLength":512},"timeout_sec":{"type":"integer","minimum":1,"maximum":600},"run_as":{"type":"string","enum":["admin","system"]}`, `"command"`), "system"},
	{ToolScriptRun, "在节点上落盘或执行脚本（PowerShell/Batch/Shell/Python）并返回完整输出。", toolSchema(`"content":{"type":"string","maxLength":100000},"path":{"type":"string","maxLength":512},"interpreter":{"type":"string","enum":["powershell","pwsh","cmd","sh","bash","ash","python","python3","auto"]},"args":{"type":"array","items":{"type":"string","maxLength":1000},"maxItems":64},"cwd":{"type":"string","maxLength":512},"keep_file":{"type":"boolean"},"timeout_sec":{"type":"integer","minimum":1,"maximum":600}`, ""), "system"},
	{ToolTerminalOpen, "在用户可见的交互桌面上打开一个可视化终端窗口（PowerShell/pwsh/Windows Terminal/CMD），可选自动执行初始命令。", toolSchema(`"shell":{"type":"string","enum":["powershell","pwsh","wt","cmd"]},"command":{"type":"string","maxLength":8000},"cwd":{"type":"string","maxLength":512},"title":{"type":"string","maxLength":120}`, ""), "interactive"},
	{ToolFSList, "列出节点任意磁盘路径下的文件与目录清单，支持环境变量展开、通配符过滤和递归遍历，锁屏可用。", toolSchema(`"path":{"type":"string","maxLength":512},"pattern":{"type":"string","maxLength":120},"recursive":{"type":"boolean"},"limit":{"type":"integer","minimum":1,"maximum":500}`, `"path"`), "read_only"},
	{ToolFSRead, "读取节点任意磁盘路径下的文本、配置或脚本文件内容，支持按行偏移分段读取，锁屏可用。", toolSchema(`"path":{"type":"string","maxLength":512},"offset_line":{"type":"integer","minimum":1},"max_lines":{"type":"integer","minimum":1,"maximum":2000}`, `"path"`), "read_only"},
	{ToolFSWrite, "在节点任意磁盘路径下写入、追加文本或脚本文件（自动创建父目录），锁屏可用。", toolSchema(`"path":{"type":"string","maxLength":512},"content":{"type":"string","maxLength":100000},"mode":{"type":"string","enum":["overwrite","append"]}`, `"path","content"`), "system"},
	{ToolClipboard, "读取、设置或清空 Windows 系统剪贴板文本内容。", toolSchema(`"action":{"type":"string","enum":["get","set","clear"]},"text":{"type":"string","maxLength":100000}`, `"action"`), "interactive"},
	{ToolPowerScreen, "控制节点屏幕唤醒、防睡眠保持、息屏或锁屏。", toolSchema(`"action":{"type":"string","enum":["wake","keep_awake","screen_off","lock_workstation"]},"duration_sec":{"type":"integer","minimum":1,"maximum":86400}`, `"action"`), "system"},
}, browserTools...)

var DeviceResources = []MCPResource{
	{URI: "ghosthand://device/status", Name: "设备状态", MIMEType: "application/json"},
	{URI: "ghosthand://device/desktop", Name: "屏幕画面", MIMEType: "image/jpeg"},
	{URI: "ghosthand://device/windows", Name: "当前窗口", MIMEType: "application/json"},
	{URI: "ghosthand://device/processes", Name: "当前进程", MIMEType: "application/json"},
	{URI: "ghosthand://device/clipboard", Name: "系统剪贴板", MIMEType: "text/plain"},
}

var LinuxToolNames = map[string]bool{
	ToolStatus:        true,
	ToolProcesses:     true,
	ToolProcessManage: true,
	ToolShellExec:     true,
	ToolScriptRun:     true,
	ToolFSList:        true,
	ToolFSRead:        true,
	ToolFSWrite:       true,
}

var LinuxTools = func() []MCPTool {
	tools := make([]MCPTool, 0, len(LinuxToolNames))
	for _, t := range DeviceTools {
		if LinuxToolNames[t.Name] {
			tools = append(tools, t)
		}
	}
	return tools
}()

var LinuxResources = []MCPResource{
	{URI: "ghosthand://device/status", Name: "设备状态", MIMEType: "application/json"},
	{URI: "ghosthand://device/processes", Name: "当前进程", MIMEType: "application/json"},
}

var ConfirmTool = MCPTool{ToolConfirm, "向管理员请求确认涉及外部高风险业务影响或人机验证码的操作。",
	toolSchema(`"summary":{"type":"string","maxLength":500},"risk_action":{"type":"string","maxLength":80},"attachment_id":{"type":"string","maxLength":26}`, `"summary","risk_action"`), "guardrail"}

func toolSchema(properties, required string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` + properties + `},"required":[` + required + `],"additionalProperties":false}`)
}

type GUIArgs struct {
	MaxDepth        int      `json:"max_depth,omitempty"`
	InteractiveOnly *bool    `json:"interactive_only,omitempty"`
	Annotate        bool     `json:"annotate,omitempty"`
	WindowOnly      bool     `json:"window_only,omitempty"`
	ElementID       *int     `json:"element_id,omitempty"`
	X               *int     `json:"x,omitempty"`
	Y               *int     `json:"y,omitempty"`
	Button          string   `json:"button,omitempty"`
	Double          bool     `json:"double,omitempty"`
	HoverOnly       bool     `json:"hover_only,omitempty"`
	Modifiers       []string `json:"modifiers,omitempty"`
	Text            *string  `json:"text,omitempty"`
	Mode            string   `json:"mode,omitempty"`
	ClearFirst      bool     `json:"clear_first,omitempty"`
	Submit          bool     `json:"submit,omitempty"`
	Keys            []string `json:"keys,omitempty"`
	Repeat          int      `json:"repeat,omitempty"`
	Direction       string   `json:"direction,omitempty"`
	Distance        int      `json:"distance,omitempty"`
	FromXY          []int    `json:"from_xy,omitempty"`
	ToXY            []int    `json:"to_xy,omitempty"`
	App             string   `json:"name_or_path,omitempty"`
	LaunchArgs      string   `json:"args,omitempty"`
	ArgsList        []string `json:"-"`
	Cwd             string   `json:"cwd,omitempty"`
	RunAsAdmin      bool     `json:"run_as_admin,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	RiskAction      string   `json:"risk_action,omitempty"`
	AttachmentID    string   `json:"attachment_id,omitempty"`
	WindowID        string   `json:"window_id,omitempty"`
	TitleContains   string   `json:"title_contains,omitempty"`
	ProcessName     string   `json:"process_name,omitempty"`
	Monitor         *int     `json:"monitor,omitempty"`
	FilterText      string   `json:"filter_text,omitempty"`
	Filter          string   `json:"filter,omitempty"`
	SortBy          string   `json:"sort_by,omitempty"`
	Limit           int      `json:"limit,omitempty"`
	Minimized       *bool    `json:"include_minimized,omitempty"`
	Action          string   `json:"action,omitempty"`
	Bounds          []int    `json:"bounds,omitempty"`
	WindowTitle     string   `json:"window_title_contains,omitempty"`
	TextContains    string   `json:"text_contains,omitempty"`
	Role            string   `json:"role,omitempty"`
	State           string   `json:"state,omitempty"`
	TimeoutMs       int      `json:"timeout_ms,omitempty"`
	IncludeDisks    *bool    `json:"include_disks,omitempty"`
	IncludeNetwork  *bool    `json:"include_network,omitempty"`
	PID             int      `json:"pid,omitempty"`
	Name            string   `json:"name,omitempty"`
	Command         string   `json:"command,omitempty"`
	Shell           string   `json:"shell,omitempty"`
	TimeoutSec      int      `json:"timeout_sec,omitempty"`
	RunAs           string   `json:"run_as,omitempty"`
	Content         *string  `json:"content,omitempty"`
	Path            string   `json:"path,omitempty"`
	Interpreter     string   `json:"interpreter,omitempty"`
	KeepFile        *bool    `json:"keep_file,omitempty"`
	Title           string   `json:"title,omitempty"`
	Pattern         string   `json:"pattern,omitempty"`
	Recursive       bool     `json:"recursive,omitempty"`
	OffsetLine      int      `json:"offset_line,omitempty"`
	MaxLines        int      `json:"max_lines,omitempty"`
	DurationSec     int      `json:"duration_sec,omitempty"`
}

var (
	windowIDPattern  = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	ErrToolArguments = errors.New("工具参数无效")
)

var keysFor = map[string]string{
	ToolStatus:        "include_disks include_network",
	ToolInspect:       "max_depth interactive_only window_id filter_text",
	ToolObserve:       "annotate window_only window_id monitor",
	ToolClick:         "element_id x y button double hover_only modifiers",
	ToolInput:         "element_id text mode clear_first submit",
	ToolKey:           "keys repeat",
	ToolScroll:        "direction distance x y from_xy to_xy",
	ToolLaunch:        "name_or_path args cwd run_as_admin",
	ToolConfirm:       "summary risk_action attachment_id",
	ToolProcesses:     "filter sort_by limit",
	ToolProcessManage: "action pid name",
	ToolWindows:       "filter include_minimized",
	ToolWindowControl: "window_id title_contains process_name action bounds",
	ToolWait:          "window_title_contains text_contains role state timeout_ms",
	ToolShellExec:     "command shell cwd timeout_sec run_as",
	ToolScriptRun:     "content path interpreter args cwd keep_file timeout_sec",
	ToolTerminalOpen:  "shell command cwd title",
	ToolFSList:        "path pattern recursive limit",
	ToolFSRead:        "path offset_line max_lines",
	ToolFSWrite:       "path content mode",
	ToolClipboard:     "action text",
	ToolPowerScreen:   "action duration_sec",
}

func ValidateTool(name string, data json.RawMessage) (GUIArgs, error) {
	var args GUIArgs
	if IsBrowserTool(name) {
		_, err := ValidateBrowserTool(name, data)
		return args, err
	}
	allowed, found := keysFor[name]
	var fields map[string]json.RawMessage
	if !found || len(data) > MaxToolArgsSize || json.Unmarshal(data, &fields) != nil || fields == nil {
		return args, ErrToolArguments
	}
	for key, value := range fields {
		if !strings.Contains(" "+allowed+" ", " "+key+" ") || key == "" || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return args, ErrToolArguments
		}
	}
	if name == ToolScriptRun {
		if rawArgs, ok := fields["args"]; ok {
			if json.Unmarshal(rawArgs, &args.ArgsList) != nil {
				return args, ErrToolArguments
			}
			delete(fields, "args")
			var err error
			data, err = json.Marshal(fields)
			if err != nil {
				return args, ErrToolArguments
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&args) != nil || decoder.Decode(new(any)) != io.EOF ||
		(args.ElementID != nil && (*args.ElementID < 1 || *args.ElementID > maxElementID)) ||
		(args.X != nil && !validCoordinate(*args.X)) || (args.Y != nil && !validCoordinate(*args.Y)) {
		return args, ErrToolArguments
	}
	var valid bool
	switch name {
	case ToolStatus:
		valid = true
	case ToolObserve:
		valid = len(args.WindowID) <= 24 && (args.Monitor == nil || *args.Monitor >= 0 && *args.Monitor <= 31)
	case ToolInspect:
		valid = (fields["max_depth"] == nil || args.MaxDepth >= 1 && args.MaxDepth <= 12) && len(args.WindowID) <= 24 && len([]rune(args.FilterText)) <= 120
	case ToolProcesses:
		valid = len([]rune(args.Filter)) <= 120 && (fields["limit"] == nil || args.Limit >= 1 && args.Limit <= 300) &&
			(fields["sort_by"] == nil || args.SortBy == "memory" || args.SortBy == "cpu" || args.SortBy == "pid" || args.SortBy == "name")
	case ToolProcessManage:
		switch args.Action {
		case "kill", "kill_tree":
			valid = (args.PID > 0 || strings.TrimSpace(args.Name) != "") && len([]rune(args.Name)) <= 256 && !strings.ContainsRune(args.Name, 0)
		case "service_status", "service_start", "service_stop", "service_restart":
			valid = strings.TrimSpace(args.Name) != "" && len([]rune(args.Name)) <= 256 && !strings.ContainsRune(args.Name, 0)
		}
	case ToolWindows:
		valid = len([]rune(args.Filter)) <= 120
	case ToolWindowControl:
		hasTarget := windowIDPattern.MatchString(args.WindowID) || strings.TrimSpace(args.TitleContains) != "" || strings.TrimSpace(args.ProcessName) != ""
		valid = hasTarget && len([]rune(args.TitleContains)) <= 120 && len([]rune(args.ProcessName)) <= 120
		switch args.Action {
		case "focus", "minimize", "maximize", "restore", "close":
			valid = valid && fields["bounds"] == nil
		case "move_resize":
			valid = valid && len(args.Bounds) == 4 && validCoordinate(args.Bounds[0]) && validCoordinate(args.Bounds[1]) &&
				args.Bounds[2] > 0 && args.Bounds[2] <= 32767 && args.Bounds[3] > 0 && args.Bounds[3] <= 32767
		default:
			valid = false
		}
	case ToolWait:
		valid = strings.TrimSpace(args.WindowTitle+args.TextContains+args.Role) != "" && len([]rune(args.WindowTitle)) <= 120 &&
			len([]rune(args.TextContains)) <= 120 && len([]rune(args.Role)) <= 64 &&
			(fields["state"] == nil || args.State == "appear" || args.State == "disappear") &&
			(fields["timeout_ms"] == nil || args.TimeoutMs >= 500 && args.TimeoutMs <= 30000)
	case ToolClick:
		target := args.ElementID != nil && args.X == nil && args.Y == nil ||
			args.ElementID == nil && args.X != nil && args.Y != nil
		valid = target && (args.Button == "" || args.Button == "left" || args.Button == "right" || args.Button == "middle") &&
			len(args.Modifiers) <= 4
		for _, m := range args.Modifiers {
			switch strings.ToLower(m) {
			case "ctrl", "control", "alt", "shift", "win", "lwin", "rwin":
			default:
				valid = false
			}
		}
	case ToolInput:
		valid = args.Text != nil && len([]rune(*args.Text)) <= 20000 && !strings.ContainsRune(*args.Text, 0) &&
			(args.Mode == "" || args.Mode == "auto" || args.Mode == "paste" || args.Mode == "type")
	case ToolKey:
		valid = len(args.Keys) >= 1 && len(args.Keys) <= 5 && (fields["repeat"] == nil || args.Repeat >= 1 && args.Repeat <= 50)
		for _, key := range args.Keys {
			valid = valid && len(key) >= 1 && len(key) <= 24
		}
	case ToolScroll:
		if len(args.FromXY) != 0 || len(args.ToXY) != 0 {
			valid = len(args.FromXY) == 2 && len(args.ToXY) == 2
			for _, point := range [][]int{args.FromXY, args.ToXY} {
				for _, coordinate := range point {
					valid = valid && validCoordinate(coordinate)
				}
			}
		} else {
			valid = (args.Direction == "up" || args.Direction == "down" || args.Direction == "left" || args.Direction == "right") &&
				args.Distance >= 0 && args.Distance <= 5000 &&
				((args.X == nil && args.Y == nil) || (args.X != nil && args.Y != nil))
		}
	case ToolLaunch:
		app := strings.TrimSpace(args.App)
		valid = app != "" && len([]rune(args.App)) <= 512 && !strings.ContainsAny(args.App, "\r\n\x00") &&
			len([]rune(args.LaunchArgs)) <= 2000 && !strings.ContainsRune(args.LaunchArgs, 0) &&
			len([]rune(args.Cwd)) <= 512 && !strings.ContainsAny(args.Cwd, "\r\n\x00")
	case ToolShellExec:
		valid = strings.TrimSpace(args.Command) != "" && len([]rune(args.Command)) <= 32000 && !strings.ContainsRune(args.Command, 0) &&
			(args.Shell == "" || args.Shell == "powershell" || args.Shell == "pwsh" || args.Shell == "cmd" || args.Shell == "sh" || args.Shell == "bash" || args.Shell == "ash") &&
			len([]rune(args.Cwd)) <= 512 && !strings.ContainsAny(args.Cwd, "\r\n\x00") &&
			(fields["timeout_sec"] == nil || args.TimeoutSec >= 1 && args.TimeoutSec <= 600) &&
			(args.RunAs == "" || args.RunAs == "admin" || args.RunAs == "system")
	case ToolScriptRun:
		hasContent := args.Content != nil && strings.TrimSpace(*args.Content) != ""
		hasPath := strings.TrimSpace(args.Path) != ""
		valid = (hasContent || hasPath) &&
			(args.Content == nil || (len([]rune(*args.Content)) <= 100000 && !strings.ContainsRune(*args.Content, 0))) &&
			len([]rune(args.Path)) <= 512 && !strings.ContainsAny(args.Path, "\r\n\x00") &&
			(args.Interpreter == "" || args.Interpreter == "powershell" || args.Interpreter == "pwsh" || args.Interpreter == "cmd" ||
				args.Interpreter == "sh" || args.Interpreter == "bash" || args.Interpreter == "ash" ||
				args.Interpreter == "python" || args.Interpreter == "python3" || args.Interpreter == "auto") &&
			len(args.ArgsList) <= 64 &&
			len([]rune(args.Cwd)) <= 512 && !strings.ContainsAny(args.Cwd, "\r\n\x00") &&
			(fields["timeout_sec"] == nil || args.TimeoutSec >= 1 && args.TimeoutSec <= 600)
		for _, a := range args.ArgsList {
			if len([]rune(a)) > 1000 || strings.ContainsRune(a, 0) {
				valid = false
			}
		}
	case ToolTerminalOpen:
		valid = (args.Shell == "" || args.Shell == "powershell" || args.Shell == "pwsh" || args.Shell == "wt" || args.Shell == "cmd") &&
			len([]rune(args.Command)) <= 8000 && !strings.ContainsRune(args.Command, 0) &&
			len([]rune(args.Cwd)) <= 512 && !strings.ContainsAny(args.Cwd, "\r\n\x00") &&
			len([]rune(args.Title)) <= 120 && !strings.ContainsAny(args.Title, "\r\n\x00")
	case ToolFSList:
		valid = strings.TrimSpace(args.Path) != "" && len([]rune(args.Path)) <= 512 && !strings.ContainsAny(args.Path, "\r\n\x00") &&
			len([]rune(args.Pattern)) <= 120 && !strings.ContainsAny(args.Pattern, "\r\n\x00") &&
			(fields["limit"] == nil || args.Limit >= 1 && args.Limit <= 500)
	case ToolFSRead:
		valid = strings.TrimSpace(args.Path) != "" && len([]rune(args.Path)) <= 512 && !strings.ContainsAny(args.Path, "\r\n\x00") &&
			(fields["offset_line"] == nil || args.OffsetLine >= 1) &&
			(fields["max_lines"] == nil || args.MaxLines >= 1 && args.MaxLines <= 2000)
	case ToolFSWrite:
		valid = strings.TrimSpace(args.Path) != "" && len([]rune(args.Path)) <= 512 && !strings.ContainsAny(args.Path, "\r\n\x00") &&
			args.Content != nil && len([]rune(*args.Content)) <= 100000 && !strings.ContainsRune(*args.Content, 0) &&
			(args.Mode == "" || args.Mode == "overwrite" || args.Mode == "append")
	case ToolClipboard:
		switch args.Action {
		case "get", "clear":
			valid = true
		case "set":
			valid = args.Text != nil && len([]rune(*args.Text)) <= 100000 && !strings.ContainsRune(*args.Text, 0)
		}
	case ToolPowerScreen:
		valid = (args.Action == "wake" || args.Action == "keep_awake" || args.Action == "screen_off" || args.Action == "lock_workstation") &&
			(fields["duration_sec"] == nil || args.DurationSec >= 1 && args.DurationSec <= 86400)
	case ToolConfirm:
		valid = len(args.Summary) > 0 && len(args.Summary) <= 1500 && len(args.RiskAction) > 0 && len(args.RiskAction) <= 240 && len(args.AttachmentID) <= 26
	}
	if !valid {
		return args, ErrToolArguments
	}
	return args, nil
}

func validCoordinate(value int) bool { return value >= -32768 && value <= 32767 }
