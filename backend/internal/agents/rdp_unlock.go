package agents

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"

	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

var (
	ErrRDPNoCredential = errors.New("未配置 Windows 解锁凭据。")
	ErrRDPSuspended    = errors.New("自动解锁已暂停，请更新密码或确认手动解锁。")
	ErrRDPTicket       = errors.New("解锁票据已过期或已使用。")
	ErrRDPTLS          = errors.New("请让 Windows 节点通过 HTTPS 重新接入，再执行解锁。")
)

type UnlockManager struct {
	fleet      *Service
	settings   *settings.Service
	mu         sync.Mutex
	operations map[string]*rdpOperation
	preflights map[string]proto.DesktopResult
	relock     map[string]*relockLease
}

type relockLease struct {
	session uint32
	tasks   map[string]struct{}
}

type rdpOperation struct {
	id, ticketHash string
	stage          string
	kind           proto.MessageType
	manual         bool
	tasks          map[string]struct{}
	expires        time.Time
	revision       int64
	generation     uint64
	result         proto.DesktopResult
	err            error
	done           chan struct{}
}

type RDPConfig struct {
	Username        string `json:"username"`
	Domain          string `json:"domain"`
	Password        string `json:"password"`
	BlankPassword   bool   `json:"blank_password"`
	Port            int    `json:"rdp_port"`
	AutoUnlock      *bool  `json:"auto_unlock"`
	AutoRelock      *bool  `json:"auto_relock"`
	KeepSystemAwake bool   `json:"keep_system_awake"`
	Clear           bool   `json:"clear"`
}

type RDPStatus struct {
	Configured      bool                 `json:"configured"`
	Username        string               `json:"username"`
	Domain          string               `json:"domain"`
	BlankPassword   bool                 `json:"blank_password"`
	Port            int                  `json:"rdp_port"`
	AutoUnlock      bool                 `json:"auto_unlock"`
	AutoRelock      bool                 `json:"auto_relock"`
	KeepSystemAwake bool                 `json:"keep_system_awake"`
	Suspended       bool                 `json:"suspended"`
	LastResult      string               `json:"last_result"`
	LastUnlockAt    int64                `json:"last_unlock_at"`
	Operation       proto.MessageType    `json:"operation,omitempty"`
	Stage           string               `json:"stage,omitempty"`
	Preflight       *proto.DesktopResult `json:"preflight,omitempty"`
	revision        int64
}

func NewUnlockManager(fleet *Service, settings *settings.Service) *UnlockManager {
	return &UnlockManager{fleet: fleet, settings: settings, operations: make(map[string]*rdpOperation),
		preflights: make(map[string]proto.DesktopResult), relock: make(map[string]*relockLease)}
}

func (m *UnlockManager) readRDPStatus(ctx context.Context, q rowQuerier, device string) (RDPStatus, error) {
	var status RDPStatus
	err := q.QueryRowContext(ctx, `SELECT r.device_id IS NOT NULL,COALESCE(r.username,''),COALESCE(r.domain,''),
		COALESCE(OCTET_LENGTH(r.password_sealed)=?,0),
		COALESCE(r.rdp_port,3389),COALESCE(r.auto_unlock,1),COALESCE(r.auto_relock,1),COALESCE(r.keep_system_awake,0),
		COALESCE(r.suspended,0),COALESCE(r.last_result,''),COALESCE(r.last_unlock_at,0),COALESCE(r.updated_at,0)
		FROM devices d LEFT JOIN device_rdp_credentials r ON r.device_id=d.id
		WHERE d.id=? AND d.is_demo=0 AND d.platform='windows'`, m.settings.SealedSize(0), device).
		Scan(&status.Configured, &status.Username, &status.Domain, &status.BlankPassword,
			&status.Port, &status.AutoUnlock, &status.AutoRelock, &status.KeepSystemAwake, &status.Suspended,
			&status.LastResult, &status.LastUnlockAt, &status.revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrMissing
	}
	return status, err
}

func (m *UnlockManager) Status(ctx context.Context, device string) (RDPStatus, error) {
	status, err := m.readRDPStatus(ctx, m.fleet.store, device)
	m.mu.Lock()
	defer m.mu.Unlock()
	if op := m.operations[device]; op != nil {
		status.Operation = op.kind
		status.Stage = op.stage
	}
	if value, ok := m.preflights[device]; ok {
		status.Preflight = &value
	}
	return status, err
}

