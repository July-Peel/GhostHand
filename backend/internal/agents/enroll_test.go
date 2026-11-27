package agents

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

func TestFixedKeyEnrollRejoinByMachineID(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := db.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	svc, err := New(ctx, store, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	fixedKey := svc.NodeKey()
	if fixedKey == "" {
		t.Fatal("expected non-empty fixed node key")
	}

	// Wrong key should fail.
	token1, _ := proto.NewToken()
	if _, err := svc.Enroll(ctx, proto.Enrollment{
		Key: "wrong-key-123456", MachineID: "0123456789abcdef0123456789abcdef",
		Token: token1, Name: "PC-1", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 11",
	}, "127.0.0.1"); err != ErrDenied {
		t.Fatalf("wrong key error=%v, want ErrDenied", err)
	}

	// First device joins with fixed key.
	res1, err := svc.Enroll(ctx, proto.Enrollment{
		Key: fixedKey, MachineID: "11111111111111111111111111111111",
		Token: token1, Name: "PC-1", Group: "Office", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 11",
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("enroll device 1: %v", err)
	}

	// Second device joins with the SAME fixed key and gets a distinct device ID.
	token2, _ := proto.NewToken()
	res2, err := svc.Enroll(ctx, proto.Enrollment{
		Key: fixedKey, MachineID: "22222222222222222222222222222222",
		Token: token2, Name: "PC-2", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 10",
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("enroll device 2: %v", err)
	}
	if res1.DeviceID == res2.DeviceID {
		t.Fatalf("expected distinct device IDs for distinct machines, both got %s", res1.DeviceID)
	}

	// Insert a user and a chat message on device 1 to verify re-association preserves existing data.
	if err := store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at) VALUES (1,'admin','hash',1)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,text,created_at) VALUES (?,'user','text','keep-me',1)`, res1.DeviceID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Device 1 restarts/updates and rejoins with the fixed key + same MachineID + new ephemeral token.
	token1New, _ := proto.NewToken()
	res1Again, err := svc.Enroll(ctx, proto.Enrollment{
		Key: fixedKey, MachineID: "11111111111111111111111111111111",
		Token: token1New, Name: "PC-1-Renamed", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 11",
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("rejoin device 1: %v", err)
	}
	if res1Again.DeviceID != res1.DeviceID {
		t.Fatalf("rejoin DeviceID=%s, want %s", res1Again.DeviceID, res1.DeviceID)
	}

	var name, group string
	var msgCount int
	if err := store.QueryRowContext(ctx, `SELECT name, group_name, (SELECT COUNT(*) FROM chat_messages WHERE device_id=devices.id) FROM devices WHERE id=?`,
		res1.DeviceID).Scan(&name, &group, &msgCount); err != nil {
		t.Fatal(err)
	}
	if name != "PC-1-Renamed" || group != "Office" || msgCount != 1 {
		t.Fatalf("name=%q group=%q msgCount=%d; want PC-1-Renamed, Office, 1", name, group, msgCount)
	}
	if _, err := svc.RenameDevice(ctx, 1, res1.DeviceID, "财务部\n恶意换行", false); err != ErrInvalid {
		t.Fatalf("name containing newline: %v", err)
	}
	if name, err := svc.RenameDevice(ctx, 1, res1.DeviceID, "  财务部工作站  ", false); err != nil || name != "财务部工作站" {
		t.Fatalf("rename: %q, %v", name, err)
	}
	if _, err := svc.Enroll(ctx, proto.Enrollment{
		Key: fixedKey, MachineID: "11111111111111111111111111111111", Token: token1New,
		Name: "AnotherHostName", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 11",
	}, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := store.QueryRowContext(ctx, `SELECT name,(SELECT detail_json FROM audit_logs WHERE device_id=devices.id AND action='device.rename')
		FROM devices WHERE id=?`, res1.DeviceID).Scan(&name, &detail); err != nil {
		t.Fatal(err)
	}
	if name != "财务部工作站" || detail != `{"new_name":"财务部工作站","old_name":"PC-1-Renamed"}` {
		t.Fatalf("name=%q audit=%s", name, detail)
	}

	// Revoked device cannot rejoin until restored.
	if err := svc.Revoke(ctx, 1, res1.DeviceID); err != nil {
		t.Fatal(err)
	}
	tokenRevoked, _ := proto.NewToken()
	if _, err := svc.Enroll(ctx, proto.Enrollment{
		Key: fixedKey, MachineID: "11111111111111111111111111111111",
		Token: tokenRevoked, Name: "PC-1", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 11",
	}, "127.0.0.1"); err != ErrDenied {
		t.Fatalf("revoked rejoin error=%v, want ErrDenied", err)
	}
	if err := svc.Restore(ctx, 1, res1.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enroll(ctx, proto.Enrollment{
		Key: fixedKey, MachineID: "11111111111111111111111111111111",
		Token: tokenRevoked, Name: "PC-1", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 11",
	}, "127.0.0.1"); err != nil {
		t.Fatalf("restored rejoin: %v", err)
	}
}

func TestDeleteOfflineDeviceRemovesAllDataAndLogs(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := db.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	svc, err := New(ctx, store, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := proto.NewToken()
	res, err := svc.Enroll(ctx, proto.Enrollment{
		Key: svc.NodeKey(), MachineID: "33333333333333333333333333333333",
		Token: token, Name: "Old-PC", Platform: proto.Windows, Arch: "amd64", OSVersion: "Windows 10",
	}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	opID := "0123456789ABCDEFGHJKMNPQRV"
	logFile := filepath.Join(svc.dir, "logs", opID+".jsonl.gz")
	if err := os.WriteFile(logFile, []byte("gz"), 0600); err != nil {
		t.Fatal(err)
	}
	taskID := "t_0123456789abcdef0123456789abcdef"

	if err := store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at) VALUES (1,'admin','hash',1)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,device_id,created_by,capability,state,created_at,expires_at)
			VALUES (?,?,1,'device.info','succeeded',1,9999999999999)`, taskID, res.DeviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,task_id,upload_id,name,mime,size,sha256,created_at,storage_backend,storage_key,thumb_key)
			VALUES ('0123456789ABCDEFGHJKMNPQRW',?,'','s.jpg','image/jpeg',1,'h',1,'local','s.jpg','s.thumb.jpg')`, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,text,created_at)
			VALUES (?,'user','text','hi',1)`, res.DeviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_operations(id,device_id,kind,state,created_at,uploaded_at)
			VALUES (?,?,'log.collect','succeeded',1,2)`, opID, res.DeviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pairing_codes(code,expires_at,device_id) VALUES ('123456',1,?)`, res.DeviceID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Online device cannot be deleted.
	if err := store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE devices SET conn_state='online' WHERE id=?", res.DeviceID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteDevice(ctx, 1, res.DeviceID); err != ErrDeviceOnline {
		t.Fatalf("online delete error=%v, want ErrDeviceOnline", err)
	}

	// Offline device deletes completely.
	if err := store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE devices SET conn_state='offline' WHERE id=?", res.DeviceID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	localFiles, err := svc.DeleteDevice(ctx, 1, res.DeviceID)
	if err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	if len(localFiles) != 1 || localFiles[0].Key != "s.jpg" {
		t.Fatalf("unexpected localFiles: %v", localFiles)
	}
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Fatalf("expected agent log file deleted, err=%v", err)
	}
	for _, table := range []string{"devices", "device_credentials", "tasks", "attachments", "chat_messages", "device_operations", "pairing_codes"} {
		query := "SELECT COUNT(*) FROM " + table
		var args []any
		if table == "devices" {
			query += " WHERE id<>?"
			args = append(args, workbench.FreeChatDeviceID)
		}
		var count int
		if err := store.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count=%d, want 0", table, count)
		}
	}
}
