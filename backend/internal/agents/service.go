package agents

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
	"ghosthand/proto/sshrunner"
)

var (
	ErrInvalid              = errors.New("invalid request")
	ErrDenied               = errors.New("device authentication failed")
	ErrLimited              = errors.New("too many pairing attempts")
	ErrMissing              = errors.New("not found")
	ErrConflict             = errors.New("operation conflicts with existing state")
	ErrDeviceOnline         = errors.New("device is currently online")
	ErrUninstallUnsupported = errors.New("device does not support remote uninstall")
)

type Service struct {
	store      *db.Store
	dir        string
	key        ed25519.PrivateKey
	ctx        context.Context
	cloudflare bool

	keyMu        sync.RWMutex
	nodeKey      string
	nodeKeyFixed bool

	connectionMu sync.Mutex
	connections  map[string]*peer

	pairMu       sync.Mutex
	pairWindow   time.Time
	pairTotal    int
	pairAttempts map[string]int

	settings     *settings.Service
	sshRunner    *sshrunner.Manager
	sshPending   sync.Map
	sshMu        sync.Mutex
	sshAttempts  map[string]*sshAttempt
	sshLocalMu   sync.Mutex
	sshLocalSess map[string]*TerminalSession
}

func New(ctx context.Context, store *db.Store, dataDir string, cloudflare bool) (*Service, error) {
	dir := filepath.Join(dataDir, "agents")
	for _, child := range []string{"", "logs", "windows-install", "linux-install"} {
		if err := os.MkdirAll(filepath.Join(dir, child), 0700); err != nil {
			return nil, err
		}
	}
	key, err := loadSigningKey(dir)
	if err != nil {
		return nil, err
	}
	nodeKey, nodeKeyFixed, err := loadNodeKey(dir)
	if err != nil {
		return nil, err
	}
	s := &Service{
		store: store, dir: dir, key: key, ctx: ctx, cloudflare: cloudflare,
		nodeKey: nodeKey, nodeKeyFixed: nodeKeyFixed, connections: make(map[string]*peer),
		sshAttempts:  make(map[string]*sshAttempt),
		sshLocalSess: make(map[string]*TerminalSession),
	}
	err = store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM device_sessions"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET conn_state='offline'
			WHERE id<>? AND is_demo=0 AND conn_state IN ('online','unstable')`, workbench.FreeChatDeviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials SET enabled=0, last_error='' WHERE enabled=1`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='queued' WHERE state='dispatched' AND
			assign_seq>(SELECT controller_ack FROM devices WHERE id=tasks.device_id)`); err != nil {
			return err
		}
		return nil
	})
	return s, err
}

func loadSigningKey(dir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dir, "ed25519.key")
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != ed25519.SeedSize {
			return nil, errors.New("invalid controller signing key")
		}
		return ed25519.NewKeyFromSeed(data), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.Write(key.Seed())
	return key, errors.Join(writeErr, file.Sync(), file.Close())
}

func audit(ctx context.Context, tx *sql.Tx, user int64, device any, action, detail string, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(user_id,device_id,action,detail_json,created_at)
		VALUES (?,?,?,?,?)`, user, device, action, detail, now)
	return err
}

