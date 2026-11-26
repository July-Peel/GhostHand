package agents

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

func TestRDPTicketFuseAndAutomaticRelock(t *testing.T) {
	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	dir := t.TempDir()
	store, err := db.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings, err := settings.New(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := New(ctx, store, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,created_at) VALUES (1,'admin','hash',1)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	enroll := func(machine string) (string, string) {
		token, _ := proto.NewToken()
		node, err := fleet.Enroll(ctx, proto.Enrollment{Key: fleet.NodeKey(), MachineID: machine, Token: token,
			Name: "Test Windows", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 11"}, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		return node.DeviceID, token
	}
	device, token := enroll("11111111111111111111111111111111")
	other, otherToken := enroll("22222222222222222222222222222222")
	p := &peer{id: device, desktopOperations: true, secureTransport: true, desktopRequests: make(chan desktopRequest),
		desktopDone: make(chan struct{}), desktopPolicyChanged: make(chan struct{}, 1)}
	fleet.connections[device] = p
	manager := NewUnlockManager(fleet, settings)
	const password = "synthetic-password"
	if err := manager.Save(ctx, 1, device, RDPConfig{Username: "LocalUser", Password: password}); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := store.QueryRowContext(ctx, "SELECT password_sealed FROM device_rdp_credentials WHERE device_id=?", device).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(password)) {
		t.Fatal("plaintext saved in database")
	}
	if _, err := settings.Open([]byte("device-rdp-v1:"+other), sealed); err == nil {
		t.Fatal("RDP credential accepted under a different device")
	}
	reply := func(kind proto.MessageType, code proto.ErrorCode, detail any) {
		var request desktopRequest
		select {
		case request = <-p.desktopRequests:
		case <-ctx.Done():
			t.Fatal("missing desktop request")
		}
		if request.kind != kind {
			t.Fatalf("request kind=%s want %s", request.kind, kind)
		}
		id, ticket := request.payload.OperationID, request.payload.Ticket
		if kind == proto.RDPUnlock {
			if _, err := manager.Redeem(ctx, other, otherToken, id, ticket); !errors.Is(err, ErrDenied) {
				t.Fatalf("different device: %v", err)
			}
			credential, err := manager.Redeem(ctx, device, token, id, ticket)
			if err != nil || credential.Username != "LocalUser" || !bytes.Equal(credential.Password, []byte(password)) {
				t.Fatalf("credential redemption failed: %v", err)
			}
			clear(credential.Password)
			if _, err := manager.Redeem(ctx, device, token, id, ticket); !errors.Is(err, ErrRDPTicket) {
				t.Fatalf("reused ticket: %v", err)
			}
		} else if request.payload.SessionID != 7 || ticket != "" {
			t.Fatal("relock not bound to the previously unlocked session")
		}
		body, _ := json.Marshal(detail)
		request.results <- proto.DesktopResult{OperationID: id, Kind: kind, OK: code == "", Code: code, Detail: body}
	}
	for i := 0; i < 2; i++ {
		finished := make(chan error, 1)
		go func() {
			_, err := manager.AutoUnlock(ctx, 1, device, "task", "brain")
			finished <- err
		}()
		reply(proto.RDPUnlock, proto.ErrRDPAuthFailed, nil)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	if manager.CanAutoUnlock(ctx, device) {
		t.Fatal("automatic unlock not suspended after two authentication failures")
	}
	if _, err := manager.Operate(ctx, 1, device, proto.RDPUnlock, "manual", false); !errors.Is(err, ErrRDPSuspended) {
		t.Fatalf("unconfirmed manual retry: %v", err)
	}
	manual := make(chan error, 1)
	go func() {
		_, err := manager.Operate(ctx, 1, device, proto.RDPUnlock, "manual", true)
		manual <- err
	}()
	reply(proto.RDPUnlock, "", proto.RDPUnlockDetail{SessionID: 7})
	if err := <-manual; err != nil || !manager.CanAutoUnlock(ctx, device) {
		t.Fatalf("confirmed success did not clear suspension: %v", err)
	}
	automatic := make(chan error, 1)
	go func() {
		_, err := manager.AutoUnlock(ctx, 1, device, "other-task", "mcp")
		automatic <- err
	}()
	reply(proto.RDPUnlock, "", proto.RDPUnlockDetail{SessionID: 7})
	if err := <-automatic; err != nil {
		t.Fatal(err)
	}
	relocked := make(chan struct{})
	go func() {
		manager.FinishAutoUnlock(ctx, 1, device, "other-task")
		close(relocked)
	}()
	reply(proto.RDPLock, "", nil)
	<-relocked
	var outbox, secretAudit, suspendedAudit int
	err = store.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM outbox),
		(SELECT COUNT(*) FROM audit_logs WHERE instr(detail_json,?)>0),
		(SELECT COUNT(*) FROM audit_logs WHERE action='rdp.suspend')`, password).Scan(&outbox, &secretAudit, &suspendedAudit)
	if err != nil || outbox != 0 || secretAudit != 0 || suspendedAudit != 1 {
		t.Fatalf("outbox=%d secret audit=%d suspension audit=%d: %v", outbox, secretAudit, suspendedAudit, err)
	}
}
