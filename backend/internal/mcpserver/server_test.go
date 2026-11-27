package mcpserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

func TestMCPGatewayExposesAllToolsAndResources(t *testing.T) {
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
	desk, err := workbench.New(store, cfg, dir, false)
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_credentials(device_id,token_hash,rotated_at) VALUES ('win1','hash',1)`); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	gw := New(desk)
	callMCP := func(method string) map[string]any {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  method,
			"params":  map[string]any{},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/mcp?device=win1", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req = WithUser(req, 1)
		rec := httptest.NewRecorder()
		gw.HTTP.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", method, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s unmarshal failed: %v, body=%s", method, err, rec.Body.String())
		}
		return out
	}

	toolsResp := callMCP("tools/list")
	toolsList := toolsResp["result"].(map[string]any)["tools"].([]any)
	if len(toolsList) != len(proto.DeviceTools)+1 {
		t.Fatalf("exposed MCP tools = %d, want %d including devices_list", len(toolsList), len(proto.DeviceTools)+1)
	}

	resResp := callMCP("resources/list")
	resList := resResp["result"].(map[string]any)["resources"].([]any)
	if len(resList) != len(proto.DeviceResources) {
		t.Fatalf("exposed MCP resources = %d, want %d", len(resList), len(proto.DeviceResources))
	}
}
