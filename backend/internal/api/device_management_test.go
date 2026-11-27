package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

func TestDeviceManagementWithOnlineNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
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
	authentication := auth.New(store)
	if _, err := authentication.Bootstrap(ctx, "admin", "synthetic-admin-password", false); err != nil {
		t.Fatal(err)
	}
	session, cookie, err := authentication.Login(ctx, "admin", "synthetic-admin-password", "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := agents.New(ctx, store, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	desk, err := workbench.New(store, cfg, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := proto.NewToken()
	node, err := fleet.Enroll(ctx, proto.Enrollment{Key: fleet.NodeKey(), MachineID: "11111111111111111111111111111111",
		Token: token, Name: "Workstation", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 11"}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	unlock := agents.NewUnlockManager(fleet, cfg)
	handler := New(store, authentication, cfg, desk, fleet, unlock, "", "", nil)
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https")+proto.AgentSocketPath,
		&websocket.DialOptions{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	send := func(kind proto.MessageType, payload any) error {
		frame, err := proto.NewEnvelope(kind, payload)
		if err != nil {
			return err
		}
		return wsjson.Write(ctx, conn, frame)
	}
	if err := send(proto.Hello, proto.HelloPayload{DeviceID: node.DeviceID, Token: token, Platform: proto.Windows,
		DesktopOperations: true, State: proto.DeviceState{DesktopState: "locked", HelperState: "ready"}}); err != nil {
		t.Fatal(err)
	}
	var welcome proto.Envelope
	if err := wsjson.Read(ctx, conn, &welcome); err != nil || welcome.Type != proto.Welcome {
		t.Fatalf("handshake: %s, %v", welcome.Type, err)
	}
	tools, _ := json.Marshal(proto.DeviceTools)
	if err := store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET task_protocol=2,mcp_tools=? WHERE id=?", string(tools), node.DeviceID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,device_id,created_by,capability,state,created_at,expires_at,lane,source)
			VALUES ('t_running',?,?,'brain.react','running',1,9999999999999,'ui','brain')`, node.DeviceID, session.UserID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var installed atomic.Bool
	installed.Store(true)
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		for ctx.Err() == nil {
			var frame proto.Envelope
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				return
			}
			if frame.Type != proto.BrowserExtensionStatusRequest {
				continue
			}
			var req proto.DesktopRequest
			if json.Unmarshal(frame.Payload, &req) != nil {
				return
			}
			status, _ := json.Marshal(proto.BrowserExtensionStatus{Browser: "chrome", Installed: installed.Load()})
			if err := send(proto.BrowserExtensionStatusResult, proto.DesktopResult{
				OperationID: req.OperationID, Kind: frame.Type, OK: true, Detail: status,
			}); err != nil {
				return
			}
		}
	}()
	defer func() { _ = conn.CloseNow(); <-agentDone }()
	devicePath := server.URL + "/api/devices/" + node.DeviceID
	request := func(method, path, body string, signedIn, csrf bool, want int) string {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, method, devicePath+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if signedIn {
			req.AddCookie(&http.Cookie{Name: cookieName(true), Value: cookie})
		}
		if csrf {
			req.Header.Set("X-CSRF-Token", auth.CSRF(cookie))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s status=%d want=%d: %s", method, path, rec.Code, want, rec.Body)
		}
		return rec.Body.String()
	}
	request("POST", "/rename", `{"name":"办公室"}`, false, false, http.StatusUnauthorized)
	request("POST", "/rename", `{"name":"办公室"}`, true, false, http.StatusForbidden)
	if body := request("POST", "/rename", `{"name":"办公室"}`, true, true, http.StatusOK); !strings.Contains(body, "办公室") {
		t.Fatal("rename response missing saved name")
	}
	request("GET", "/browser-extension", "", true, false, http.StatusOK)
	if body := request("POST", "/browser-extension", `{"action":"install","browser":"chrome"}`, true, true, http.StatusOK); !strings.Contains(body, `"skipped":true`) {
		t.Fatal("install preflight did not skip an installed extension")
	}
	if body := request("POST", "/browser-extension", `{"action":"uninstall","browser":"chrome"}`, true, true, http.StatusConflict); !strings.Contains(body, "其他任务") {
		t.Fatal("uninstall did not observe the active Brain task")
	}
	installed.Store(false)
	request("POST", "/browser-extension", `{"action":"install","browser":"chrome"}`, true, true, http.StatusConflict)
	if body := request("POST", "/browser-extension", `{"action":"uninstall","browser":"chrome"}`, true, true, http.StatusOK); !strings.Contains(body, `"skipped":true`) {
		t.Fatal("uninstall preflight did not skip an absent extension")
	}
	request("POST", "/rdp/config", `{"username":"worker","password":"fixture-rdp-password"}`, true, true, http.StatusNoContent)
	body := request("GET", "/rdp", "", true, false, http.StatusOK)
	if !strings.Contains(body, `"configured":true`) || strings.Contains(body, `"password"`) || strings.Contains(body, "fixture-rdp-password") {
		t.Fatal("credential status missing or exposes password")
	}
}
