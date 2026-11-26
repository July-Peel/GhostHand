package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"ghosthand/proto"
)

func (s *Service) Seed(ctx context.Context) error {
	if !s.demo {
		return nil
	}
	devices := []struct {
		id, name, platform, arch, version, group, conn, helper, desktop string
		battery, charging                                               any
		age                                                             time.Duration
	}{
		{"demo-windows", "工作站", "windows", "amd64", "Windows 11", "办公", "online", "ready", "unlocked", 82, true, 0},
		{"demo-android", "Pixel 测试机", "android", "arm64", "Android 14", "移动设备", "online", "ready", "screen_off", 64, false, 0},
		{"demo-linux", "实验室服务器", "linux", "amd64", "Debian 12", "实验室", "online", "none", "headless", nil, nil, 0},
		{"demo-laptop", "备用笔记本", "windows", "amd64", "Windows 10", "办公", "offline", "unavailable", "no_session", 37, false, 2 * time.Hour},
	}
	now := time.Now()
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		for _, device := range devices {
			result, err := tx.ExecContext(ctx, `
				INSERT INTO devices(id, name, platform, arch, os_version, group_name, conn_state,
				                    helper_state, desktop_state, battery_pct, charging, last_seen_at, is_demo)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1) ON CONFLICT(id) DO NOTHING
			`, device.id, device.name, device.platform, device.arch, device.version, device.group, device.conn,
				device.helper, device.desktop, device.battery, device.charging, now.Add(-device.age).UnixMilli())
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count == 0 {
				continue
			}
			note := "模拟设备已就绪。试试下方的快捷指令。"
			if device.conn == "offline" {
				note = "此模拟设备处于离线状态。指令会进入队列，你可以撤回。"
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO chat_messages(device_id, sender, type, text, created_at) VALUES (?, 'system', 'system', ?, ?)
			`, device.id, note, now.UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunDemo wakes only when a transition is due or the database changes.
func (s *Service) RunDemo(ctx context.Context) {
	if !s.demo {
		return
	}
	for ctx.Err() == nil {
		changed := s.store.Changed()
		var next *int64
		err := s.store.QueryRowContext(ctx, `
			SELECT MIN(t.next_at) FROM tasks t JOIN devices d ON d.id=t.device_id
			WHERE t.next_at IS NOT NULL AND d.is_demo=1
		`).Scan(&next)
		if err == nil && next == nil {
			select {
			case <-ctx.Done():
			case <-changed:
			}
			continue
		}
		var delay time.Duration
		if err == nil {
			delay = time.Until(time.UnixMilli(*next))
			if delay <= 0 {
				err = s.advanceDemo(ctx)
				if err == nil {
					continue
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("demo scheduler", "error", err)
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

type dueTask struct {
	id, deviceID string
	state        proto.TaskState
	conn         proto.ConnState
	capability   proto.CapabilityID
	stepIndex    int
	expiresAt    int64
}

func (s *Service) advanceDemo(ctx context.Context) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		var task dueTask
		err := tx.QueryRowContext(ctx, `
			SELECT t.id, t.device_id, t.state, d.conn_state, t.capability, t.step_index, t.expires_at
			FROM tasks t JOIN devices d ON d.id=t.device_id WHERE d.is_demo=1 AND t.next_at<=?
			ORDER BY t.next_at, t.id LIMIT 1
		`, now.UnixMilli()).Scan(&task.id, &task.deviceID, &task.state, &task.conn, &task.capability,
			&task.stepIndex, &task.expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := task.advance(ctx, tx, now); err != nil {
			return err
		}
		return RecordChange(ctx, tx, task.deviceID)
	})
}

func (task dueTask) advance(ctx context.Context, tx *sql.Tx, now time.Time) error {
	ms := now.UnixMilli()
	command := commandFor(task.capability)
	switch {
	case task.state == proto.Queued && task.expiresAt <= ms:
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='expired', finished_at=?, next_at=NULL WHERE id=?", ms, task.id); err != nil {
			return err
		}
		return addTaskEvent(ctx, tx, task.id, "queue", "expired", "超过队列有效期", ms)
	case task.state == proto.Queued && task.conn != proto.Online:
		_, err := tx.ExecContext(ctx, "UPDATE tasks SET next_at=expires_at WHERE id=?", task.id)
		return err
	case task.state == proto.Queued:
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state='running', step_index=1, started_at=?, next_at=? WHERE id=?
		`, ms, now.Add(800*time.Millisecond).UnixMilli(), task.id); err != nil {
			return err
		}
		return addTaskEvent(ctx, tx, task.id, "read", "running", command.Step, ms)
	case task.stepIndex == 1:
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET step_index=2, next_at=? WHERE id=?",
			now.Add(650*time.Millisecond).UnixMilli(), task.id); err != nil {
			return err
		}
		if err := addTaskEvent(ctx, tx, task.id, "read", "succeeded", "模拟数据已读取", ms); err != nil {
			return err
		}
		return addTaskEvent(ctx, tx, task.id, "result", "running", "整理结果", ms)
	default:
		return task.complete(ctx, tx, command.Title, ms)
	}
}

func (task dueTask) complete(ctx context.Context, tx *sql.Tx, title string, now int64) error {
	device, err := scanDevice(tx.QueryRowContext(ctx, "SELECT "+deviceFields+" FROM devices d WHERE d.id=?", task.deviceID))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='succeeded', finished_at=?, next_at=NULL WHERE id=?",
		now, task.id); err != nil {
		return err
	}
	if err := addTaskEvent(ctx, tx, task.id, "result", "succeeded", "结果已写入对话", now); err != nil {
		return err
	}
	payload, err := json.Marshal(StatusSnapshot{
		Title: title, OSVersion: device.OSVersion, Arch: device.Arch,
		BatteryPct: device.BatteryPct, Charging: device.Charging, DesktopState: device.DesktopState,
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO chat_messages(device_id, sender, type, task_id, payload_json, created_at)
		VALUES (?, 'agent', 'status_card', ?, ?, ?)
	`, task.deviceID, task.id, string(payload), now)
	return err
}
