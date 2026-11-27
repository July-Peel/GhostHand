package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

type HistoryEntry struct {
	Generation     uint64  `json:"generation"`
	ConnectedAt    int64   `json:"connected_at"`
	DisconnectedAt *int64  `json:"disconnected_at"`
	Reason         *string `json:"reason"`
	RemoteIP       string  `json:"remote_ip"`
	AgentVersion   string  `json:"agent_ver"`
}

type OperationEntry struct {
	ID           string            `json:"id"`
	Kind         proto.MessageType `json:"kind"`
	State        string            `json:"state"`
	Detail       string            `json:"detail"`
	CreatedAt    int64             `json:"created_at"`
	FinishedAt   *int64            `json:"finished_at"`
	LogAvailable bool              `json:"log_available"`
}

type RealtimeDevice struct {
	Version      string           `json:"version"`
	Revoked      bool             `json:"revoked"`
	CanUninstall bool             `json:"can_uninstall"`
	Sessions     []HistoryEntry   `json:"sessions"`
	Operations   []OperationEntry `json:"operations"`
}

func (s *Service) History(ctx context.Context, id string) (RealtimeDevice, error) {
	result := RealtimeDevice{Sessions: []HistoryEntry{}, Operations: []OperationEntry{}}
	var platform proto.Platform
	var capsHash string
	err := s.store.QueryRowContext(ctx, `SELECT d.agent_ver,c.revoked_at IS NOT NULL,d.platform,d.caps_hash
		FROM devices d JOIN device_credentials c ON c.device_id=d.id WHERE d.id=?`, id).
		Scan(&result.Version, &result.Revoked, &platform, &capsHash)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrMissing
	}
	if err != nil {
		return result, err
	}
	result.CanUninstall = (platform == proto.Windows || platform == proto.Linux) && proto.SupportsUninstall(capsHash)
	rows, err := s.store.QueryContext(ctx, `SELECT id,kind,state,detail,created_at,finished_at,uploaded_at IS NOT NULL
		FROM device_operations WHERE device_id=? ORDER BY created_at DESC,id DESC LIMIT 50`, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry OperationEntry
		if err := rows.Scan(&entry.ID, &entry.Kind, &entry.State, &entry.Detail, &entry.CreatedAt, &entry.FinishedAt, &entry.LogAvailable); err != nil {
			return result, err
		}
		result.Operations = append(result.Operations, entry)
	}
	return result, rows.Err()
}

func (s *Service) Revoke(ctx context.Context, user int64, id string) error {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		result, err := tx.ExecContext(ctx, "UPDATE device_credentials SET revoked_at=?,pending_hash=NULL WHERE device_id=? AND revoked_at IS NULL", now, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrMissing
		}
		if _, err := tx.ExecContext(ctx, "UPDATE device_operations SET state='failed',detail='凭证已吊销',finished_at=?,ticket_hash=NULL WHERE device_id=? AND state='queued'", now, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM device_sessions WHERE device_id=?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET conn_state='offline' WHERE id=?", id); err != nil {
			return err
		}
		if err := audit(ctx, tx, user, id, "device.revoke", "{}", now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, id)
	})
	if err == nil {
		if p := s.connections[id]; p != nil {
			_ = p.conn.CloseNow()
		}
		go s.disconnectSSHIfManaged(context.Background(), id)
	}
	return err
}

func (s *Service) Restore(ctx context.Context, user int64, id string) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		result, err := tx.ExecContext(ctx, "UPDATE device_credentials SET revoked_at=NULL WHERE device_id=? AND revoked_at IS NOT NULL", id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrMissing
		}
		if err := audit(ctx, tx, user, id, "device.restore", "{}", now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, id)
	})
}

