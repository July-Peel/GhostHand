package mcpserver

import (
	"bytes"
	"context"
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

type sshConnectionStub struct {
	device string
	user   int64
	state  proto.ConnState
}

func (s *sshConnectionStub) IsSSHDevice(_ context.Context, device string) bool {
	return device == "ssh1"
}

func (s *sshConnectionStub) ConnectSSH(_ context.Context, user int64, device string) error {
	s.user, s.device, s.state = user, device, proto.Online
	return nil
}

func (s *sshConnectionStub) DisconnectSSH(_ context.Context, user int64, device string) error {
	s.user, s.device, s.state = user, device, proto.Offline
	return nil
}

func (s *sshConnectionStub) ExecuteSSHTool(context.Context, string, proto.MCPCall) (proto.MCPResult, error) {
	panic("SSH connection management must run on the controller")
}

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
	ssh := &sshConnectionStub{}
	desk.SetSSHExecutor(ssh)

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
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices
			(id,name,platform,arch,os_version,group_name,conn_state,helper_state,desktop_state,last_seen_at)
			VALUES ('ssh1','SSH Test','linux','amd64','Linux','default','offline','none','headless',1)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO device_credentials(device_id,token_hash,rotated_at) VALUES ('ssh1','ssh-hash',1)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	gw := New(desk)
	callMCP := func(path, method string, params map[string]any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  method,
			"params":  params,
		})
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
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

	toolsResp := callMCP("/api/mcp?device=win1", "tools/list", nil)
	toolsList := toolsResp["result"].(map[string]any)["tools"].([]any)
	wantTools := len(proto.DeviceTools) + len(workbench.MCPSSHConnectionTools) + 1
	if len(toolsList) != wantTools {
		t.Fatalf("exposed MCP tools = %d, want %d including devices_list and SSH connection tools", len(toolsList), wantTools)
	}

	resResp := callMCP("/api/mcp?device=win1", "resources/list", nil)
	resList := resResp["result"].(map[string]any)["resources"].([]any)
	if len(resList) != len(proto.DeviceResources) {
		t.Fatalf("exposed MCP resources = %d, want %d", len(resList), len(proto.DeviceResources))
	}

	callSSH := func(path, tool string, args map[string]string) map[string]any {
		t.Helper()
		response := callMCP(path, "tools/call", map[string]any{"name": tool, "arguments": args})
		result, ok := response["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s response: %v", tool, response)
		}
		return result
	}
	checkConnection := func(result map[string]any, want proto.ConnState) {
		t.Helper()
		data, _ := result["structuredContent"].(map[string]any)
		if data["device_id"] != "ssh1" || data["conn_state"] != string(want) || ssh.user != 1 || ssh.device != "ssh1" || ssh.state != want {
			t.Fatalf("SSH tool result=%v, executor=%+v, want device ssh1, user 1, state %s", result, ssh, want)
		}
	}
	checkConnection(callSSH("/api/mcp", "ssh_connect", map[string]string{"device_id": "SSH Test"}), proto.Online)
	if result := callSSH("/api/mcp?device=win1", "ssh_disconnect", map[string]string{}); result["isError"] != true || ssh.state != proto.Online {
		t.Fatalf("SSH tool accepted a native Agent: %+v", result)
	}
	if result := callSSH("/api/mcp?device=ssh1", "ssh_disconnect", map[string]string{"host": "unused"}); result["isError"] != true || ssh.state != proto.Online {
		t.Fatalf("SSH tool accepted an extra parameter: %+v", result)
	}
	checkConnection(callSSH("/api/mcp?device=ssh1", "ssh_disconnect", map[string]string{}), proto.Offline)
}
