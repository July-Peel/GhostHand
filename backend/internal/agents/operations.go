package agents

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

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
	result.CanUninstall = platform == proto.Windows && proto.SupportsUninstall(capsHash)
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
func (s *Service) DeleteDevice(ctx context.Context, user int64, id string) ([][2]string, error) {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	if s.connections[id] != nil {
		return nil, ErrDeviceOnline
	}
	var localFiles [][2]string
	var logIDs []string
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

		attRows, err := tx.QueryContext(ctx, `SELECT a.storage_key, a.thumb_key
			FROM attachments a JOIN tasks t ON t.id=a.task_id
			WHERE t.device_id=? AND a.storage_backend='local'`, id)
		if err != nil {
			return err
		}
		for attRows.Next() {
			var pair [2]string
			if err := attRows.Scan(&pair[0], &pair[1]); err != nil {
				attRows.Close()
				return err
			}
			localFiles = append(localFiles, pair)
		}
		attRows.Close()
		if err := attRows.Err(); err != nil {
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
	logDir := filepath.Join(s.dir, "logs")
	for _, opID := range logIDs {
		_ = os.Remove(filepath.Join(logDir, opID+".jsonl.gz"))
	}
	return localFiles, nil
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
		if input.Kind == proto.AgentUninstall && (platform != proto.Windows || !proto.SupportsUninstall(capsHash)) {
			return ErrUninstallUnsupported
		}
		operationID, err = queue(ctx, tx, user, id, input.Kind, input.Hours, nil)
		return err
	})
	return operationID, err
}

func queue(ctx context.Context, tx *sql.Tx, user int64, device string, kind proto.MessageType, hours int, release *proto.Release) (string, error) {
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
	if _, err := workbench.Enqueue(ctx, tx, device, kind, proto.Operation{ID: id, Hours: hours, Release: release}); err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	var releaseID any
	if release != nil {
		releaseID = release.ID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_operations(id,device_id,kind,release_id,created_at) VALUES (?,?,?,?,?)`,
		id, device, kind, releaseID, now); err != nil {
		return "", err
	}
	detail, _ := json.Marshal(map[string]any{"operation_id": id, "hours": hours, "release_id": releaseID})
	if err := audit(ctx, tx, user, device, string(kind), string(detail), now); err != nil {
		return "", err
	}
	return id, workbench.RecordChange(ctx, tx, device)
}

var versionPattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$")

const releaseFields = "id,version,platform,arch,component,size,sha256,signature"

func scanRelease(row interface{ Scan(...any) error }) (r proto.Release, err error) {
	err = row.Scan(&r.ID, &r.Version, &r.Platform, &r.Arch, &r.Component, &r.Size, &r.SHA256, &r.Signature)
	return
}

func (s *Service) Releases(ctx context.Context) ([]proto.Release, error) {
	rows, err := s.store.QueryContext(ctx, "SELECT "+releaseFields+" FROM agent_releases ORDER BY created_at DESC,id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []proto.Release{}
	for rows.Next() {
		entry, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func storeFile(dir string, body io.Reader, limit int64) (fileName, digest string, size int64, err error) {
	file, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return "", "", 0, err
	}
	hash := sha256.New()
	size, err = io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, limit+1))
	if err == nil && (size == 0 || size > limit) {
		err = ErrInvalid
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		_ = os.Remove(file.Name())
		return "", "", 0, err
	}
	return file.Name(), hex.EncodeToString(hash.Sum(nil)), size, nil
}

func (s *Service) SaveRelease(ctx context.Context, user int64, version, component, expectedHash string, body io.Reader) (proto.Release, error) {
	if !versionPattern.MatchString(version) || (component != "core" && component != "helper_win") || len(expectedHash) != 64 {
		return proto.Release{}, ErrInvalid
	}
	dir := filepath.Join(s.dir, "packages")
	temp, digest, size, err := storeFile(dir, body, proto.MaxReleaseSize)
	if err != nil {
		return proto.Release{}, err
	}
	defer os.Remove(temp)
	if !strings.EqualFold(expectedHash, digest) || !validPackage(temp, component) {
		return proto.Release{}, ErrInvalid
	}
	id, err := proto.NewID()
	if err != nil {
		return proto.Release{}, err
	}
	release := proto.Release{ID: id, Version: version, Platform: proto.Windows, Arch: "amd64", Component: component, Size: size, SHA256: digest}
	release.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, release.SignedBytes()))
	path := filepath.Join(dir, id+".zip")
	if err := os.Rename(temp, path); err != nil {
		return proto.Release{}, err
	}
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_releases(`+releaseFields+`,created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, id, version, release.Platform, release.Arch, component, size, digest, release.Signature, now)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrConflict
		}
		return audit(ctx, tx, user, nil, "release.create", "{\"release_id\":\""+id+"\"}", now)
	})
	if err != nil {
		_ = os.Remove(path)
	}
	return release, err
}

func validPackage(path, component string) bool {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return false
	}
	defer archive.Close()
	required := "ghosthand-core.exe"
	if component == "helper_win" {
		required = "GhostHand.Helper.exe"
	}
	seen := make(map[string]bool)
	for _, file := range archive.File {
		if !file.Mode().IsRegular() || file.UncompressedSize64 > proto.MaxReleaseSize ||
			(file.Name != required && !(component == "core" && file.Name == "GhostHand.Helper.exe")) || seen[file.Name] {
			return false
		}
		src, err := file.Open()
		if err != nil {
			return false
		}
		var header [2]byte
		_, err = io.ReadFull(src, header[:])
		src.Close()
		if err != nil || string(header[:]) != "MZ" {
			return false
		}
		seen[file.Name] = true
	}
	return seen[required]
}

type Rollout struct {
	DeviceID string `json:"device_id,omitempty"`
	Group    string `json:"group,omitempty"`
	All      bool   `json:"all,omitempty"`
}

func (s *Service) Rollout(ctx context.Context, user int64, id string, scope Rollout) (int, error) {
	if scope.DeviceID == "" && scope.Group == "" && !scope.All ||
		scope.DeviceID != "" && (scope.Group != "" || scope.All) || scope.Group != "" && scope.All ||
		utf8.RuneCountInString(scope.Group) > 80 {
		return 0, ErrInvalid
	}
	count := 0
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		release, err := scanRelease(tx.QueryRowContext(ctx, "SELECT "+releaseFields+" FROM agent_releases WHERE id=?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT d.id FROM devices d JOIN device_credentials c ON d.id=c.device_id
			WHERE c.revoked_at IS NULL AND d.platform=? AND d.arch=? AND (? OR d.id=? OR (?<>'' AND d.group_name=?))`,
			release.Platform, release.Arch, scope.All, scope.DeviceID, scope.Group, scope.Group)
		if err != nil {
			return err
		}
		var devices []string
		for rows.Next() {
			var device string
			if err := rows.Scan(&device); err != nil {
				rows.Close()
				return err
			}
			devices = append(devices, device)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(devices) == 0 {
			return ErrMissing
		}
		for _, device := range devices {
			if _, err := queue(ctx, tx, user, device, proto.AgentUpdate, 0, &release); err != nil {
				return err
			}
		}
		count = len(devices)
		return nil
	})
	return count, err
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
	temp, _, _, err := storeFile(dir, body, proto.MaxLogSize)
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