func loadNodeKey(dir string) (string, bool, error) {
	if envKey := strings.TrimSpace(os.Getenv("GH_NODE_KEY")); envKey != "" {
		if len(envKey) < 12 || len(envKey) > 128 || strings.ContainsAny(envKey, " \t\r\n\x00") {
			return "", false, errors.New("GH_NODE_KEY must be 12-128 non-whitespace characters")
		}
		return envKey, true, nil
	}
	path := filepath.Join(dir, "node.key")
	data, err := os.ReadFile(path)
	if err == nil {
		key := strings.TrimSpace(string(data))
		if len(key) < 12 || len(key) > 128 {
			return "", false, errors.New("invalid node access key")
		}
		return key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	key, err := generateNodeKey()
	if err != nil {
		return "", false, err
	}
	if err := writeKeyFile(path, key); err != nil {
		return "", false, err
	}
	return key, false, nil
}

func generateNodeKey() (string, error) {
	token, err := proto.NewToken()
	if err != nil {
		return "", err
	}
	return "ghk_" + token, nil
}

func writeKeyFile(path, key string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".node-key-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temp)
		}
	}()
	if _, err := file.WriteString(key + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := errors.Join(file.Chmod(0600), file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (s *Service) NodeKey() string {
	s.keyMu.RLock()
	defer s.keyMu.RUnlock()
	return s.nodeKey
}

func (s *Service) NodeKeyManagedByEnv() bool {
	return s.nodeKeyFixed
}

func (s *Service) RotateNodeKey(ctx context.Context, userID int64) (string, error) {
	if s.nodeKeyFixed {
		return "", ErrConflict
	}
	key, err := generateNodeKey()
	if err != nil {
		return "", err
	}
	if err := writeKeyFile(filepath.Join(s.dir, "node.key"), key); err != nil {
		return "", err
	}
	s.keyMu.Lock()
	s.nodeKey = key
	s.keyMu.Unlock()
	_ = s.store.Write(ctx, func(tx *sql.Tx) error {
		return audit(ctx, tx, userID, nil, "node_key.rotate", "{}", time.Now().UnixMilli())
	})
	return key, nil
}

func (s *Service) isPairBlocked(ip string, now time.Time) bool {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if now.Sub(s.pairWindow) >= time.Minute {
		s.pairWindow, s.pairTotal = now, 0
		s.pairAttempts = make(map[string]int)
		return false
	}
	return s.pairTotal >= 60 || s.pairAttempts[ip] >= 10
}

func (s *Service) recordPairFailure(ip string, now time.Time) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if now.Sub(s.pairWindow) >= time.Minute {
		s.pairWindow, s.pairTotal = now, 0
		s.pairAttempts = make(map[string]int)
	}
	s.pairTotal++
	s.pairAttempts[ip]++
}

var machineIDPattern = regexp.MustCompile("^[A-Za-z0-9_-]{16,128}$")

func deriveDeviceID(platform proto.Platform, machineID string) string {
	sum := sha256.Sum256([]byte("ghosthand-device-v1\n" + string(platform) + "\n" + machineID))
	var b [16]byte
	copy(b[:], sum[:16])
	b[0] &= 0x7f
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	number := new(big.Int).SetBytes(b[:])
	divisor := big.NewInt(32)
	var remainder big.Int
	var encoded [26]byte
	for i := len(encoded) - 1; i >= 0; i-- {
		number.QuoRem(number, divisor, &remainder)
		encoded[i] = alphabet[remainder.Int64()]
	}
	return string(encoded[:])
}

func (s *Service) Enroll(ctx context.Context, input proto.Enrollment, ip string) (proto.EnrollmentResult, error) {
	now := time.Now()
	if s.isPairBlocked(ip, now) {
		return proto.EnrollmentResult{}, ErrLimited
	}
	input.Key = strings.TrimSpace(input.Key)
	input.MachineID = strings.TrimSpace(input.MachineID)
	input.Name = strings.TrimSpace(input.Name)
	input.Group = strings.TrimSpace(input.Group)
	hash, err := proto.TokenHash(input.Token)
	if err != nil || !machineIDPattern.MatchString(input.MachineID) || input.Name == "" ||
		utf8.RuneCountInString(input.Name) > 80 || utf8.RuneCountInString(input.Group) > 80 || len(input.OSVersion) > 128 ||
		(input.Platform != proto.Windows && input.Platform != proto.Linux && input.Platform != proto.Android) ||
		(input.Arch != "amd64" && input.Arch != "arm64") {
		return proto.EnrollmentResult{}, ErrInvalid
	}
	expectedKey := s.NodeKey()
	if input.Key == "" || subtle.ConstantTimeCompare([]byte(input.Key), []byte(expectedKey)) != 1 {
		s.recordPairFailure(ip, now)
		return proto.EnrollmentResult{}, ErrDenied
	}

	deviceID := deriveDeviceID(input.Platform, input.MachineID)
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		var existingID string
		var revoked bool
		err := tx.QueryRowContext(ctx, `SELECT d.id, COALESCE(c.revoked_at IS NOT NULL, 0)
			FROM devices d LEFT JOIN device_credentials c ON c.device_id=d.id
			WHERE d.id<>? AND (d.machine_id=? OR d.id=?) LIMIT 1`, workbench.FreeChatDeviceID, input.MachineID, deviceID).Scan(&existingID, &revoked)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if revoked {
				return ErrDenied
			}
			deviceID = existingID
			if _, err := tx.ExecContext(ctx, `UPDATE devices
				SET name=CASE WHEN name_customized=1 THEN name ELSE ? END, arch=?, os_version=?,
				    group_name=CASE WHEN ?<>'' THEN ? ELSE group_name END,
				    machine_id=CASE WHEN machine_id='' THEN ? ELSE machine_id END
				WHERE id=?`,
				input.Name, input.Arch, input.OSVersion, input.Group, input.Group, input.MachineID, deviceID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO device_credentials(device_id,token_hash,pending_hash,rotated_at)
				VALUES (?,?,NULL,?)
				ON CONFLICT(device_id) DO UPDATE SET token_hash=excluded.token_hash,pending_hash=NULL,rotated_at=excluded.rotated_at`,
				deviceID, hash, now.UnixMilli()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(device_id,action,created_at) VALUES (?,'device.rejoin',?)`,
				deviceID, now.UnixMilli()); err != nil {
				return err
			}
			return workbench.RecordChange(ctx, tx, deviceID)
		}

		helper, desktop := "unavailable", "no_session"
		if input.Platform == proto.Linux {
			helper, desktop = "none", "headless"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices
			(id,machine_id,name,platform,arch,os_version,group_name,conn_state,helper_state,desktop_state,last_seen_at)
			VALUES (?,?,?,?,?,?,?,'offline',?,?,?)`, deviceID, input.MachineID, input.Name, input.Platform, input.Arch, input.OSVersion,
			input.Group, helper, desktop, now.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO device_credentials(device_id,token_hash,rotated_at) VALUES (?,?,?)",
			deviceID, hash, now.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(device_id,action,created_at) VALUES (?,'device.enroll',?)`,
			deviceID, now.UnixMilli()); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	})
	public := s.key.Public().(ed25519.PublicKey)
	return proto.EnrollmentResult{DeviceID: deviceID, UpdateKey: base64.RawURLEncoding.EncodeToString(public)}, err
}

