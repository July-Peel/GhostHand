package workbench

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
)

func TestClearMessagesClearsHistoryTasksAndBrainSessions(t *testing.T) {
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

	taskID := "t_0123456789abcdef0123456789abcdef"
	attKey := "shot1.jpg"
	attThumb := "shot1.thumb.jpg"
	if err := os.WriteFile(filepath.Join(svc.fileDir, attKey), []byte("img"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.fileDir, attThumb), []byte("thumb"), 0600); err != nil {
		t.Fatal(err)
	}

	err = store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at) VALUES (1,'admin','hash',1)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices
			(id,name,platform,arch,os_version,group_name,conn_state,helper_state,desktop_state,last_seen_at,task_protocol,controller_seq)
			VALUES ('win1','WinNode','windows','amd64','Windows 11','default','online','ready','unlocked',1,2,1)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks
			(id,device_id,created_by,capability,state,created_at,expires_at,lane,source,assign_seq)
			VALUES (?, 'win1', 1, 'brain.react', 'running', 1, 9999999999999, 'ui', 'brain', 1)`, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(device_id,seq,envelope_json)
			VALUES ('win1', 1, '{"v":1,"type":"task.assign","payload":{"task_id":"`+taskID+`"}}')`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO brain_calls(id,task_id,tool,created_at)
			VALUES ('0123456789ABCDEFGHJKMNPQRS', ?, 'screen.observe', 1)`, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO confirmations(id,task_id,summary,created_at)
			VALUES ('0123456789ABCDEFGHJKMNPQRT', ?, 'confirm', 1)`, taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments
			(id,task_id,upload_id,name,mime,size,sha256,created_at,storage_backend,storage_key,thumb_key)
			VALUES ('0123456789ABCDEFGHJKMNPQRU', ?, '', 'shot.jpg', 'image/jpeg', 3, 'abc', 1, 'local', ?, ?)`,
			taskID, attKey, attThumb); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,text,created_at)
			VALUES ('win1','user','text','hello',1)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,text,task_id,created_at)
			VALUES ('win1','agent','text','world',?,2)`, taskID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	svc.registerBrainSession("win1", taskID, sessionCancel)

	if err := svc.ClearMessages(ctx, 1, "win1"); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}

	if sessionCtx.Err() == nil {
		t.Fatal("expected active brain session context to be cancelled")
	}

	for _, table := range []string{"chat_messages", "tasks", "brain_calls", "confirmations", "attachments"} {
		var count int
		if err := store.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count=%d, want 0", table, count)
		}
	}

	var outboxCount int
	if err := store.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE device_id='win1' AND json_extract(envelope_json,'$.type')='task.cancel'`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 2 {
		t.Fatalf("expected task.cancel entries in outbox, got %d", outboxCount)
	}

	if _, err := os.Stat(filepath.Join(svc.fileDir, attKey)); !os.IsNotExist(err) {
		t.Fatalf("expected local attachment file deleted, err=%v", err)
	}
}
