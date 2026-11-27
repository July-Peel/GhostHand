package workbench

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ghosthand/backend/internal/brain"
	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

func TestMentionedDevicesAndInstructions(t *testing.T) {
	devices := []MCPDevice{
		{Device: Device{ID: FreeChatDeviceID, Name: "随意闲聊", Platform: "chat"}},
		{Device: Device{ID: "demo1", Name: "模拟机", Demo: true}},
		{Device: Device{ID: "dev66", Name: "66开发机", Platform: proto.Windows, OSVersion: "Windows 10", ConnState: proto.Online, DesktopState: "unlocked"}, MCPReady: true},
		{Device: Device{ID: "dev66bak", Name: "66开发机-备份", Platform: proto.Windows, OSVersion: "Windows 10", ConnState: proto.Online, DesktopState: "unlocked"}, MCPReady: true},
		{Device: Device{ID: "deves", Name: "西班牙", Platform: proto.Windows, OSVersion: "Windows 11", ConnState: proto.Offline, DesktopState: "locked"}, MCPReady: false},
		{Device: Device{ID: "dup1", Name: "重名机", Platform: proto.Windows, ConnState: proto.Online}, MCPReady: true},
		{Device: Device{ID: "dup2", Name: "重名机", Platform: proto.Windows, ConnState: proto.Online}, MCPReady: true},
	}

	// Longest match first, Chinese prefix without space, deduplication, and excluding __chat__ / demo / email.
	got := mentionedDevices(devices, FreeChatDeviceID, "请让@66开发机-备份 和@66开发机 还有@西班牙 测速，忽略 @随意闲聊 @模拟机 admin@66开发机.com @重名机 @dup2 @西班牙")
	wantIDs := []string{"dev66bak", "dev66", "deves", "dup2"}
	if len(got) != len(wantIDs) {
		t.Fatalf("mentionedDevices len = %d (%+v), want %v", len(got), got, wantIDs)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Fatalf("mentionedDevices[%d] = %q, want %q", i, got[i].ID, want)
		}
	}

	// Single-device chat excludes self even if user explicitly @self.
	single := mentionedDevices(devices, "dev66", "@66开发机 @西班牙 互测")
	if len(single) != 1 || single[0].ID != "deves" {
		t.Fatalf("single-device mentions = %+v, want only deves", single)
	}

	// Verify instructions for single-device with remote offline device.
	inst := mentionInstructions("dev66", "66开发机", single)
	for _, want := range []string{
		`当前聊天的本机名称是 "66开发机"，device_id 是 "dev66"`,
		`名称 "西班牙" | device_id "deves"`,
		`conn_state "offline"`,
		`mcp_ready false`,
		"【多设备协同执行准则】",
		"iperf3",
	} {
		if !strings.Contains(inst, want) {
			t.Fatalf("mentionInstructions missing %q:\n%s", want, inst)
		}
	}

	// Unrecognized @ in single-device chat warns not to substitute local device and omits multi-device guidelines.
	unrecognized := mentionInstructions("dev66", "66开发机", nil)
	if !strings.Contains(unrecognized, "不能以本机代替") || strings.Contains(unrecognized, "【多设备协同执行准则】") {
		t.Fatalf("unexpected unrecognized instructions:\n%s", unrecognized)
	}

	// Free chat with single mentioned device does not need multi-device guidelines, with 2 devices it does.
	if strings.Contains(mentionInstructions(FreeChatDeviceID, "", single), "【多设备协同执行准则】") {
		t.Fatal("free chat with 1 mentioned device should not inject multi-device guidelines")
	}
	if !strings.Contains(mentionInstructions(FreeChatDeviceID, "", got[:2]), "【多设备协同执行准则】") {
		t.Fatal("free chat with 2 mentioned devices should inject multi-device guidelines")
	}
}

