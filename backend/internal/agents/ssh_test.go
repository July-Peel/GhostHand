package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

func TestDelegatedSSHRequiresExplicitConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	store, err := db.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, err := settings.New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at) VALUES (1,'admin','hash',1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(ctx, store, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetSettings(config)
	defer svc.CloseSSH()
	token, err := proto.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	node, err := svc.Enroll(ctx, proto.Enrollment{Key: svc.NodeKey(), MachineID: "11111111111111111111111111111111",
		Token: token, Name: "SSH host Agent", Platform: proto.Linux, Arch: "amd64", OSVersion: "Linux"}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	host := &peer{
		id: node.DeviceID, sshFrames: make(chan proto.Envelope, 4), sshDone: make(chan struct{}),
		terminalFrames: make(chan proto.Envelope, 4), terminalDone: make(chan struct{}),
		terminalSessions: make(map[string]*TerminalSession),
	}
	svc.connections[host.id] = host
	device, err := svc.CreateSSHDevice(ctx, 1, SSHCreateRequest{Name: "SSH device", Host: "127.0.0.1",
		Username: "fixture", Secret: "fixture-ssh-password", HostAgentID: host.id})
	if err != nil {
		t.Fatal(err)
	}
	checkState := func(want proto.ConnState, wantEnabled bool) {
		t.Helper()
		var state proto.ConnState
		var enabled bool
		err := store.QueryRowContext(ctx, `SELECT d.conn_state, s.enabled FROM devices d JOIN device_ssh_credentials s
			ON s.device_id=d.id WHERE d.id=?`, device.DeviceID).Scan(&state, &enabled)
		if err != nil || state != want || enabled != wantEnabled {
			t.Fatalf("SSH state=(%s, %t, %v), want (%s, %t, nil)", state, enabled, err, want, wantEnabled)
		}
	}
	nextFrame := func(want proto.MessageType) proto.Envelope {
		t.Helper()
		select {
		case frame := <-host.sshFrames:
			if frame.Type != want {
				t.Fatalf("SSH frame=%s, want %s", frame.Type, want)
			}
			return frame
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", want, ctx.Err())
			return proto.Envelope{}
		}
	}
	checkState(proto.Offline, false)
	select {
	case frame := <-host.sshFrames:
		t.Fatalf("adding an SSH device sent %s", frame.Type)
	default:
	}

	connect := func() (string, <-chan error) {
		t.Helper()
		result := make(chan error, 1)
		go func() { result <- svc.ConnectSSH(ctx, 1, device.DeviceID) }()
		var payload proto.SSHConnectPayload
		if err := json.Unmarshal(nextFrame(proto.SSHConnectMessage).Payload, &payload); err != nil || payload.AttemptID == "" {
			t.Fatalf("missing SSH attempt ID: %v", err)
		}
		return payload.AttemptID, result
	}
	firstAttempt, firstResult := connect()
	if err := svc.DisconnectSSH(ctx, 1, device.DeviceID); err != nil {
		t.Fatal(err)
	}
	nextFrame(proto.SSHDisconnectMessage)
	if err := <-firstResult; err == nil {
		t.Fatal("cancelled login reported success")
	}
	secondAttempt, secondResult := connect()
	svc.handleAgentSSHState(host, proto.SSHStatePayload{SSHDeviceID: device.DeviceID, AttemptID: firstAttempt, State: proto.Online})
	checkState(proto.Offline, true)
	svc.handleAgentSSHState(host, proto.SSHStatePayload{SSHDeviceID: device.DeviceID, AttemptID: secondAttempt, State: proto.Online})
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}
	checkState(proto.Online, true)
	if err := svc.ConnectSSH(ctx, 1, device.DeviceID); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-host.sshFrames:
		t.Fatalf("connecting an already online SSH device sent %s", frame.Type)
	default:
	}

	terminal, err := svc.ReserveTerminal(ctx, device.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminal.Open(ctx, "sh", 80, 24); err != nil {
		t.Fatal(err)
	}
	attempt := terminal.sshAttempt
	backdateActivity := func() {
		svc.sshMu.Lock()
		attempt.lastActivity = time.Now().Add(-sshIdleTimeout - time.Second)
		svc.sshMu.Unlock()
	}
	backdateActivity()
	if err := terminal.Input(ctx, "echo fixture\n"); err != nil {
		t.Fatal(err)
	}
	svc.expireSSHIdle(device.DeviceID, attempt)
	checkState(proto.Online, true)

	// An active tool call must keep the SSH session open, even if its last activity is old.
	const callID = "fixture-call"
	callResult := make(chan error, 1)
	go func() {
		_, err := svc.ExecuteSSHTool(ctx, device.DeviceID, proto.MCPCall{CallID: callID, Tool: proto.ToolStatus})
		callResult <- err
	}()
	nextFrame(proto.SSHCallMessage)
	backdateActivity()
	svc.expireSSHIdle(device.DeviceID, attempt)
	checkState(proto.Online, true)
	svc.handleAgentSSHResult(host.id, proto.SSHMCPResultPayload{CallID: callID})
	if err := <-callResult; err != nil {
		t.Fatal(err)
	}
	svc.expireSSHIdle(device.DeviceID, attempt)
	checkState(proto.Online, true)

	backdateActivity()
	svc.sshMu.Lock()
	attempt.idleTimer.Reset(0)
	svc.sshMu.Unlock()
	nextFrame(proto.SSHDisconnectMessage)
	<-terminal.Done()
	checkState(proto.Offline, false)
	status, err := svc.SSHStatus(ctx, device.DeviceID)
	if err != nil || status.LastError != "SSH 闲置 10 分钟，已自动断开" {
		t.Fatalf("idle status=%+v, err=%v", status, err)
	}

	thirdAttempt, thirdResult := connect()
	svc.handleAgentSSHState(host, proto.SSHStatePayload{SSHDeviceID: device.DeviceID, AttemptID: thirdAttempt, State: proto.Online})
	if err := <-thirdResult; err != nil {
		t.Fatal(err)
	}
	// An old idle callback must never close a new session on the same device.
	svc.expireSSHIdle(device.DeviceID, attempt)
	checkState(proto.Online, true)

	restarted, err := New(ctx, store, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetSettings(config)
	checkState(proto.Offline, false)
	restarted.handleAgentSSHState(host, proto.SSHStatePayload{SSHDeviceID: device.DeviceID, AttemptID: thirdAttempt, State: proto.Online})
	nextFrame(proto.SSHDisconnectMessage)
	checkState(proto.Offline, false)
}
