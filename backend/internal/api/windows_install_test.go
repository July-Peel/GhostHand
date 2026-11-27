package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
	"ghosthand/backend/internal/db"
	"ghosthand/proto"
)

func TestWindowsInstaller(t *testing.T) {
	t.Setenv("GH_NODE_KEY", "")
	ctx, dir := t.Context(), t.TempDir()
	store, err := db.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authentication := auth.New(store)
	if _, err := authentication.Bootstrap(ctx, "admin", "synthetic-admin-password", false); err != nil {
		t.Fatal(err)
	}
	_, cookie, err := authentication.Login(ctx, "admin", "synthetic-admin-password", "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := agents.New(ctx, store, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://controller.example"
	handler := New(store, authentication, nil, nil, fleet, nil, origin, "", nil)
	request := func(method, path string, body []byte, credentials string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/octet-stream")
		if credentials == "session" || credentials == "admin" {
			req.AddCookie(&http.Cookie{Name: cookieName(true), Value: cookie})
		}
		if credentials == "admin" {
			req.Header.Set("X-CSRF-Token", auth.CSRF(cookie))
		} else if strings.HasPrefix(credentials, "Bearer ") {
			req.Header.Set("Authorization", credentials)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("%s %s got %d, want %d: %s", method, path, response.Code, want, response.Body)
		}
		return response
	}
	const filesPath = "/api/pairing/windows/files"
	const manifestPath = windowsInstallPath + "manifest"
	const launcher = "ghosthand-launcher.exe"
	firstKey := "Bearer " + base64.StdEncoding.EncodeToString([]byte(fleet.NodeKey()))
	request("POST", filesPath+"/"+launcher, nil, "", http.StatusUnauthorized)
	request("POST", filesPath+"/"+launcher, nil, "session", http.StatusForbidden)
	request("POST", filesPath+"/other.exe", nil, "admin", http.StatusNotFound)
	request("GET", manifestPath, nil, "", http.StatusUnauthorized)
	request("GET", manifestPath, nil, firstKey, http.StatusConflict)

	// Minimal PE header fixture for server-side validation, never executed.
	fixture := make([]byte, 91)
	copy(fixture, "MZ")
	binary.LittleEndian.PutUint32(fixture[60:], 64)
	copy(fixture[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(fixture[68:], 0x8664)
	binary.LittleEndian.PutUint16(fixture[86:], 2)
	binary.LittleEndian.PutUint16(fixture[88:], 0x20b)
	for i, name := range []string{"GhostHand.Helper.exe", "ghosthand-core.exe", launcher} {
		fixture[90] = byte(i)
		request("POST", filesPath+"/"+name, fixture, "admin", http.StatusCreated)
	}
	request("POST", filesPath+"/"+launcher, []byte("bad replacement"), "admin", http.StatusBadRequest)
	response := request("GET", manifestPath, nil, firstKey, http.StatusOK)
	var manifest []agents.WindowsInstallerFile
	if err := json.Unmarshal(response.Body.Bytes(), &manifest); err != nil || len(manifest) != 3 {
		t.Fatalf("unexpected manifest: %s (%v)", response.Body, err)
	}
	downloadPath := windowsInstallPath + "files/" + launcher
	request("GET", downloadPath, nil, "", http.StatusUnauthorized)
	response = request("GET", downloadPath, nil, firstKey, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), fixture) || manifest[2].SHA256 != fmt.Sprintf("%x", sha256.Sum256(fixture)) {
		t.Fatal("invalid replacement changed the launcher or its checksum")
	}
	fixture[90] = 42
	request("POST", filesPath+"/"+launcher, fixture, "admin", http.StatusCreated)
	if response := request("GET", downloadPath, nil, firstKey, http.StatusOK); !bytes.Equal(response.Body.Bytes(), fixture) {
		t.Fatal("launcher replacement was not published")
	}

	script := request("GET", windowsInstallPath+"install.ps1", nil, "", http.StatusOK)
	scriptHash := fmt.Sprintf("%x", sha256.Sum256(script.Body.Bytes()))
	pairing := request("GET", "/api/pairing", nil, "admin", http.StatusOK)
	if !strings.Contains(pairing.Body.String(), scriptHash) || !strings.Contains(pairing.Body.String(), windowsInstallPath+"install.ps1") {
		t.Fatal("pairing command does not reference the served script and its checksum")
	}
	request("POST", "/api/pairing/rotate", nil, "admin", http.StatusOK)
	request("GET", downloadPath, nil, firstKey, http.StatusUnauthorized)

	request("GET", "/api/agent/components/freerdp", nil, "", http.StatusUnauthorized)
	token, err := proto.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := fleet.Enroll(ctx, proto.Enrollment{
		Key: fleet.NodeKey(), MachineID: "machine-freerdp-test-0001", Token: token,
		Name: "WinNode", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 10.0.19045",
	}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	freerdpReq := httptest.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/agent/components/freerdp", nil)
	freerdpReq.Header.Set("Authorization", "Bearer "+token)
	freerdpReq.Header.Set("X-GhostHand-Device", enrolled.DeviceID)
	freerdpResp := httptest.NewRecorder()
	handler.ServeHTTP(freerdpResp, freerdpReq)
	if freerdpResp.Code != http.StatusOK {
		t.Fatalf("GET /api/agent/components/freerdp got %d: %s", freerdpResp.Code, freerdpResp.Body)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(freerdpResp.Body.Bytes()))
	if gotHash := freerdpResp.Header().Get("X-GhostHand-SHA256"); gotHash != wantHash || freerdpResp.Body.Len() == 0 {
		t.Fatalf("FreeRDP bundle hash header=%q want=%q len=%d", gotHash, wantHash, freerdpResp.Body.Len())
	}
}
