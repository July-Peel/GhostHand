package workbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"ghosthand/proto"
)

type ConfirmationCard struct {
	ID           string `json:"id"`
	TaskID       string `json:"task_id"`
	Summary      string `json:"summary"`
	RiskAction   string `json:"risk_action"`
	AttachmentID string `json:"attachment_id,omitempty"`
}

func (s *Service) requestConfirmation(ctx context.Context, t brainTask, summary, risk, attachment string) (bool, error) {
	id, err := proto.NewID()
	if err != nil {
		return false, err
	}
	err = s.writeBrainReady(ctx, t.id, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		var step int
		if err := tx.QueryRowContext(ctx, `UPDATE tasks SET state='waiting_user' WHERE id=? RETURNING step_index`, t.id).Scan(&step); err != nil {
			return err
		}
		if attachment != "" {
			var exists bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attachments WHERE id=? AND task_id=?)", attachment, t.id).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				attachment = ""
			}
		}
		card := ConfirmationCard{ID: id, TaskID: t.id, Summary: summary, RiskAction: risk, AttachmentID: attachment}
		raw, _ := json.Marshal(card)
		if _, err := tx.ExecContext(ctx, "INSERT INTO confirmations(id,task_id,summary,created_at) VALUES (?,?,?,?)", id, t.id, summary, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,task_id,payload_json,created_at)
			VALUES (?,'agent','confirm_card',?,?,?)`, t.device, t.id, string(raw), now); err != nil {
			return err
		}
		if t.source != "chat" {
			if _, err := Enqueue(ctx, tx, t.device, proto.MCPTaskDisplay, proto.TaskDisplay{
				TaskID: t.id, Title: t.title, StepIndex: step, StepTotal: step, StepName: "等待管理员确认", State: proto.WaitingUser,
			}); err != nil {
				return err
			}
		}
		return RecordChange(ctx, tx, t.device)
	})
	if err != nil {
		return false, err
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for wait.Err() == nil {
		changed := s.store.Changed()
		var decision string
		if err := s.store.QueryRowContext(wait, "SELECT decision FROM confirmations WHERE id=?", id).Scan(&decision); err != nil {
			return false, err
		}
		switch decision {
		case "approved":
			return true, nil
		case "denied", "cancelled":
			return false, nil
		}
		select {
		case <-wait.Done():
		case <-changed:
		}
	}
	return false, wait.Err()
}

func (s *Service) Decide(ctx context.Context, user int64, device, id string, approve bool) error {
	var task, decision string
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var state proto.TaskState
		err := tx.QueryRowContext(ctx, `SELECT c.task_id,c.decision,t.state FROM confirmations c JOIN tasks t ON t.id=c.task_id
			WHERE c.id=? AND t.device_id=?`, id, device).Scan(&task, &decision, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTaskNotFound
		}
		if err != nil {
			return err
		}
		value := "denied"
		if approve {
			value = "approved"
		}
		if decision == value {
			return nil
		}
		if decision != "pending" || (state != proto.WaitingUser && state != proto.Paused) {
			return ErrTaskControl
		}
		now := time.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, "UPDATE confirmations SET decision=?,decided_at=? WHERE id=?", value, now, id); err != nil {
			return err
		}
		if !approve {
			if device != FreeChatDeviceID {
				if err := enqueueTaskCancel(ctx, tx, device, task, false, false); err != nil {
					return err
				}
			}
			if err := addReply(ctx, tx, device, task, errBrainDenied.Error(), now); err != nil {
				return err
			}
			if err := finishTask(ctx, tx, task, proto.Cancelled, proto.ErrCancelled, now); err != nil {
				return err
			}
		} else if state == proto.WaitingUser {
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET state='running' WHERE id=?", task); err != nil {
				return err
			}
		}
		if err := auditTask(ctx, tx, user, device, task, "task.confirm."+value, now); err != nil {
			return err
		}
		return RecordChange(ctx, tx, device)
	})
	if err == nil && !approve {
		s.cancelBrainSession(device, task)
	}
	return err
}