func (m *UnlockManager) Save(ctx context.Context, user int64, device string, input RDPConfig) error {
	input.Username = strings.TrimSpace(input.Username)
	input.Domain = strings.TrimSpace(input.Domain)
	if input.Port == 0 {
		input.Port = 3389
	}
	var sealed []byte
	if !input.Clear {
		if input.Username == "" || len(input.Username) > 256 || len(input.Domain) > 256 || len(input.Password) > 480 ||
			input.Port < 1 || input.Port > 65535 || strings.ContainsFunc(input.Username, unicode.IsControl) ||
			strings.ContainsFunc(input.Domain, unicode.IsControl) || strings.ContainsFunc(input.Password, unicode.IsControl) {
			return ErrInvalid
		}
		if !input.BlankPassword && input.Password != "" {
			password := []byte(input.Password)
			var err error
			sealed, err = m.settings.Seal([]byte("device-rdp-v1:"+device), password)
			clear(password)
			if err != nil {
				return err
			}
		}
	}
	input.Password = ""
	err := m.fleet.store.Write(ctx, func(tx *sql.Tx) error {
		old, err := m.readRDPStatus(ctx, tx, device)
		if err != nil {
			return err
		}
		action := "rdp.config.save"
		now := time.Now().UnixMilli()
		if input.Clear {
			action = "rdp.config.clear"
			_, err = tx.ExecContext(ctx, "DELETE FROM device_rdp_credentials WHERE device_id=?", device)
		} else {
			if input.BlankPassword || (!old.Configured && sealed == nil) {
				sealed, err = m.settings.Seal([]byte("device-rdp-v1:"+device), nil)
				if err != nil {
					return err
				}
			}
			autoUnlock, autoRelock := old.AutoUnlock, old.AutoRelock
			if input.AutoUnlock != nil {
				autoUnlock = *input.AutoUnlock
			}
			if input.AutoRelock != nil {
				autoRelock = *input.AutoRelock
			}
			if sealed == nil {
				_, err = tx.ExecContext(ctx, `UPDATE device_rdp_credentials SET username=?, domain=?, rdp_port=?, auto_unlock=?, auto_relock=?,
					keep_system_awake=?,updated_at=MAX(updated_at+1,?) WHERE device_id=?`,
					input.Username, input.Domain, input.Port, autoUnlock, autoRelock, input.KeepSystemAwake, now, device)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO device_rdp_credentials
					(device_id,username,domain,password_sealed,rdp_port,auto_unlock,auto_relock,keep_system_awake,updated_at)
					VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(device_id) DO UPDATE SET username=excluded.username,domain=excluded.domain,
					password_sealed=excluded.password_sealed,rdp_port=excluded.rdp_port,auto_unlock=excluded.auto_unlock,
					auto_relock=excluded.auto_relock,keep_system_awake=excluded.keep_system_awake,auth_fail_count=0,suspended=0,
					updated_at=MAX(device_rdp_credentials.updated_at+1,excluded.updated_at)`,
					device, input.Username, input.Domain, sealed, input.Port, autoUnlock, autoRelock, input.KeepSystemAwake, now)
			}
		}
		if err != nil {
			return err
		}
		if err := audit(ctx, tx, user, device, action, "{}", now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, device)
	})
	if err == nil {
		m.mu.Lock()
		delete(m.preflights, device)
		if input.Clear || (input.AutoRelock != nil && !*input.AutoRelock) {
			delete(m.relock, device)
		}
		m.mu.Unlock()
		if p, err := m.fleet.desktopPeer(device); err == nil {
			select {
			case p.desktopPolicyChanged <- struct{}{}:
			default:
			}
		}
	}
	return err
}

func (m *UnlockManager) Redeem(ctx context.Context, device, deviceToken, operation, ticket string) (proto.RDPCredential, error) {
	var credential proto.RDPCredential
	if _, _, err := authenticate(ctx, m.fleet.store, device, deviceToken); err != nil {
		return credential, err
	}
	hash, err := proto.TokenHash(ticket)
	if err != nil {
		return credential, ErrRDPTicket
	}
	m.mu.Lock()
	op := m.operations[device]
	if op == nil || op.id != operation || !time.Now().Before(op.expires) ||
		subtle.ConstantTimeCompare([]byte(op.ticketHash), []byte(hash)) != 1 {
		for other, active := range m.operations {
			if other != device && active.id == operation && time.Now().Before(active.expires) &&
				subtle.ConstantTimeCompare([]byte(active.ticketHash), []byte(hash)) == 1 {
				m.mu.Unlock()
				return credential, ErrDenied
			}
		}
		m.mu.Unlock()
		return credential, ErrRDPTicket
	}
	op.ticketHash = ""
	revision, generation := op.revision, op.generation
	m.mu.Unlock()
	var sealed []byte
	err = m.fleet.store.QueryRowContext(ctx, `SELECT r.username,r.domain,r.password_sealed
		FROM device_rdp_credentials r JOIN devices d ON d.id=r.device_id
		JOIN device_credentials c ON c.device_id=d.id AND c.revoked_at IS NULL
		WHERE r.device_id=? AND r.updated_at=? AND d.conn_gen=?`, device, revision, generation).
		Scan(&credential.Username, &credential.Domain, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return credential, ErrRDPTicket
	}
	if err != nil {
		return credential, err
	}
	credential.Password, err = m.settings.Open([]byte("device-rdp-v1:"+device), sealed)
	return credential, err
}

func (m *UnlockManager) Operate(ctx context.Context, user int64, device string, kind proto.MessageType, source string, confirm bool) (proto.DesktopResult, error) {
	return m.operate(ctx, user, device, kind, source, confirm, 0)
}

func (m *UnlockManager) operate(ctx context.Context, user int64, device string, kind proto.MessageType, source string, confirm bool, session uint32) (proto.DesktopResult, error) {
	p, err := m.fleet.desktopPeer(device)
	if err != nil {
		if _, statusErr := m.readRDPStatus(ctx, m.fleet.store, device); statusErr != nil {
			return proto.DesktopResult{}, statusErr
		}
		return proto.DesktopResult{}, err
	}
	m.mu.Lock()
	status, err := m.readRDPStatus(ctx, m.fleet.store, device)
	if err != nil {
		m.mu.Unlock()
		return proto.DesktopResult{}, err
	}
	if kind == proto.RDPUnlock {
		if !p.secureTransport {
			m.mu.Unlock()
			return proto.DesktopResult{}, ErrRDPTLS
		}
		if !status.Configured {
			m.mu.Unlock()
			return proto.DesktopResult{}, ErrRDPNoCredential
		}
		if status.Suspended && !confirm {
			m.mu.Unlock()
			return proto.DesktopResult{}, ErrRDPSuspended
		}
		if source != "manual" && !status.AutoUnlock {
			m.mu.Unlock()
			return proto.DesktopResult{}, ErrConflict
		}
	}
	id, err := proto.NewID()
	if err != nil {
		m.mu.Unlock()
		return proto.DesktopResult{}, err
	}
	ttl := 45 * time.Second
	if kind == proto.RDPLock || kind == proto.RDPPreflight {
		ttl = 15 * time.Second
	}
	op := &rdpOperation{id: id, kind: kind, revision: status.revision, generation: p.gen,
		expires: time.Now().Add(ttl), done: make(chan struct{}), manual: source == "manual", tasks: make(map[string]struct{})}
	_, task, hasTask := strings.Cut(source, ":")
	if kind == proto.RDPUnlock && hasTask {
		op.tasks[task] = struct{}{}
	}
	payload := proto.DesktopRequest{OperationID: id, Port: status.Port, ExpiresAt: op.expires.UnixMilli(), SessionID: session}
	if kind == proto.RDPUnlock {
		op.stage = "preflight"
		payload.Ticket, err = proto.NewToken()
		if err != nil {
			m.mu.Unlock()
			return proto.DesktopResult{}, err
		}
		op.ticketHash, _ = proto.TokenHash(payload.Ticket)
	}
	if current := m.operations[device]; current != nil {
		if kind == proto.RDPUnlock && current.kind == kind {
			current.manual = current.manual || op.manual
			if hasTask {
				current.tasks[task] = struct{}{}
			}
		}
		m.mu.Unlock()
		if kind != proto.RDPUnlock || current.kind != kind {
			return proto.DesktopResult{}, ErrConflict
		}
		<-current.done
		return current.result, current.err
	}
	m.operations[device] = op
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if kind == proto.RDPUnlock && op.result.OK {
			var detail proto.RDPUnlockDetail
			switch {
			case op.manual:
				delete(m.relock, device)
			case json.Unmarshal(op.result.Detail, &detail) == nil && detail.SessionID != 0:
				lease := m.relock[device]
				if !detail.AlreadyUnlocked && (lease == nil || lease.session != detail.SessionID) {
					lease = &relockLease{session: detail.SessionID, tasks: make(map[string]struct{})}
					m.relock[device] = lease
				}
				if lease != nil && lease.session == detail.SessionID {
					for task := range op.tasks {
						lease.tasks[task] = struct{}{}
					}
				}
			}
		}
		delete(m.operations, device)
		close(op.done)
		m.mu.Unlock()
		m.notify(device)
	}()
	m.notify(device)
	// An HTTP disconnect must not discard a node's authentication failure.
	runCtx, cancel := context.WithDeadline(m.fleet.ctx, op.expires)
	defer cancel()
	op.result, op.err = requestDesktop(runCtx, p, kind, payload, func(stage string) bool {
		if kind != proto.RDPUnlock || (stage != "login" && stage != "transfer" && stage != "helper") {
			return false
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if op.stage == stage {
			return false
		}
		op.stage = stage
		return true
	})
	if op.err != nil {
		op.result = proto.DesktopResult{OperationID: id, Kind: kind, Code: proto.ErrRDPConnection}
		if errors.Is(op.err, context.DeadlineExceeded) {
			op.result.Code = proto.ErrRDPTimeout
		}
	}
	m.mu.Lock()
	if kind == proto.RDPPreflight && op.err == nil {
		m.preflights[device] = op.result
	}
	m.mu.Unlock()
	persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer stop()
	if err := m.recordRDPResult(persist, user, device, source, op); err != nil && op.err == nil {
		op.err = err
	}
	return op.result, op.err
}

func (m *UnlockManager) notify(device string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.fleet.ctx), 2*time.Second)
	defer cancel()
	_ = m.fleet.store.Write(ctx, func(tx *sql.Tx) error {
		return workbench.RecordChange(ctx, tx, device)
	})
}

func (m *UnlockManager) CanAutoUnlock(ctx context.Context, device string) bool {
	status, err := m.readRDPStatus(ctx, m.fleet.store, device)
	return err == nil && status.Configured && status.AutoUnlock && !status.Suspended
}

func (m *UnlockManager) AutoUnlock(ctx context.Context, user int64, device, task, source string) (proto.DesktopResult, error) {
	return m.Operate(ctx, user, device, proto.RDPUnlock, source+":"+task, false)
}

func (m *UnlockManager) FinishAutoUnlock(ctx context.Context, user int64, device, task string) {
	m.mu.Lock()
	lease := m.relock[device]
	if lease == nil {
		m.mu.Unlock()
		return
	}
	if _, ok := lease.tasks[task]; !ok {
		m.mu.Unlock()
		return
	}
	delete(lease.tasks, task)
	shouldLock := len(lease.tasks) == 0
	if shouldLock {
		delete(m.relock, device)
	}
	m.mu.Unlock()
	if !shouldLock {
		return
	}
	holdUntil := time.Now().Add(18 * time.Second).UnixMilli()
	var reserved bool
	if err := m.fleet.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		reservedResult, err := tx.ExecContext(ctx, `UPDATE device_rdp_credentials SET relock_until=?
			WHERE device_id=? AND auto_relock=1 AND NOT EXISTS (SELECT 1 FROM tasks WHERE device_id=?
			AND (state IN ('dispatched','running','paused','waiting_user','cancelling') OR (state='queued' AND assign_seq IS NOT NULL)))`,
			holdUntil, device, device)
		if err != nil {
			return err
		}
		n, err := reservedResult.RowsAffected()
		reserved = n == 1
		return err
	}); err != nil || !reserved {
		return
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = m.fleet.store.Write(cleanup, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(cleanup, "UPDATE device_rdp_credentials SET relock_until=0 WHERE device_id=? AND relock_until=?", device, holdUntil)
			return err
		})
	}()
	_, _ = m.operate(ctx, user, device, proto.RDPLock, "auto_relock:"+task, false, lease.session)
}

func (m *UnlockManager) recordRDPResult(ctx context.Context, user int64, device, source string, op *rdpOperation) error {
	code := string(op.result.Code)
	var detail proto.RDPUnlockDetail
	authenticated := op.result.OK && op.kind == proto.RDPUnlock &&
		json.Unmarshal(op.result.Detail, &detail) == nil && detail.SessionID != 0 && !detail.AlreadyUnlocked
	if op.result.OK {
		code = "ok"
	}
	return m.fleet.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		if op.kind == proto.RDPUnlock {
			var wasSuspended, suspended bool
			_ = tx.QueryRowContext(ctx, "SELECT suspended FROM device_rdp_credentials WHERE device_id=?", device).Scan(&wasSuspended)
			err := tx.QueryRowContext(ctx, `UPDATE device_rdp_credentials SET last_result=?,last_unlock_at=?,
				auth_fail_count=CASE WHEN ? THEN 0 WHEN ?='E_RDP_AUTH_FAILED' THEN auth_fail_count+1 ELSE auth_fail_count END,
				suspended=CASE WHEN ? THEN 0 WHEN ?='E_RDP_AUTH_FAILED' AND auth_fail_count>=1 THEN 1 ELSE suspended END
				WHERE device_id=? AND updated_at=? RETURNING suspended`,
				code, now, authenticated, code, authenticated, code, device, op.revision).Scan(&suspended)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if !wasSuspended && suspended {
				if err := audit(ctx, tx, user, device, "rdp.suspend", "{}", now); err != nil {
					return err
				}
			}
		}
		detail, _ := json.Marshal(map[string]string{"operation_id": op.id, "source": source, "result": code})
		if err := audit(ctx, tx, user, device, string(op.kind), string(detail), now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, device)
	})
}
