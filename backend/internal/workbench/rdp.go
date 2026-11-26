package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"ghosthand/proto"
)

type DesktopUnlocker interface {
	CanAutoUnlock(context.Context, string) bool
	AutoUnlock(ctx context.Context, user int64, device, task, source string) (proto.DesktopResult, error)
	FinishAutoUnlock(ctx context.Context, user int64, device, task string)
}

func (s *Service) SetDesktopUnlocker(unlocker DesktopUnlocker) {
	s.desktopUnlocker = unlocker
}

type taskUnlock struct {
	service  *Service
	task     brainTask
	attempts int
	user     int64
}

func (u *taskUnlock) retry(ctx context.Context, result proto.MCPResult, step int, attempt *int, tool string, args json.RawMessage, label, window string, approved bool) (proto.MCPResult, error) {
	t, s := u.task, u.service
	if t.quiet || s.desktopUnlocker == nil || result.Error != string(proto.ErrDesktopLocked) {
		return result, nil
	}
	if u.attempts >= 3 {
		result.Error = "本任务自动解锁已达 3 次。请由管理员解锁桌面。"
		return result, nil
	}
	if !s.desktopUnlocker.CanAutoUnlock(ctx, t.device) {
		return result, nil
	}
	if err := s.store.QueryRowContext(ctx, "SELECT created_by FROM tasks WHERE id=?", t.id).Scan(&u.user); err != nil {
		return result, err
	}
	if t.source != "mcp" {
		if err := s.store.Write(ctx, func(tx *sql.Tx) error {
			if err := checkBrainReady(ctx, tx, t.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,task_id,text,created_at)
				VALUES (?,'system','text',?,'检测到 Windows 桌面已锁屏，正在自动解锁…',?)`,
				t.device, t.id, time.Now().UnixMilli()); err != nil {
				return err
			}
			return RecordChange(ctx, tx, t.device)
		}); err != nil {
			return result, err
		}
	}
	u.attempts++
	outcome, err := s.desktopUnlocker.AutoUnlock(ctx, u.user, t.device, t.id, t.source)
	if err == nil && outcome.OK {
		(*attempt)++
		return s.callDevice(ctx, t, step, *attempt, tool, args, label, window, approved)
	}
	message := "请检查设备详情中的远程解锁状态。"
	switch outcome.Code {
	case proto.ErrRDPAuthFailed:
		message = "账号或密码错误，请在设备详情更新解锁凭据。"
	case proto.ErrRDPBlankPasswordRestricted:
		message = "当前账号或 Windows 安全策略不允许空密码远程登录。请改用 Windows 账户密码。"
	case proto.ErrRDPUserMismatch:
		message = "解锁账号与当前 Windows 控制台用户不一致。"
	case proto.ErrRDPTimeout:
		message = "等待 Windows 桌面就绪超时。"
	}
	result.Error = "自动解锁失败。" + message + " 请等待管理员处理锁屏。"
	if t.source == "mcp" {
		err := s.store.Write(ctx, func(tx *sql.Tx) error {
			body, _ := json.Marshal(result)
			_, err := tx.ExecContext(ctx, "UPDATE brain_calls SET result_json=? WHERE id=? AND task_id=?", string(body), result.CallID, t.id)
			return err
		})
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (u *taskUnlock) finish(ctx context.Context) {
	if u.attempts == 0 {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 26*time.Second)
	defer cancel()
	wait, stop := context.WithTimeout(cleanup, 8*time.Second)
	defer stop()
	for wait.Err() == nil {
		changed := u.service.store.Changed()
		var state proto.TaskState
		err := u.service.store.QueryRowContext(wait, "SELECT state FROM tasks WHERE id=?", u.task.id).Scan(&state)
		if err != nil || state != proto.Cancelling {
			break
		}
		select {
		case <-wait.Done():
		case <-changed:
		}
	}
	u.service.desktopUnlocker.FinishAutoUnlock(cleanup, u.user, u.task.device, u.task.id)
}
