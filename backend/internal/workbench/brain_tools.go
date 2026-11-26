package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"ghosthand/proto"
)

func (s *Service) showBrain(ctx context.Context, t brainTask, step int, label string) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		if err := checkBrainReady(ctx, tx, t.id); err != nil {
			return err
		}
		_, err := Enqueue(ctx, tx, t.device, proto.MCPTaskDisplay, proto.TaskDisplay{
			TaskID: t.id, Title: t.title, StepIndex: step, StepTotal: step, StepName: label, State: proto.Running,
		})
		return err
	})
}

func toolTimeout(tool string, args json.RawMessage) time.Duration {
	timeout := 75 * time.Second
	if tool == proto.ToolBrowserAct {
		if parsed, err := proto.ValidateBrowserTool(tool, args); err == nil &&
			(parsed.Action == "screenshot_element" || parsed.Action == "screenshot_tab") {
			return 95 * time.Second
		}
	}
	if tool == proto.ToolShellExec || tool == proto.ToolScriptRun {
		if parsed, err := proto.ValidateTool(tool, args); err == nil && parsed.TimeoutSec > 0 {
			timeout = max(timeout, time.Duration(parsed.TimeoutSec)*time.Second+20*time.Second)
		} else if tool == proto.ToolScriptRun {
			timeout = 140 * time.Second
		}
	}
	return timeout
}

func (s *Service) callDevice(ctx context.Context, t brainTask, step, attempt int, tool string, args json.RawMessage, label, window string, approved bool) (proto.MCPResult, error) {
	id, err := proto.NewID()
	if err != nil {
		return proto.MCPResult{}, err
	}
	timeout := toolTimeout(tool, args)
	now := time.Now().UnixMilli()
	display := proto.TaskDisplay{TaskID: t.id, Title: t.title, StepIndex: step, StepTotal: step, StepName: label, State: proto.Running}
	call := proto.MCPCall{CallID: id, TaskID: t.id, Tool: tool, Arguments: args, Display: display, Approved: approved, Window: window}
	for {
		if err := s.awaitBrain(ctx, t.id); err != nil {
			return proto.MCPResult{}, err
		}
		err = s.store.Write(ctx, func(tx *sql.Tx) error {
			if err := checkBrainReady(ctx, tx, t.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO brain_calls(id,task_id,tool,created_at) VALUES (?,?,?,?)", id, t.id, tool, now); err != nil {
				return err
			}
			payload, _ := json.Marshal(proto.TaskProgressPayload{TaskID: t.id, StepIndex: step, StepTotal: step,
				StepKey: tool, StepName: label, Status: "running", Attempt: attempt, Detail: label, TimeoutMS: timeout.Milliseconds()})
			if err := ApplyTaskReport(ctx, tx, t.device, proto.Envelope{Type: proto.TaskProgress, Payload: payload}, now); err != nil {
				return err
			}
			if _, err := Enqueue(ctx, tx, t.device, proto.MCPToolCall, call); err != nil {
				return err
			}
			return RecordChange(ctx, tx, t.device)
		})
		if !errors.Is(err, errBrainWait) {
			break
		}
	}
	if err != nil {
		return proto.MCPResult{}, err
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for wait.Err() == nil {
		changed := s.store.Changed()
		var result sql.NullString
		if err := s.store.QueryRowContext(wait, "SELECT result_json FROM brain_calls WHERE id=? AND task_id=?", id, t.id).Scan(&result); err != nil {
			return proto.MCPResult{}, err
		}
		if result.Valid {
			var value proto.MCPResult
			err := json.Unmarshal([]byte(result.String), &value)
			return value, err
		}
		select {
		case <-wait.Done():
		case <-changed:
		}
	}
	return proto.MCPResult{}, wait.Err()
}

func recordToolResult(ctx context.Context, tx *sql.Tx, device, task string, raw json.RawMessage, now int64) error {
	var result proto.MCPResult
	if json.Unmarshal(raw, &result) != nil || len(result.CallID) != 26 || len(result.Data) > proto.MaxToolDataSize || len(result.Error) > 512 ||
		(result.AttachmentID != "" && len(result.AttachmentID) != 26) {
		return ErrTaskMessage
	}
	var tool string
	var existing sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT tool,result_json FROM brain_calls WHERE id=? AND task_id=?", result.CallID, task).Scan(&tool, &existing)
	if errors.Is(err, sql.ErrNoRows) || existing.Valid {
		return nil
	}
	if err != nil {
		return err
	}
	if result.AttachmentID != "" {
		if tool != proto.ToolObserve && tool != proto.ToolBrowserAct {
			return ErrTaskMessage
		}
		var media MediaCard
		err := tx.QueryRowContext(ctx, "SELECT id,name,size FROM attachments WHERE id=? AND task_id=? AND upload_id=?",
			result.AttachmentID, task, result.CallID).Scan(&media.ID, &media.Name, &media.Size)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTaskMessage
		}
		if err != nil {
			return err
		}
		body, _ := json.Marshal(media)
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,task_id,payload_json,created_at)
			VALUES (?,'agent','media',?,?,?)`, device, task, string(body), now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE brain_calls SET result_json=? WHERE id=?", string(raw), result.CallID); err != nil {
		return err
	}
	status, detail := "succeeded", ""
	if hint := confirmationHint(result.Error); hint != "" {
		status, detail = "waiting_user", hint
	} else if result.Error != "" {
		status, detail = "failed", result.Error
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_events SET status=?,detail=?,elapsed_ms=MAX(elapsed_ms,?-created_at)
		WHERE task_id=? AND seq=(SELECT MAX(seq) FROM task_events WHERE task_id=? AND status='running' AND step_key=?)`,
		status, detail, now, task, task, tool); err != nil {
		return err
	}
	return touchTask(ctx, tx, task, now)
}

func (s *Service) finishBrain(ctx context.Context, t brainTask, reply string, state proto.TaskState) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		var current proto.TaskState
		if err := tx.QueryRowContext(ctx, "SELECT state FROM tasks WHERE id=?", t.id).Scan(&current); err != nil {
			return err
		}
		if !brainActive(current) {
			return nil
		}
		now := time.Now().UnixMilli()
		code := proto.ErrorCode("")
		switch state {
		case proto.Failed:
			code = proto.ErrInternal
		case proto.Cancelled:
			code = proto.ErrCancelled
		}
		if _, err := Enqueue(ctx, tx, t.device, proto.MCPTaskFinish, proto.TaskResultPayload{TaskID: t.id, State: state, ErrorCode: code}); err != nil {
			return err
		}
		if !t.quiet {
			if err := addReply(ctx, tx, t.device, t.id, limitText(reply, 5000), now); err != nil {
				return err
			}
		}
		if err := finishTask(ctx, tx, t.id, state, code, now); err != nil {
			return err
		}
		return RecordChange(ctx, tx, t.device)
	})
}