type credential struct {
	hash    string
	pending sql.NullString
	rotated int64
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func authenticate(ctx context.Context, q rowQuerier, id, token string) (credential, string, error) {
	var c credential
	hash, err := proto.TokenHash(token)
	if err != nil || len(id) != 26 {
		return c, "", ErrDenied
	}
	err = q.QueryRowContext(ctx, `SELECT token_hash,pending_hash,rotated_at FROM device_credentials
		WHERE device_id=? AND revoked_at IS NULL`, id).Scan(&c.hash, &c.pending, &c.rotated)
	if errors.Is(err, sql.ErrNoRows) {
		return c, "", ErrDenied
	}
	if err != nil {
		return c, "", err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(c.hash)) != 1 &&
		subtle.ConstantTimeCompare([]byte(hash), []byte(c.pending.String)) != 1 {
		return c, "", ErrDenied
	}
	return c, hash, nil
}

func (s *Service) rotate(ctx context.Context, tx *sql.Tx, id string, c credential, hash string, now time.Time) (string, error) {
	if c.pending.Valid && hash == c.pending.String {
		_, err := tx.ExecContext(ctx, "UPDATE device_credentials SET token_hash=?,pending_hash=NULL,rotated_at=? WHERE device_id=?",
			hash, now.UnixMilli(), id)
		return "", err
	}
	if !c.pending.Valid && now.UnixMilli()-c.rotated < int64((30*24*time.Hour)/time.Millisecond) {
		return "", nil
	}
	mac := hmac.New(sha256.New, s.key.Seed())
	fmt.Fprintf(mac, "ghosthand-token-v1\n%s\n%s\n%d", id, c.hash, c.rotated)
	token := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	pending, err := proto.TokenHash(token)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, "UPDATE device_credentials SET pending_hash=? WHERE device_id=?", pending, id)
	return token, err
}
