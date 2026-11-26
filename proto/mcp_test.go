package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeviceToolsCountAndValidation(t *testing.T) {
	if len(DeviceTools) != 26 {
		t.Fatalf("DeviceTools count = %d, want 26", len(DeviceTools))
	}
	if len(DeviceResources) != 5 {
		t.Fatalf("DeviceResources count = %d, want 5", len(DeviceResources))
	}

	cases := []struct {
		name    string
		tool    string
		args    string
		wantErr bool
	}{
		{name: "shell.exec valid", tool: ToolShellExec, args: `{"command":"Get-Process | Select-Object -First 5","shell":"powershell","run_as":"system","timeout_sec":120}`},
		{name: "shell.exec invalid run_as", tool: ToolShellExec, args: `{"command":"dir","run_as":"guest"}`, wantErr: true},
		{name: "script.run valid", tool: ToolScriptRun, args: `{"content":"Write-Output 'ok'","interpreter":"powershell","args":["-v"],"keep_file":false}`},
		{name: "script.run empty", tool: ToolScriptRun, args: `{}`, wantErr: true},
		{name: "terminal.open valid", tool: ToolTerminalOpen, args: `{"shell":"wt","command":"ping 127.0.0.1 -n 3","title":"Ping"}`},
		{name: "process.manage kill", tool: ToolProcessManage, args: `{"action":"kill_tree","pid":1234}`},
		{name: "process.manage service", tool: ToolProcessManage, args: `{"action":"service_status","name":"Spooler"}`},
		{name: "process.manage missing target", tool: ToolProcessManage, args: `{"action":"kill"}`, wantErr: true},
		{name: "fs.list valid", tool: ToolFSList, args: `{"path":"C:\\Windows\\System32","pattern":"*.exe","limit":50}`},
		{name: "fs.read valid", tool: ToolFSRead, args: `{"path":"%USERPROFILE%\\Desktop\\sysinfo.ps1","offset_line":1,"max_lines":200}`},
		{name: "fs.write valid", tool: ToolFSWrite, args: `{"path":"C:\\Temp\\test.txt","content":"hello","mode":"overwrite"}`},
		{name: "clipboard.manage set", tool: ToolClipboard, args: `{"action":"set","text":"hello world"}`},
		{name: "clipboard.manage set missing text", tool: ToolClipboard, args: `{"action":"set"}`, wantErr: true},
		{name: "power_screen valid", tool: ToolPowerScreen, args: `{"action":"keep_awake","duration_sec":600}`},
		{name: "app.launch path and args", tool: ToolLaunch, args: `{"name_or_path":"C:\\Windows\\System32\\cmd.exe","args":"/c echo hi","run_as_admin":true}`},
		{name: "ui.click middle with modifiers", tool: ToolClick, args: `{"x":100,"y":200,"button":"middle","modifiers":["Ctrl","Shift"]}`},
		{name: "ui.input_text long paste", tool: ToolInput, args: `{"text":"` + strings.Repeat("a", 5000) + `","mode":"paste","submit":true}`},
		{name: "ui.press_key repeat", tool: ToolKey, args: `{"keys":["Win","R"],"repeat":1}`},
		{name: "window.control by title", tool: ToolWindowControl, args: `{"title_contains":"任务管理器","action":"focus"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateTool(tc.tool, json.RawMessage(tc.args))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateTool(%s) err = %v, wantErr = %v", tc.tool, err, tc.wantErr)
			}
		})
	}
}
