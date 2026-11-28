package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

func TestAutoApproveAndFullMCPTools(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := db.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

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
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices
			(id,name,platform,arch,os_version,group_name,conn_state,helper_state,desktop_state,last_seen_at,task_protocol,mcp_tools,mcp_resources)
			VALUES ('win1','WinNode','windows','amd64','Windows 11','default','online','ready','unlocked',1,2,?,?)`,
			string(toolsJSON), string(resJSON)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks
			(id,device_id,created_by,capability,state,created_at,expires_at,lane,source)
			VALUES ('t_0123456789abcdef0123456789abcdef','win1',1,'brain.react','waiting_user',1,9999999999999,'ui','brain')`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO confirmations(id,task_id,summary,created_at)
			VALUES ('0123456789ABCDEFGHJKMNPQRS','t_0123456789abcdef0123456789abcdef','confirm click',1)`); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.SetAutoApprove(ctx, 1, "win1", true); err != nil {
		t.Fatal(err)
	}

	devices, err := svc.Devices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || !devices[0].AutoApprove {
		t.Fatalf("expected device AutoApprove=true, got %+v", devices)
	}

	var decision, taskState string
	if err := store.QueryRowContext(ctx, `SELECT c.decision, t.state FROM confirmations c JOIN tasks t ON t.id=c.task_id WHERE c.id='0123456789ABCDEFGHJKMNPQRS'`).
		Scan(&decision, &taskState); err != nil {
		t.Fatal(err)
	}
	if decision != "approved" || taskState != "running" {
		t.Fatalf("decision=%q taskState=%q; want approved and running", decision, taskState)
	}

	for _, tool := range []string{proto.ToolShellExec, proto.ToolScriptRun, proto.ToolFSWrite, proto.ToolClipboard, proto.ToolClick, proto.ToolProcessManage} {
		if !IsMCPToolAllowed(tool) {
			t.Fatalf("IsMCPToolAllowed(%q) = false, want true", tool)
		}
	}
	if IsMCPToolAllowed(proto.ToolConfirm) {
		t.Fatal("human.confirm must not be exposed via external MCP")
	}
}

func TestCallMCPDoesNotWriteChatMessages(t *testing.T) {
	s, ctx := newMentionTestWorkbench(t)
	runCtx, stop := context.WithCancel(ctx)
	var bg sync.WaitGroup
	bg.Add(3)
	go func() { defer bg.Done(); s.RunTasks(runCtx) }()
	go func() { defer bg.Done(); s.RunBrain(runCtx) }()
	go func() {
		defer bg.Done()
		sampleJPEG := testSampleJPEG(t)
		var processed int64
		for runCtx.Err() == nil {
			rows, err := s.store.QueryContext(runCtx, `SELECT seq, envelope_json FROM outbox WHERE device_id='dev66' AND seq>? ORDER BY seq`, processed)
			if err != nil {
				return
			}
			type item struct {
				seq int64
				raw string
			}
			var items []item
			for rows.Next() {
				var it item
				if rows.Scan(&it.seq, &it.raw) == nil {
					items = append(items, it)
				}
			}
			rows.Close()
			for _, it := range items {
				processed = it.seq
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
						return ApplyTaskReport(runCtx, tx, "dev66", accept, time.Now().UnixMilli())
					})
				case proto.MCPToolCall:
					var call proto.MCPCall
					_ = json.Unmarshal(env.Payload, &call)
					res := proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Data: json.RawMessage(`{"ok":true}`)}
					if call.Tool == proto.ToolObserve {
						now := time.Now()
						img, err := s.saveUserAttachment(runCtx, UploadImage{Name: "screen.jpg", Data: sampleJPEG}, settings.Configuration{}, now)
						if err == nil {
							_ = s.store.Write(runCtx, func(tx *sql.Tx) error {
								return img.insert(runCtx, tx, call.TaskID, call.CallID, now)
							})
							res.AttachmentID = img.ID
						}
					}
					frame, _ := proto.NewEnvelope(proto.MCPToolResult, res)
					_ = s.store.Write(runCtx, func(tx *sql.Tx) error {
						if err := ApplyTaskReport(runCtx, tx, "dev66", frame, time.Now().UnixMilli()); err != nil {
							return err
						}
						return RecordChange(runCtx, tx, "dev66")
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

	res, err := s.CallMCP(ctx, 1, "dev66", proto.ToolStatus, json.RawMessage(`{}`))
	if err != nil || res.Error != "" {
		t.Fatalf("CallMCP(device_status) = (%+v, %v)", res, err)
	}
	obs, err := s.CallMCP(ctx, 1, "dev66", proto.ToolObserve, json.RawMessage(`{}`))
	if err != nil || obs.Error != "" || obs.AttachmentID == "" {
		t.Fatalf("CallMCP(screen_observe) = (%+v, %v)", obs, err)
	}
	img, err := s.MCPImage(ctx, obs.AttachmentID)
	if err != nil || len(img) == 0 {
		t.Fatalf("MCPImage(%q) = (%d bytes, %v)", obs.AttachmentID, len(img), err)
	}

	page, err := s.Messages(ctx, "dev66", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 0 {
		t.Fatalf("expected 0 chat messages after MCP calls, got %+v", page.Messages)
	}
	var count int
	if err := s.store.QueryRowContext(ctx, "SELECT COUNT(*) FROM chat_messages WHERE device_id='dev66'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("chat_messages count = %d, want 0", count)
	}
}