// DeleteDevice removes an offline device and all its associated records and agent logs.
func (s *Service) DeleteDevice(ctx context.Context, user int64, id string) ([]workbench.AttachmentFiles, error) {
	if id == workbench.FreeChatDeviceID {
		return nil, ErrMissing
	}
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	if s.connections[id] != nil {
		return nil, ErrDeviceOnline
	}
	var files []workbench.AttachmentFiles
	var logIDs []string
	var sshHostAgent sql.NullString
	var isSSH bool
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var connState proto.ConnState
		var name string
		err := tx.QueryRowContext(ctx, "SELECT conn_state, name FROM devices WHERE id=?", id).Scan(&connState, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		if connState == proto.Online {
			return ErrDeviceOnline
		}
		if err := tx.QueryRowContext(ctx, "SELECT host_agent_id FROM device_ssh_credentials WHERE device_id=?", id).Scan(&sshHostAgent); err == nil {
			isSSH = true
		}

		files, err = workbench.DeviceAttachmentFiles(ctx, tx, id)
		if err != nil {
			return err
		}

		logRows, err := tx.QueryContext(ctx, `SELECT id FROM device_operations
			WHERE device_id=? AND uploaded_at IS NOT NULL`, id)
		if err != nil {
			return err
		}
		for logRows.Next() {
			var opID string
			if err := logRows.Scan(&opID); err != nil {
				logRows.Close()
				return err
			}
			logIDs = append(logIDs, opID)
		}
		logRows.Close()
		if err := logRows.Err(); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, "DELETE FROM pairing_codes WHERE device_id=?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM audit_logs WHERE device_id=?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM devices WHERE id=?", id); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]string{"deleted_device_id": id, "name": name})
		return audit(ctx, tx, user, nil, "device.delete", string(detail), time.Now().UnixMilli())
	})
	if err != nil {
		return nil, err
	}
	if isSSH {
		s.dispatchSSHDisconnect(sshHostAgent.String, id)
		s.closeLocalSSHTerminals(id)
	}
	logDir := filepath.Join(s.dir, "logs")
	for _, opID := range logIDs {
		_ = os.Remove(filepath.Join(logDir, opID+".jsonl.gz"))
	}
	return files, nil
}

type OperationRequest struct {
	Kind  proto.MessageType `json:"kind"`
	Hours int               `json:"hours,omitempty"`
}

func (s *Service) Queue(ctx context.Context, user int64, id string, input OperationRequest) (string, error) {
	switch input.Kind {
	case proto.LogCollect:
		if input.Hours < 1 || input.Hours > 168 {
			return "", ErrInvalid
		}
	case proto.AgentRestart, proto.HelperRestart, proto.AgentRollback:
	case proto.AgentUninstall:
		if input.Hours != 0 {
			return "", ErrInvalid
		}
	default:
		return "", ErrInvalid
	}
	var operationID string
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var platform proto.Platform
		var capsHash string
		err := tx.QueryRowContext(ctx, `SELECT d.platform,d.caps_hash FROM devices d JOIN device_credentials c ON c.device_id=d.id
			WHERE d.id=? AND c.revoked_at IS NULL`, id).Scan(&platform, &capsHash)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		if (input.Kind == proto.HelperRestart || input.Kind == proto.AgentRollback) && platform != proto.Windows {
			return ErrInvalid
		}
		if input.Kind == proto.AgentUninstall && ((platform != proto.Windows && platform != proto.Linux) || !proto.SupportsUninstall(capsHash)) {
			return ErrUninstallUnsupported
		}
		operationID, err = queue(ctx, tx, user, id, input.Kind, input.Hours)
		return err
	})
	return operationID, err
}

