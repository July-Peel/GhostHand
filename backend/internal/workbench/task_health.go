package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"ghosthand/proto"
)

const runningTaskStates = "('running','paused','waiting_user','cancelling')"

func ReconcileTasks(ctx context.Context, tx *sql.Tx, device string, hello proto.HelloPayload, now int64) error {
	if !hello.TaskInventory {
		return nil
	}
	if len(hello.RunningTasks) > 32 || len(hello.PendingTaskResults) > 2048 {
		return ErrTaskMessage
	}
	live, pending := make(map[string]bool), make(map[string]bool)
	for _, task := range hello.RunningTasks {
		if !proto.TaskIDPattern.MatchString(task.TaskID) {
			return ErrTaskMessage
		}
		live[task.TaskID] = true
	}
	for _, id := range hello.PendingTaskResults {
		if !proto.TaskIDPattern.MatchString(id) {
			return ErrTaskMessage
		}
		pending[id] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,state FROM tasks WHERE device_id=? AND assign_seq<=?
		AND state IN ('queued','dispatched','running','paused','waiting_user','cancelling')`, device, hello.LastAck)
	if err != nil {
		return err
	}
	type entry struct {
		id    string
		state proto.TaskState
	}
	var tasks []entry
	for rows.Next() {
		var task entry
		if err := rows.Scan(&task.id, &task.state); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, task)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, task := range tasks {
		if live[task.id] || pending[task.id] {
			continue
		}
		state, code := proto.Failed, proto.ErrInternal
		if task.state == proto.Cancelling {
			state, code = proto.Cancelled, proto.ErrCancelled
		}
		if err := addTaskEvent(ctx, tx, task.id, "reconnect", "missing", "设备重连后任务上下文丢失", now); err != nil {
			return err
		}
		if err := finishTask(ctx, tx, task.id, state, code, now); err != nil {
			return err
		}
	}
	for id := range live {
		var state proto.TaskState
		var control string
		err := tx.QueryRowContext(ctx, "SELECT state,control FROM tasks WHERE device_id=? AND id=?", device, id).Scan(&state, &control)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		switch state {
		case proto.Cancelling, proto.Cancelled, proto.Expired, proto.Failed, proto.Succeeded, proto.Unknown:
			force := control == "force"
			var queued bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE device_id=?
				AND json_extract(envelope_json,'$.payload.task_id')=? AND (
					(json_extract(envelope_json,'$.type')='task.cancel' AND (NOT ? OR json_extract(envelope_json,'$.payload.force')=1))
					OR (? AND json_extract(envelope_json,'$.type')='mcp.task_finish')))`,
				device, id, force, state != proto.Cancelling).Scan(&queued); err != nil {
				return err
			}
			if !queued {
				if err := enqueueTaskCancel(ctx, tx, device, id, force, false); err != nil {
					return err
				}
			}
		case proto.Running, proto.Paused, proto.WaitingUser:
			if err := touchTask(ctx, tx, id, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func ResetAgentTasks(ctx context.Context, tx *sql.Tx, device string, now int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, state FROM tasks WHERE device_id=?
		AND state IN ('dispatched','running','paused','waiting_user','cancelling')`, device)
	if err != nil {
		return err
	}
	type entry struct {
		id    string
		state proto.TaskState
	}
	var active []entry
	for rows.Next() {
		var item entry
		if err := rows.Scan(&item.id, &item.state); err != nil {
			rows.Close()
			return err
		}
		active = append(active, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range active {
		state, code := proto.Failed, proto.ErrInternal
		if item.state == proto.Cancelling {
			state, code = proto.Cancelled, proto.ErrCancelled
		}
		if err := addTaskEvent(ctx, tx, item.id, "reconnect", "missing", "节点重新接入，上一会话任务上下文已重置", now); err != nil {
			return err
		}
		if err := finishTask(ctx, tx, item.id, state, code, now); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET assign_seq=NULL WHERE device_id=? AND state='queued' AND assign_seq IS NOT NULL`, device)
	return err
}

func touchTask(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	_, err := tx.ExecContext(ctx, "UPDATE tasks SET last_heartbeat_at=?,stalled=0 WHERE id=?", now, id)
	return err
}

// Older M3 agents can advance their reliable sequence by receiving equivalent cancellations.
func ReconcileLegacyBrain(ctx context.Context, tx *sql.Tx, device string, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET envelope_json=json_set(envelope_json,'$.type','task.cancel','$.payload',
		json_object('task_id',json_extract(envelope_json,'$.payload.task_id'))) WHERE device_id=? AND
		(json_extract(envelope_json,'$.type') IN ('mcp.tool_call','mcp.task_display','mcp.task_finish') OR
		(json_extract(envelope_json,'$.type')='task.assign' AND json_extract(envelope_json,'$.payload.capability')=?))`,
		device, proto.BrainTask); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE device_id=? AND source='brain' AND assign_seq IS NOT NULL
		AND state IN ('queued','dispatched','running','paused','waiting_user','cancelling')`, device)
	if err != nil {
		return err
	}
	var tasks []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range tasks {
		if err := addTaskEvent(ctx, tx, id, "reconnect", "unsupported", "重连的 Core 版本不支持 AI 任务", now); err != nil {
			return err
		}
		if err := finishTask(ctx, tx, id, proto.Failed, proto.ErrCapUnsupported, now); err != nil {
			return err
		}
	}
	return nil
}

func ApplyTaskHeartbeat(ctx context.Context, tx *sql.Tx, device string, payload json.RawMessage, now int64) error {
	var status proto.TaskStatus
	if json.Unmarshal(payload, &status) != nil || !proto.TaskIDPattern.MatchString(status.TaskID) || !status.HeartbeatOnly {
		return ErrTaskMessage
	}
	var stalled bool
	err := tx.QueryRowContext(ctx, "SELECT stalled FROM tasks WHERE id=? AND device_id=? AND state IN "+runningTaskStates,
		status.TaskID, device).Scan(&stalled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := touchTask(ctx, tx, status.TaskID, now); err != nil {
		return err
	}
	if stalled {
		return RecordChange(ctx, tx, device)
	}
	return nil
}

func (s *Service) nextStall(ctx context.Context) (*int64, error) {
	var next *int64
	err := s.store.QueryRowContext(ctx, `SELECT MIN(t.last_heartbeat_at+MAX(30000,t.step_timeout_ms)) FROM tasks t
		JOIN devices d ON d.id=t.device_id WHERE d.is_demo=0 AND t.source<>'chat' AND t.stalled=0 AND t.state IN `+runningTaskStates).Scan(&next)
	return next, err
}

func (s *Service) markStalled(ctx context.Context) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `UPDATE tasks SET stalled=1 WHERE source<>'chat' AND stalled=0 AND state IN `+runningTaskStates+`
			AND last_heartbeat_at+MAX(30000,step_timeout_ms)<=? AND device_id IN (SELECT id FROM devices WHERE is_demo=0)
			RETURNING device_id`, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		devices := make(map[string]bool)
		for rows.Next() {
			var device string
			if err := rows.Scan(&device); err != nil {
				rows.Close()
				return err
			}
			devices[device] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for device := range devices {
			if err := RecordChange(ctx, tx, device); err != nil {
				return err
			}
		}
		return nil
	})
}
