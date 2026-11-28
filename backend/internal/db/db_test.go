package db

import (
	"database/sql"
	"testing"
)

func TestMigrateV13DeletesMCPChatMessages(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	store, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,created_at) VALUES (1,'admin','hash',1)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices
			(id,name,platform,arch,os_version,group_name,conn_state,helper_state,desktop_state,last_seen_at)
			VALUES ('dev1','Dev','windows','amd64','Windows 11','default','online','ready','unlocked',1)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks
			(id,device_id,created_by,capability,state,created_at,expires_at,lane,source) VALUES
			('t_mcp','dev1',1,'brain.react','succeeded',1,2,'ui','mcp'),
			('t_brain','dev1',1,'brain.react','succeeded',1,2,'ui','brain')`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages
			(device_id,sender,type,text,task_id,created_at) VALUES
			('dev1','agent','task_card','','t_mcp',1),
			('dev1','agent','text','外部 MCP 调用已完成。','t_mcp',2),
			('dev1','agent','media','','t_mcp',3),
			('dev1','user','text','hello',NULL,4),
			('dev1','agent','task_card','','t_brain',5),
			('dev1','agent','text','done','t_brain',6)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "PRAGMA user_version=12")
		return err
	})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	var version, mcpCount, remaining int
	if err := reopened.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 13 {
		t.Fatalf("user_version = %d, want 13", version)
	}
	if err := reopened.QueryRowContext(ctx, "SELECT COUNT(*) FROM chat_messages WHERE task_id='t_mcp'").Scan(&mcpCount); err != nil {
		t.Fatal(err)
	}
	if mcpCount != 0 {
		t.Fatalf("mcp chat_messages count = %d, want 0", mcpCount)
	}
	if err := reopened.QueryRowContext(ctx, "SELECT COUNT(*) FROM chat_messages").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 3 {
		t.Fatalf("remaining chat_messages count = %d, want 3", remaining)
	}
}
