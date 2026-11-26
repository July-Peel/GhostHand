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

const ForceCancelDelay = 5 * time.Second

const eligibleTasks = `
	FROM tasks t
	JOIN devices d ON d.id=t.device_id
	WHERE d.is_demo=0 AND (
		(t.state IN ('queued','dispatched') AND t.expires_at<=?)
		OR (t.state='queued' AND d.task_protocol>=1 AND (t.source NOT IN ('brain','mcp') OR (d.task_protocol>=2 AND json_array_length(d.mcp_tools)>0))
			AND d.conn_state IN ('online','unstable')
			AND EXISTS (SELECT 1 FROM device_sessions s WHERE s.device_id=d.id AND s.conn_gen=d.conn_gen AND s.disconnected_at IS NULL)
			AND (t.assign_seq IS NOT NULL OR (
				SELECT COUNT(*) + CASE WHEN t.lane='bg' THEN EXISTS (
					SELECT 1 FROM device_operations x WHERE x.device_id=t.device_id AND x.kind='log.collect' AND x.state='queued'
				) ELSE 0 END FROM tasks a WHERE a.device_id=t.device_id AND a.lane=t.lane
				AND (a.state IN ('dispatched','running','paused','waiting_user','cancelling')
					OR (a.state='queued' AND a.assign_seq IS NOT NULL))
			) < CASE WHEN t.lane='ui' THEN 1 ELSE 4 END))
	)`

func (s *Service) RunTasks(ctx context.Context) {
	for ctx.Err() == nil {
		changed := s.store.Changed()
		now := time.Now().UnixMilli()
		var ready bool
		err := s.store.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1"+eligibleTasks+")", now).Scan(&ready)
		if err == nil && ready {
			err = s.dispatchTask(ctx)
			if err == nil {
				continue
			}
		}
		var next *int64
		if err == nil {
			err = s.store.QueryRowContext(ctx, `SELECT MIN(t.expires_at) FROM tasks t JOIN devices d ON d.id=t.device_id
				WHERE d.is_demo=0 AND t.state IN ('queued','dispatched')`).Scan(&next)
		}
		if err == nil {
			var stall *int64
			stall, err = s.nextStall(ctx)
			if err == nil && stall != nil {
				if *stall <= now {
					if err = s.markStalled(ctx); err == nil {
						continue
					}
				} else if next == nil || *stall < *next {
					next = stall
				}
			}
		}
		var timer *time.Timer
		var due <-chan time.Time
		if err != nil && ctx.Err() == nil {
			slog.Error("task scheduler", "error", err)
			timer = time.NewTimer(time.Second)
		} else if next != nil {
			timer = time.NewTimer(time.Until(time.UnixMilli(*next)))
		}
		if timer != nil {
			due = timer.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Service) dispatchTask(ctx context.Context) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		var assignment proto.TaskAssignment
		var device string
		var seq sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT t.id,t.device_id,t.capability,t.lane,t.expires_at,t.title,t.assign_seq`+
			eligibleTasks+` ORDER BY t.assign_seq IS NULL,t.created_at,t.id LIMIT 1`, now).
			Scan(&assignment.TaskID, &device, &assignment.Capability, &assignment.Lane, &assignment.ExpiresAt, &assignment.Title, &seq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if assignment.ExpiresAt <= now {
			if seq.Valid {
				if err := cancelQueuedAssignment(ctx, tx, device, assignment.TaskID, seq.Int64); err != nil {
					return err
				}
			}
			if err := finishTask(ctx, tx, assignment.TaskID, proto.Expired, "", now); err != nil {
				return err
			}
			return RecordChange(ctx, tx, device)
		}
		if !seq.Valid {
			number, err := Enqueue(ctx, tx, device, proto.TaskAssign, assignment)
			if err != nil {
				return err
			}
			seq = sql.NullInt64{Int64: number, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='dispatched',assign_seq=? WHERE id=?", seq, assignment.TaskID); err != nil {
			return err
		}
		if err := addTaskEvent(ctx, tx, assignment.TaskID, "queue", "dispatched", "已下发到设备", now); err != nil {
			return err
		}
		return RecordChange(ctx, tx, device)
	})
}

func Enqueue(ctx context.Context, tx *sql.Tx, device string, kind proto.MessageType, payload any) (int64, error) {
	frame, err := proto.NewEnvelope(kind, payload)
	if err != nil {
		return 0, err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, "UPDATE devices SET controller_seq=controller_seq+1 WHERE id=? RETURNING controller_seq",
		device).Scan(&seq); err != nil {
		return 0, err
	}
	frame.Seq = uint64(seq)
	data, err := json.Marshal(frame)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO outbox(device_id,seq,envelope_json) VALUES (?,?,?)", device, seq, string(data))
	return seq, err
}

// RequeueUnacknowledged preserves the original outbox sequence for replay.
func RequeueUnacknowledged(ctx context.Context, tx *sql.Tx, device string) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET state='queued' WHERE device_id=? AND state='dispatched'
		AND assign_seq>(SELECT controller_ack FROM devices WHERE id=?)`, device, device)
	return err
}

