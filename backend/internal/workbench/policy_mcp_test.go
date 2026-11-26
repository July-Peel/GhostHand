package workbench

import (
	"database/sql"
	"encoding/json"
	"testing"

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