func TestMCPArgumentsOptionalDefault(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		defaultDevice string
		wantDevice    string
		wantErr       bool
	}{
		{name: "omitted with default", raw: `{"command":"ipconfig"}`, defaultDevice: "dev66", wantDevice: "dev66"},
		{name: "empty string with default", raw: `{"device_id":"","command":"ipconfig"}`, defaultDevice: "dev66", wantDevice: "dev66"},
		{name: "null with default", raw: `{"device_id":null,"command":"ipconfig"}`, defaultDevice: "dev66", wantDevice: "dev66"},
		{name: "explicit target overrides default", raw: `{"device_id":"西班牙","command":"ipconfig"}`, defaultDevice: "dev66", wantDevice: "西班牙"},
		{name: "empty string without default fails", raw: `{"device_id":""}`, defaultDevice: "", wantErr: true},
		{name: "null without default fails", raw: `{"device_id":null}`, defaultDevice: "", wantErr: true},
		{name: "non-string fails", raw: `{"device_id":123}`, defaultDevice: "dev66", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			device, rest, err := MCPArguments(json.RawMessage(tc.raw), tc.defaultDevice)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got device=%q rest=%s", device, rest)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if device != tc.wantDevice {
				t.Fatalf("device = %q, want %q", device, tc.wantDevice)
			}
			var m map[string]any
			if err := json.Unmarshal(rest, &m); err != nil {
				t.Fatalf("unmarshal rest: %v", err)
			}
			if _, has := m["device_id"]; has {
				t.Fatalf("device_id should be stripped from args, got %s", rest)
			}
		})
	}
}

func TestPlanChatCalls(t *testing.T) {
	s, ctx := newMentionTestWorkbench(t)
	mentioned := []MCPDevice{{Device: Device{ID: "deves", Name: "西班牙"}}}

	// Single-device without mentions: always local, devices_list rejected.
	plain := s.planChatCalls(ctx, brainTask{device: "dev66", source: "brain"}, []brain.Call{
		{ID: "c1", Name: proto.ToolStatus, Args: json.RawMessage(`{}`)},
		{ID: "c2", Name: MCPDevicesTool.Name, Args: json.RawMessage(`{}`)},
	}, nil)
	if plain[0].problem != "" || plain[0].device != "dev66" {
		t.Fatalf("plain local call = %+v", plain[0])
	}
	if plain[1].problem == "" {
		t.Fatalf("expected devices_list rejected without mentions, got %+v", plain[1])
	}

	// Single-device with @西班牙: allows local (omitted, null, by name) and mentioned deves; rejects unmentioned devjp.
	cross := s.planChatCalls(ctx, brainTask{device: "dev66", source: "brain"}, []brain.Call{
		{ID: "c1", Name: proto.ToolStatus, Args: json.RawMessage(`{}`)},
		{ID: "c2", Name: proto.ToolStatus, Args: json.RawMessage(`{"device_id":""}`)},
		{ID: "c3", Name: proto.ToolStatus, Args: json.RawMessage(`{"device_id":"66开发机"}`)},
		{ID: "c4", Name: proto.ToolStatus, Args: json.RawMessage(`{"device_id":"西班牙"}`)},
		{ID: "c5", Name: proto.ToolStatus, Args: json.RawMessage(`{"device_id":"东京"}`)},
		{ID: "c6", Name: MCPDevicesTool.Name, Args: json.RawMessage(`{}`)},
	}, mentioned)
	if cross[0].problem != "" || cross[0].device != "dev66" {
		t.Fatalf("cross omitted = %+v", cross[0])
	}
	if cross[1].problem != "" || cross[1].device != "dev66" {
		t.Fatalf("cross empty = %+v", cross[1])
	}
	if cross[2].problem != "" || cross[2].device != "dev66" {
		t.Fatalf("cross self by name = %+v", cross[2])
	}
	if cross[3].problem != "" || cross[3].device != "deves" {
		t.Fatalf("cross mentioned = %+v", cross[3])
	}
	if !strings.Contains(cross[4].problem, "仅限于本条消息") {
		t.Fatalf("expected unmentioned device rejected, got %+v", cross[4])
	}
	if cross[5].problem != "" {
		t.Fatalf("expected devices_list allowed in cross-device mode, got %+v", cross[5])
	}

	// Free chat: rejects __chat__ for device tools, allows human_confirm without device_id, allows physical devices.
	free := s.planChatCalls(ctx, brainTask{device: FreeChatDeviceID, source: "chat"}, []brain.Call{
		{ID: "f1", Name: proto.ToolStatus, Args: json.RawMessage(`{"device_id":"__chat__"}`)},
		{ID: "f2", Name: proto.ToolStatus, Args: json.RawMessage(`{"device_id":"西班牙"}`)},
		{ID: "f3", Name: proto.ToolConfirm, Args: json.RawMessage(`{"summary":"ok","risk_action":"test"}`)},
		{ID: "f4", Name: proto.ToolConfirm, Args: json.RawMessage(`{"device_id":"西班牙","summary":"ok","risk_action":"test"}`)},
	}, mentioned)
	if !strings.Contains(free[0].problem, "物理设备") {
		t.Fatalf("expected __chat__ rejected, got %+v", free[0])
	}
	if free[1].problem != "" || free[1].device != "deves" {
		t.Fatalf("free physical call = %+v", free[1])
	}
	if free[2].problem != "" || free[2].device != FreeChatDeviceID {
		t.Fatalf("free confirm default = %+v", free[2])
	}
	if free[3].problem != "" || free[3].device != "deves" {
		t.Fatalf("free confirm targeted = %+v", free[3])
	}
}

