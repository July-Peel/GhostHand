package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"ghosthand/proto"
)

var ErrTaskMessage = errors.New("invalid task message")

type StatusSnapshot struct {
	Title        string `json:"title"`
	OSVersion    string `json:"os_version"`
	Arch         string `json:"arch"`
	BatteryPct   *int   `json:"battery_pct"`
	Charging     *bool  `json:"charging"`
	DesktopState string `json:"desktop_state"`
}

type MediaCard struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type activeTask struct {
	state      proto.TaskState
	capability proto.CapabilityID
	step       int
	attempt    int
	eventSeq   int64
}

func ApplyTaskReport(ctx context.Context, tx *sql.Tx, device string, frame proto.Envelope, now int64) error {
	var ref proto.TaskRef
	if err := json.Unmarshal(frame.Payload, &ref); err != nil || !proto.TaskIDPattern.MatchString(ref.TaskID) {
		return ErrTaskMessage
	}
	var task activeTask
	err := tx.QueryRowContext(ctx, `SELECT t.state,t.capability,t.step_index,COALESCE(e.attempt,0),COALESCE(e.seq,0)
		FROM tasks t LEFT JOIN task_events e ON e.task_id=t.id AND e.seq=(
			SELECT MAX(seq) FROM task_events WHERE task_id=t.id AND attempt>0)
		WHERE t.id=? AND t.device_id=? AND t.assign_seq IS NOT NULL
		AND t.state IN ('queued','dispatched','running','paused','waiting_user','cancelling')`, ref.TaskID, device).
		Scan(&task.state, &task.capability, &task.step, &task.attempt, &task.eventSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch frame.Type {
	case proto.TaskAccepted:
		if task.state == proto.Cancelling {
			return nil
		}
		if task.state == proto.Queued || task.state == proto.Dispatched {
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='running',started_at=?,last_heartbeat_at=? WHERE id=?", now, now, ref.TaskID); err != nil {
				return err
			}
			return addTaskEvent(ctx, tx, ref.TaskID, "queue", "accepted", "设备已接收任务", now)
		}
	case proto.TaskHeartbeat:
		var report proto.TaskStatus
		if json.Unmarshal(frame.Payload, &report) != nil ||
			(report.State != proto.Running && report.State != proto.Paused && report.State != proto.Cancelling) {
			return ErrTaskMessage
		}
		if err := touchTask(ctx, tx, ref.TaskID, now); err != nil {
			return err
		}
		if task.state == report.State || (task.state != proto.Running && task.state != proto.Paused &&
			!(task.state == proto.WaitingUser && report.State == proto.Cancelling)) {
			return nil
		}
		if report.State == proto.Cancelling {
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='cancelling',control='cancel',cancel_at=? WHERE id=?",
				now, ref.TaskID); err != nil {
				return err
			}
			return addTaskEvent(ctx, tx, ref.TaskID, "control", "cancelling", "设备端请求终止任务", now)
		}
		detail := "任务已继续"
		if report.State == proto.Paused {
			detail = "任务已暂停"
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state=?,control='' WHERE id=?", report.State, ref.TaskID); err != nil {
			return err
		}
		return addTaskEvent(ctx, tx, ref.TaskID, "control", string(report.State), detail, now)
	case proto.TaskProgress:
		var report proto.TaskProgressPayload
		if json.Unmarshal(frame.Payload, &report) != nil || report.StepIndex < 1 ||
			report.StepIndex > report.StepTotal || report.StepTotal > 1000 || report.Status != "running" ||
			report.Attempt < 1 || report.Attempt > 100 || report.ElapsedMS < 0 || report.TimeoutMS < 0 || report.TimeoutMS > 600000 ||
			len(report.StepKey) == 0 || len(report.StepKey) > 40 || len(report.StepName) > 120 || len(report.Detail) > 512 {
			return ErrTaskMessage
		}
		if task.state != proto.Running || report.StepIndex < task.step || report.StepIndex > task.step+1 ||
			(report.StepIndex == task.step && report.Attempt < task.attempt) {
			return nil
		}
		return recordProgress(ctx, tx, task, report, now)
	case proto.TaskResult:
		var result proto.TaskResultPayload
		if json.Unmarshal(frame.Payload, &result) != nil || len(result.ErrorCode) > 64 {
			return ErrTaskMessage
		}
		switch result.State {
		case proto.Succeeded, proto.Failed, proto.Cancelled, proto.Unknown:
		case proto.Expired:
			if task.step != 0 {
				return ErrTaskMessage
			}
		default:
			return ErrTaskMessage
		}
		if task.state == proto.Cancelling && result.State != proto.Succeeded {
			result.State, result.ErrorCode = proto.Cancelled, proto.ErrCancelled
		}
		if result.State == proto.Succeeded && task.capability != proto.BrainTask {
			if err := publishResult(ctx, tx, device, task.capability, result, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM screenshot_tickets WHERE task_id=?", ref.TaskID); err != nil {
			return err
		}
		if err := finishStep(ctx, tx, ref.TaskID, task, result.State, now); err != nil {
			return err
		}
		return finishTask(ctx, tx, ref.TaskID, result.State, result.ErrorCode, now)
	case proto.UploadRequest:
		var request proto.ScreenshotRequest
		if json.Unmarshal(frame.Payload, &request) != nil || len(request.RequestID) != 26 {
			return ErrTaskMessage
		}
		if task.capability == proto.BrainTask {
			var allowed bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM brain_calls
				WHERE id=? AND task_id=? AND tool IN (?,?) AND result_json IS NULL)`, request.UploadID, ref.TaskID, proto.ToolObserve, proto.ToolBrowserAct).
				Scan(&allowed); err != nil || !allowed {
				return err
			}
		} else if task.capability != proto.ScreenCapture || request.UploadID != "" {
			return ErrTaskMessage
		}
		if task.state != proto.Running && task.state != proto.Paused {
			return nil
		}
		return issueScreenshotTicket(ctx, tx, device, request, now)
	case proto.MCPToolResult:
		if task.capability != proto.BrainTask {
			return ErrTaskMessage
		}
		return recordToolResult(ctx, tx, device, ref.TaskID, frame.Payload, now)
	}
	return nil
}

func recordProgress(ctx context.Context, tx *sql.Tx, task activeTask, report proto.TaskProgressPayload, now int64) error {
	sameAttempt := report.StepIndex == task.step && report.Attempt == task.attempt
	if !sameAttempt {
		status := proto.Succeeded
		if report.StepIndex == task.step {
			status = proto.Failed
		}
		if err := finishStep(ctx, tx, report.TaskID, task, status, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET step_index=?,step_total=?,step_name=?,elapsed_ms=MAX(elapsed_ms,?),
		last_heartbeat_at=?,stalled=0,step_timeout_ms=? WHERE id=?`, report.StepIndex, report.StepTotal,
		report.StepName, report.ElapsedMS, now, max(30000, report.TimeoutMS), report.TaskID); err != nil {
		return err
	}
	if sameAttempt && task.eventSeq != 0 {
		_, err := tx.ExecContext(ctx, `UPDATE task_events SET step_key=?,step_name=?,detail=?,elapsed_ms=MAX(elapsed_ms,?,?-created_at)
			WHERE task_id=? AND seq=? AND status='running'`, report.StepKey, report.StepName, report.Detail,
			report.ElapsedMS, now, report.TaskID, task.eventSeq)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO task_events(task_id,seq,step_key,step_name,status,detail,elapsed_ms,attempt,created_at)
		SELECT ?,COALESCE(MAX(seq),0)+1,?,?,'running',?,0,?,? FROM task_events WHERE task_id=?`,
		report.TaskID, report.StepKey, report.StepName, report.Detail, report.Attempt, now, report.TaskID)
	return err
}

func finishStep(ctx context.Context, tx *sql.Tx, id string, task activeTask, state proto.TaskState, now int64) error {
	if task.step == 0 || task.eventSeq == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE task_events SET status=?,elapsed_ms=MAX(elapsed_ms,?-created_at)
		WHERE task_id=? AND seq=? AND status='running'`, state, now, id, task.eventSeq)
	return err
}