func queue(ctx context.Context, tx *sql.Tx, user int64, device string, kind proto.MessageType, hours int) (string, error) {
	var busy bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM device_operations WHERE device_id=? AND state='queued'
		AND (kind IN (?, 'agent.uninstall') OR ?='agent.uninstall'
		OR (kind IN ('agent.update','agent.rollback','agent.restart') AND ? IN ('agent.update','agent.rollback','agent.restart'))))`,
		device, kind, kind, kind).Scan(&busy)
	if err != nil {
		return "", err
	}
	if busy {
		return "", ErrConflict
	}
	id, err := proto.NewID()
	if err != nil {
		return "", err
	}
	if _, err := workbench.Enqueue(ctx, tx, device, kind, proto.Operation{ID: id, Hours: hours}); err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_operations(id,device_id,kind,created_at) VALUES (?,?,?,?)`,
		id, device, kind, now); err != nil {
		return "", err
	}
	detail, _ := json.Marshal(map[string]any{"operation_id": id, "hours": hours})
	if err := audit(ctx, tx, user, device, string(kind), string(detail), now); err != nil {
		return "", err
	}
	return id, workbench.RecordChange(ctx, tx, device)
}

func storeFile(dir string, body io.Reader, limit int64) (string, error) {
	file, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return "", err
	}
	size, err := io.Copy(file, io.LimitReader(body, limit+1))
	if err == nil && (size == 0 || size > limit) {
		err = ErrInvalid
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

func (s *Service) PackagePath(ctx context.Context, device, token, id string) (string, error) {
	if _, _, err := authenticate(ctx, s.store, device, token); err != nil {
		return "", err
	}
	var found bool
	err := s.store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM device_operations
		WHERE device_id=? AND release_id=? AND state='queued')`, device, id).Scan(&found)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrMissing
	}
	return filepath.Join(s.dir, "packages", id+".zip"), nil
}

func (s *Service) IssueLogTicket(ctx context.Context, device, token, id string) (proto.LogTicket, error) {
	var ticket proto.LogTicket
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		if _, _, err := authenticate(ctx, tx, device, token); err != nil {
			return err
		}
		var uploaded bool
		err := tx.QueryRowContext(ctx, `SELECT uploaded_at IS NOT NULL FROM device_operations
			WHERE device_id=? AND id=? AND kind='log.collect' AND state='queued'`, device, id).Scan(&uploaded)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil || uploaded {
			return err
		}
		raw, err := proto.NewToken()
		if err != nil {
			return err
		}
		hash, err := proto.TokenHash(raw)
		if err != nil {
			return err
		}
		ticket = proto.LogTicket{Token: raw, ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli()}
		_, err = tx.ExecContext(ctx, "UPDATE device_operations SET ticket_hash=?,ticket_expiry=? WHERE id=?", hash, ticket.ExpiresAt, id)
		return err
	})
	return ticket, err
}

func (s *Service) SaveLog(ctx context.Context, id, token string, body io.Reader) error {
	hash, err := proto.TokenHash(token)
	if err != nil {
		return ErrDenied
	}
	now := time.Now().UnixMilli()
	where := " WHERE id=? AND ticket_hash=? AND ticket_expiry>? AND uploaded_at IS NULL AND state='queued' AND kind='log.collect'"
	var device string
	err = s.store.QueryRowContext(ctx, "SELECT device_id FROM device_operations"+where, id, hash, now).Scan(&device)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	dir := filepath.Join(s.dir, "logs")
	temp, err := storeFile(dir, body, proto.MaxLogSize)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		result, err := tx.ExecContext(ctx, "UPDATE device_operations SET uploaded_at=?,ticket_hash=NULL,ticket_expiry=NULL"+where, now, id, hash, now)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrDenied
		}
		if err := workbench.RecordChange(ctx, tx, device); err != nil {
			return err
		}
		return os.Rename(temp, filepath.Join(dir, id+".jsonl.gz"))
	})
}

func (s *Service) LogPath(ctx context.Context, device, id string) (string, error) {
	var found bool
	if err := s.store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM device_operations
		WHERE device_id=? AND id=? AND uploaded_at IS NOT NULL)`, device, id).Scan(&found); err != nil {
		return "", err
	}
	if !found {
		return "", ErrMissing
	}
	return filepath.Join(s.dir, "logs", id+".jsonl.gz"), nil
}