func TestRunChatCallsParallelAndOrdering(t *testing.T) {
	ctx := t.Context()
	calls := []chatToolCall{
		{name: proto.ToolStatus, device: "devA", args: json.RawMessage(`{"step":"A1"}`)},
		{name: proto.ToolStatus, device: "devB", args: json.RawMessage(`{"step":"B1"}`)},
		{name: proto.ToolStatus, device: "devA", args: json.RawMessage(`{"step":"A2"}`)},
		{name: proto.ToolStatus, device: "devB", args: json.RawMessage(`{"step":"B2"}`)},
	}

	// Barrier proves devA#1 and devB#1 run concurrently; per-device slices prove in-device order.
	startedA := make(chan struct{})
	startedB := make(chan struct{})
	var mu sync.Mutex
	order := map[string][]string{}

	obs, err := runChatCalls(ctx, calls, func(ctx context.Context, call chatToolCall) (brain.Observation, error) {
		var payload struct{ Step string }
		_ = json.Unmarshal(call.args, &payload)
		switch payload.Step {
		case "A1":
			close(startedA)
			select {
			case <-startedB:
			case <-time.After(2 * time.Second):
				return brain.Observation{}, errors.New("expected devA and devB to run in parallel")
			}
		case "B1":
			close(startedB)
			select {
			case <-startedA:
			case <-time.After(2 * time.Second):
				return brain.Observation{}, errors.New("expected devB and devA to run in parallel")
			}
		}
		mu.Lock()
		order[call.device] = append(order[call.device], payload.Step)
		mu.Unlock()
		return brain.Observation{Text: payload.Step}, nil
	})
	if err != nil {
		t.Fatalf("runChatCalls parallel: %v", err)
	}
	wantObs := []string{"A1", "B1", "A2", "B2"}
	for i, want := range wantObs {
		if obs[i].Text != want {
			t.Fatalf("obs[%d] = %q, want %q", i, obs[i].Text, want)
		}
	}
	if strings.Join(order["devA"], ",") != "A1,A2" || strings.Join(order["devB"], ",") != "B1,B2" {
		t.Fatalf("per-device ordering broken: %+v", order)
	}

	// Confirm call forces serial execution even across multiple devices.
	var active atomic.Int32
	var maxConcurrent atomic.Int32
	serialCalls := []chatToolCall{
		{name: proto.ToolStatus, device: "devA"},
		{name: proto.ToolConfirm, device: "devB"},
		{name: proto.ToolStatus, device: "devB"},
	}
	_, err = runChatCalls(ctx, serialCalls, func(ctx context.Context, call chatToolCall) (brain.Observation, error) {
		cur := active.Add(1)
		for {
			prev := maxConcurrent.Load()
			if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		active.Add(-1)
		return brain.Observation{Text: call.name}, nil
	})
	if err != nil {
		t.Fatalf("runChatCalls serial: %v", err)
	}
	if maxConcurrent.Load() != 1 {
		t.Fatalf("maxConcurrent with human_confirm = %d, want 1", maxConcurrent.Load())
	}
}

func TestCrossDeviceConfirmationAndAutoApprove(t *testing.T) {
	s, ctx := newMentionTestWorkbench(t)
	taskID := "t_11111111111111111111111111111111"
	if err := s.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO tasks
			(id,device_id,created_by,capability,state,created_at,expires_at,lane,source,title)
			VALUES (?,'dev66',1,'brain.react','running',1,9999999999999,'ui','brain','跨机确认测试')`, taskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	task := brainTask{id: taskID, device: "dev66", title: "跨机确认测试", source: "brain", user: 1}
	done := make(chan struct {
		approved bool
		err      error
	}, 1)
	go func() {
		approved, err := s.requestConfirmation(ctx, task, "deves", "西班牙", "请审核这一次操作\n执行终端命令", proto.ToolShellExec, "")
		done <- struct {
			approved bool
			err      error
		}{approved, err}
	}()

	// Wait until confirmation card is written to dev66 with target_device_id = deves.
	deadline := time.Now().Add(3 * time.Second)
	var targetID sql.NullString
	var cardPayload string
	for time.Now().Before(deadline) {
		err := s.store.QueryRowContext(ctx, `SELECT c.target_device_id, m.payload_json
			FROM confirmations c JOIN chat_messages m ON m.task_id=c.task_id AND m.type='confirm_card'
			WHERE c.task_id=?`, taskID).Scan(&targetID, &cardPayload)
		if err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !targetID.Valid || targetID.String != "deves" {
		t.Fatalf("target_device_id = %+v, want deves", targetID)
	}
	var card ConfirmationCard
	if err := json.Unmarshal([]byte(cardPayload), &card); err != nil || card.DeviceName != "西班牙" {
		t.Fatalf("confirm card = %+v (raw=%s), want DeviceName=西班牙", card, cardPayload)
	}

	// Enabling auto_approve on target device deves approves the pending cross-device confirmation and wakes dev66's task.
	if err := s.SetAutoApprove(ctx, 1, "deves", true); err != nil {
		t.Fatalf("SetAutoApprove(deves): %v", err)
	}
	select {
	case res := <-done:
		if res.err != nil || !res.approved {
			t.Fatalf("requestConfirmation = (%v, %v), want (true, nil)", res.approved, res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cross-device confirmation approval")
	}

	// Subsequent requestConfirmation targeting deves auto-approves immediately without inserting another card.
	approved, err := s.requestConfirmation(ctx, task, "deves", "西班牙", "second check", proto.ToolShellExec, "")
	if err != nil || !approved {
		t.Fatalf("second requestConfirmation = (%v, %v), want (true, nil)", approved, err)
	}
	var count int
	if err := s.store.QueryRowContext(ctx, "SELECT COUNT(*) FROM confirmations WHERE task_id=?", taskID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("confirmations count = %d, want 1", count)
	}
}

func TestSingleDeviceCrossDeviceLoopAndScreenshotReturn(t *testing.T) {
	s, ctx := newMentionTestWorkbench(t)
	if err := s.SetAutoApprove(ctx, 1, "deves", true); err != nil {
		t.Fatal(err)
	}

	var modelTurn atomic.Int32
	var seenDevContext, seenRemoteLabel atomic.Bool
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		turn := modelTurn.Add(1)
		switch turn {
		case 1:
			raw := string(body)
			if strings.Contains(raw, `当前聊天的本机名称是 \"66开发机\"`) &&
				strings.Contains(raw, `device_id \"deves\"`) &&
				strings.Contains(raw, "【多设备协同执行准则】") {
				seenDevContext.Store(true)
			}
			_, _ = io.WriteString(w, `{"output":[
				{"type":"function_call","call_id":"call_local","name":"device_status","arguments":"{}"},
				{"type":"function_call","call_id":"call_remote","name":"screen_observe","arguments":"{\"device_id\":\"西班牙\"}"}
			]}`)
		case 2:
			if strings.Contains(string(body), "[西班牙] 观察屏幕") && strings.Contains(string(body), "data:image/jpeg;base64,") {
				seenRemoteLabel.Store(true)
			}
			_, _ = io.WriteString(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"已完成 66开发机 与 西班牙 的协同检查。"}]}]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer model.Close()

	if err := s.config.Save(ctx, 1, settings.LLM, settings.Update{Endpoint: model.URL, Label: "test-model"}); err != nil {
		t.Fatal(err)
	}

	runCtx, stop := context.WithCancel(ctx)
	var bg sync.WaitGroup
	bg.Add(3)
	go func() { defer bg.Done(); s.RunTasks(runCtx) }()
	go func() { defer bg.Done(); s.RunBrain(runCtx) }()
	// Simulated Windows nodes (dev66 & deves) processing outbox frames.
	go func() {
		defer bg.Done()
		sampleJPEG := testSampleJPEG(t)
		processed := map[string]int64{}
		for runCtx.Err() == nil {
			rows, err := s.store.QueryContext(runCtx, `SELECT device_id, seq, envelope_json FROM outbox ORDER BY seq`)
			if err != nil {
				return
			}
			type item struct {
				device string
				seq    int64
				raw    string
			}
			var items []item
			for rows.Next() {
				var it item
				if rows.Scan(&it.device, &it.seq, &it.raw) == nil && it.seq > processed[it.device] {
					items = append(items, it)
				}
			}
			rows.Close()
			for _, it := range items {
				processed[it.device] = it.seq
				var env proto.Envelope
				if json.Unmarshal([]byte(it.raw), &env) != nil {
					continue
				}
				switch env.Type {
				case proto.TaskAssign:
					var assign proto.TaskAssignment
					_ = json.Unmarshal(env.Payload, &assign)
					accept, _ := proto.NewEnvelope(proto.TaskAccepted, proto.TaskRef{TaskID: assign.TaskID})
					_ = s.store.Write(runCtx, func(tx *sql.Tx) error {
						return ApplyTaskReport(runCtx, tx, it.device, accept, time.Now().UnixMilli())
					})
				case proto.MCPToolCall:
					var call proto.MCPCall
					_ = json.Unmarshal(env.Payload, &call)
					res := proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Data: json.RawMessage(`{"ok":true,"host":"` + it.device + `"}`)}
					if call.Tool == proto.ToolObserve {
						attID, err := proto.NewID()
						if err == nil {
							now := time.Now()
							img, err := s.saveUserAttachment(runCtx, UploadImage{Name: "screen.jpg", Data: sampleJPEG}, settings.Configuration{}, now)
							if err == nil {
								_ = s.store.Write(runCtx, func(tx *sql.Tx) error {
									if err := img.insert(runCtx, tx, call.TaskID, call.CallID, now); err != nil {
										return err
									}
									attID = img.ID
									return nil
								})
								res.AttachmentID = attID
							}
						}
					}
					frame, _ := proto.NewEnvelope(proto.MCPToolResult, res)
					_ = s.store.Write(runCtx, func(tx *sql.Tx) error {
						if err := ApplyTaskReport(runCtx, tx, it.device, frame, time.Now().UnixMilli()); err != nil {
							return err
						}
						return RecordChange(runCtx, tx, it.device)
					})
				}
			}
			select {
			case <-runCtx.Done():
			case <-time.After(15 * time.Millisecond):
			}
		}
	}()
	defer func() {
		stop()
		bg.Wait()
	}()

	if err := s.Send(ctx, 1, "dev66", "请帮我查本机状态并让@西班牙 截屏", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	var finalReply string
	for time.Now().Before(deadline) {
		page, err := s.Messages(ctx, "dev66", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Messages {
			if m.Sender == "agent" && m.Type == "text" {
				finalReply = m.Text
			}
		}
		if finalReply != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if finalReply != "已完成 66开发机 与 西班牙 的协同检查。" {
		t.Fatalf("finalReply = %q", finalReply)
	}
	if !seenDevContext.Load() {
		t.Fatal("model request did not contain expected mention context and multi-device guidelines")
	}
	if !seenRemoteLabel.Load() {
		t.Fatal("second model request did not contain labeled remote screenshot")
	}

	page, err := s.Messages(ctx, "dev66", 0)
	if err != nil {
		t.Fatal(err)
	}
	var foundRemoteStep, foundRemoteMedia bool
	for _, m := range page.Messages {
		switch m.Type {
		case "task_card":
			var card TaskCard
			if err := json.Unmarshal(m.Payload, &card); err != nil {
				t.Fatal(err)
			}
			var timeline []struct {
				StepKey  string `json:"step_key"`
				StepName string `json:"step_name"`
				Status   string `json:"status"`
			}
			if err := json.Unmarshal(card.Timeline, &timeline); err != nil {
				t.Fatal(err)
			}
			for _, ev := range timeline {
				if ev.StepKey == "remote_tool" && ev.StepName == "[西班牙] 观察屏幕" && ev.Status == "succeeded" {
					foundRemoteStep = true
				}
			}
		case "media":
			var media MediaCard
			if err := json.Unmarshal(m.Payload, &media); err != nil {
				t.Fatal(err)
			}
			if media.DeviceName == "西班牙" && media.ID != "" {
				body, _, err := s.Attachment(ctx, media.ID, false)
				if err != nil {
					t.Fatalf("Attachment(%s): %v", media.ID, err)
				}
				body.Close()
				foundRemoteMedia = true
			}
		}
	}
	if !foundRemoteStep {
		t.Fatal("expected [西班牙] 观察屏幕 remote_tool step in dev66 task timeline")
	}
	if !foundRemoteMedia {
		t.Fatal("expected 西班牙 screenshot media card in dev66 chat messages")
	}
}

func newMentionTestWorkbench(t *testing.T) (*Service, context.Context) {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	store, err := db.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg, err := settings.New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(store, cfg, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	toolsJSON, _ := json.Marshal(proto.DeviceTools)
	resJSON, _ := json.Marshal(proto.DeviceResources)
	err = store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at) VALUES (1,'admin','hash',1)`); err != nil {
			return err
		}
		for _, d := range []struct{ id, name string }{
			{"dev66", "66开发机"},
			{"deves", "西班牙"},
			{"devjp", "东京"},
		} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO devices
				(id,name,platform,arch,os_version,group_name,conn_state,helper_state,desktop_state,last_seen_at,task_protocol,mcp_tools,mcp_resources)
				VALUES (?,?,'windows','amd64','Windows 11','default','online','ready','unlocked',1,2,?,?)`,
				d.id, d.name, string(toolsJSON), string(resJSON)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO device_credentials(device_id,token_hash,rotated_at) VALUES (?,'hash',1)`, d.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO device_sessions(device_id,conn_gen,connected_at,remote_ip,agent_ver) VALUES (?,0,1,'127.0.0.1','test')`, d.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, ctx
}

func testSampleJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 120, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
