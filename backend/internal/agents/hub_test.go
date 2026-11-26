package agents

import (
	"database/sql"
	"testing"

	"ghosthand/backend/internal/db"
	"ghosthand/proto"
)

func TestUpdateDevicePublishesOnlyChangesAndNewSessions(t *testing.T) {
	ctx := t.Context()
	store, err := db.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO devices
			(id,name,platform,arch,os_version,group_name,conn_state,helper_state,desktop_state,last_seen_at)
			VALUES ('node','Node','windows','amd64','Windows','', 'online','ready','unlocked',1)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	battery, charging := 85, true
	steps := []struct {
		name       string
		state      proto.DeviceState
		newSession bool
		events     int
	}{
		{name: "plain heartbeat", events: 0},
		{name: "new session with same status", newSession: true, events: 1},
		{name: "reported battery", state: proto.DeviceState{BatteryPct: &battery, Charging: &charging}, events: 2},
		{name: "repeated battery", state: proto.DeviceState{BatteryPct: &battery, Charging: &charging}, events: 2},
		{name: "locked", state: proto.DeviceState{DesktopState: "locked", BatteryPct: &battery, Charging: &charging}, events: 3},
	}
	for index, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			now := int64(index + 2)
			if err := store.Write(ctx, func(tx *sql.Tx) error {
				return updateDevice(ctx, tx, "node", proto.Online, step.state, now, step.newSession)
			}); err != nil {
				t.Fatal(err)
			}
			var seen int64
			var events int
			err := store.QueryRowContext(ctx, `SELECT last_seen_at,(SELECT COUNT(*) FROM browser_events)
				FROM devices WHERE id='node'`).Scan(&seen, &events)
			if err != nil {
				t.Fatal(err)
			}
			if seen != now || events != step.events {
				t.Fatalf("last_seen_at = %d, browser events = %d; want %d and %d", seen, events, now, step.events)
			}
		})
	}
}