func publishResult(ctx context.Context, tx *sql.Tx, device string, capability proto.CapabilityID, result proto.TaskResultPayload, now int64) error {
	var payload any
	kind := "status_card"
	if capability == proto.ScreenCapture {
		var reply proto.ScreenshotResult
		if json.Unmarshal(result.Result, &reply) != nil {
			return ErrTaskMessage
		}
		var media MediaCard
		err := tx.QueryRowContext(ctx, "SELECT id,name,size FROM attachments WHERE id=? AND task_id=?",
			reply.AttachmentID, result.TaskID).Scan(&media.ID, &media.Name, &media.Size)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTaskMessage
		}
		if err != nil {
			return err
		}
		payload, kind = media, "media"
	} else {
		var state proto.DeviceState
		if json.Unmarshal(result.Result, &state) != nil ||
			(state.BatteryPct != nil && (*state.BatteryPct < 0 || *state.BatteryPct > 100)) {
			return ErrTaskMessage
		}
		var snapshot StatusSnapshot
		err := tx.QueryRowContext(ctx, "SELECT os_version,arch,desktop_state FROM devices WHERE id=?", device).
			Scan(&snapshot.OSVersion, &snapshot.Arch, &snapshot.DesktopState)
		if err != nil {
			return err
		}
		snapshot.Title = commandFor(capability).Title
		snapshot.BatteryPct, snapshot.Charging = state.BatteryPct, state.Charging
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET battery_pct=?,charging=? WHERE id=?",
			state.BatteryPct, state.Charging, device); err != nil {
			return err
		}
		payload = snapshot
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,task_id,payload_json,created_at)
		VALUES (?,'agent',?,?,?,?)`, device, kind, result.TaskID, string(raw), now)
	return err
}

func issueScreenshotTicket(ctx context.Context, tx *sql.Tx, device string, request proto.ScreenshotRequest, now int64) error {
	ticket := proto.ScreenshotTicket{TaskID: request.TaskID, RequestID: request.RequestID, UploadID: request.UploadID}
	err := tx.QueryRowContext(ctx, "SELECT id FROM attachments WHERE task_id=? AND upload_id=?", request.TaskID, request.UploadID).
		Scan(&ticket.AttachmentID)
	if errors.Is(err, sql.ErrNoRows) {
		token, err := proto.NewToken()
		if err != nil {
			return err
		}
		hash, err := proto.TokenHash(token)
		if err != nil {
			return err
		}
		ticket.Token, ticket.ExpiresAt = token, now+(2*time.Minute).Milliseconds()
		if _, err := tx.ExecContext(ctx, `INSERT INTO screenshot_tickets(task_id,upload_id,token_hash,expires_at) VALUES (?,?,?,?)
			ON CONFLICT(task_id,upload_id) DO UPDATE SET token_hash=excluded.token_hash,expires_at=excluded.expires_at`,
			request.TaskID, request.UploadID, hash, ticket.ExpiresAt); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = Enqueue(ctx, tx, device, proto.UploadTicket, ticket)
	return err
}