func (s *Service) Control(ctx context.Context, userID int64, device, task, action string) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		var state proto.TaskState
		var lane proto.Lane
		var capability proto.CapabilityID
		var control, prompt, source string
		var stalled bool
		var cancelAt, seq sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT t.state,t.lane,t.capability,t.control,t.cancel_at,t.assign_seq,t.stalled,t.prompt,t.source
			FROM tasks t JOIN devices d ON d.id=t.device_id
			WHERE t.id=? AND t.device_id=? AND (d.is_demo=0 OR ?)`, task, device, s.demo).
			Scan(&state, &lane, &capability, &control, &cancelAt, &seq, &stalled, &prompt, &source)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTaskNotFound
		}
		if err != nil {
			return err
		}
		created := time.Now()
		now := created.UnixMilli()
		switch {
		case action == "retry":
			if source == "mcp" || (state != proto.Failed && state != proto.Expired) {
				return ErrTaskControl
			}
			info, err := scanDevice(tx.QueryRowContext(ctx, "SELECT "+deviceFields+" FROM devices d WHERE d.id=?", device))
			if err != nil {
				return err
			}
			command := commandFor(capability)
			request := command.Slash
			if capability == proto.BrainTask {
				command, request = brainCommand, prompt
			}
			if request == "" || !command.supported(info) {
				return ErrUnsupported
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,text,created_at)
				VALUES (?,'user','text',?,?)`, device, request, now); err != nil {
				return err
			}
			if err := createTask(ctx, tx, userID, info, command, prompt, created); err != nil {
				return err
			}
		case action == "cancel" && state == proto.Cancelled:
			return nil
		case action == "cancel" && state == proto.Queued:
			if seq.Valid {
				if err := cancelQueuedAssignment(ctx, tx, device, task, seq.Int64); err != nil {
					return err
				}
			}
			if err := finishTask(ctx, tx, task, proto.Cancelled, "", now); err != nil {
				return err
			}
		default:
			if !seq.Valid {
				return ErrTaskControl
			}
			if err := requestControl(ctx, tx, device, task, state, lane, control, cancelAt, stalled, action, now); err != nil {
				return err
			}
		}
		if err := auditTask(ctx, tx, userID, device, task, "task."+action, now); err != nil {
			return err
		}
		return RecordChange(ctx, tx, device)
	})
}

func cancelQueuedAssignment(ctx context.Context, tx *sql.Tx, device, task string, seq int64) error {
	// Reuse the unacknowledged sequence without assigning expired or withdrawn work.
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET envelope_json=json_set(envelope_json,
		'$.type', ?, '$.payload', json_object('task_id',?)) WHERE device_id=? AND seq=?`,
		proto.TaskCancel, task, device, seq); err != nil {
		return err
	}
	// A peer may already have received the original frame before it acknowledged it.
	_, err := Enqueue(ctx, tx, device, proto.TaskCancel, proto.TaskControl{TaskID: task})
	return err
}

func requestControl(ctx context.Context, tx *sql.Tx, device, task string, state proto.TaskState, lane proto.Lane,
	current string, cancelAt sql.NullInt64, stalled bool, action string, now int64) error {
	control := proto.TaskControl{TaskID: task}
	var kind proto.MessageType
	switch action {
	case "pause":
		if state != proto.Running || current != "" {
			return ErrTaskControl
		}
		kind = proto.TaskPause
	case "resume":
		if state != proto.Paused || current != "" {
			return ErrTaskControl
		}
		kind = proto.TaskResume
	case "cancel":
		if state == proto.Cancelling {
			return nil
		}
		switch state {
		case proto.Dispatched, proto.Running, proto.Paused, proto.WaitingUser:
		default:
			return ErrTaskControl
		}
		state, kind = proto.Cancelling, proto.TaskCancel
		cancelAt = sql.NullInt64{Int64: now, Valid: true}
	case "force":
		active := state == proto.Running || state == proto.Paused || state == proto.WaitingUser || state == proto.Cancelling
		waited := state == proto.Cancelling && cancelAt.Valid && now >= cancelAt.Int64+ForceCancelDelay.Milliseconds()
		if lane != proto.UILane || current == "force" || !(waited || (active && stalled)) {
			return ErrTaskControl
		}
		state = proto.Cancelling
		cancelAt = sql.NullInt64{Int64: now, Valid: true}
		kind, control.Force = proto.TaskCancel, true
	default:
		return ErrTaskControl
	}
	if _, err := Enqueue(ctx, tx, device, kind, control); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state=?,control=?,cancel_at=? WHERE id=?", state, action, cancelAt, task); err != nil {
		return err
	}
	details := map[string]string{"pause": "请求在当前步骤结束后暂停", "resume": "请求继续任务",
		"cancel": "请求终止任务", "force": "请求强制终止任务"}
	return addTaskEvent(ctx, tx, task, "control", action, details[action], now)
}

func finishTask(ctx context.Context, tx *sql.Tx, task string, state proto.TaskState, code proto.ErrorCode, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?,error_code=?,finished_at=?,next_at=NULL,control='',stalled=0,
		elapsed_ms=CASE WHEN started_at IS NULL THEN 0 ELSE MAX(0,?-started_at) END,
		step_index=CASE WHEN ?='succeeded' THEN step_total ELSE step_index END WHERE id=?`,
		state, code, now, now, state, task); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_events SET status=?,elapsed_ms=MAX(elapsed_ms,?-created_at)
		WHERE task_id=? AND status='running' AND attempt>0`, state, now, task); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE confirmations SET decision='cancelled',decided_at=? WHERE task_id=? AND decision='pending'", now, task); err != nil {
		return err
	}
	details := map[proto.TaskState]string{
		proto.Cancelled: "任务已取消", proto.Expired: "超过队列有效期", proto.Succeeded: "任务已完成",
		proto.Failed: "任务执行失败", proto.Unknown: "执行结果待核实",
	}
	return addTaskEvent(ctx, tx, task, "result", string(state), details[state], now)
}
