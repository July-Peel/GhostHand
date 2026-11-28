package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
	"ghosthand/proto/sshrunner"
)

type SSHCreateRequest struct {
	Name        string `json:"name"`
	Group       string `json:"group"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	AuthKind    string `json:"auth_kind"`
	Secret      string `json:"secret"`
	HostAgentID string `json:"host_agent_id"`
}

type SSHCreateResult struct {
	DeviceID  string          `json:"device_id"`
	Name      string          `json:"name"`
	ConnState proto.ConnState `json:"conn_state"`
	OSVersion string          `json:"os_version"`
	Arch      string          `json:"arch"`
	LastError string          `json:"last_error,omitempty"`
}

type SSHConfigStatus struct {
	Configured    bool   `json:"configured"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username"`
	AuthKind      string `json:"auth_kind"`
	HostAgentID   string `json:"host_agent_id"`
	HostAgentName string `json:"host_agent_name,omitempty"`
	HostKeyFP     string `json:"host_key_fp,omitempty"`
	Enabled       bool   `json:"enabled"`
	LastError     string `json:"last_error,omitempty"`
}

type SSHUpdateRequest struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	AuthKind    string `json:"auth_kind"`
	Secret      string `json:"secret"`
	HostAgentID string `json:"host_agent_id"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

type sshAttempt struct {
	id           string
	host         *peer
	result       chan error
	online       bool
	idleTimer    *time.Timer
	lastActivity time.Time
	activeCalls  int
}

const sshIdleTimeout = 10 * time.Minute

type sshCallWaiter struct {
	hostAgentID string
	ch          chan proto.SSHMCPResultPayload
}

func (s *Service) SetSettings(cfg *settings.Service) {
	s.settings = cfg
}

func (s *Service) StartSSH(ctx context.Context) {
	if s.settings == nil {
		return
	}
	s.sshRunner = sshrunner.NewManager(ctx, func(st proto.SSHStatePayload) {
		go s.applySSHState(nil, st)
	})
}

func (s *Service) CloseSSH() {
	s.sshMu.Lock()
	for _, attempt := range s.sshAttempts {
		if attempt.idleTimer != nil {
			attempt.idleTimer.Stop()
			attempt.idleTimer = nil
		}
	}
	s.sshMu.Unlock()
	if s.sshRunner != nil {
		s.sshRunner.Close()
	}
}

func (s *Service) handleAgentSSHState(host *peer, st proto.SSHStatePayload) {
	if len(st.SSHDeviceID) == 26 {
		s.applySSHState(host, st)
	}
}

func (s *Service) handleAgentSSHResult(hostAgentID string, payload proto.SSHMCPResultPayload) {
	val, ok := s.sshPending.Load(payload.CallID)
	if !ok {
		return
	}
	waiter := val.(sshCallWaiter)
	if waiter.hostAgentID != hostAgentID {
		return
	}
	select {
	case waiter.ch <- payload:
	default:
	}
}

func (s *Service) applySSHState(host *peer, st proto.SSHStatePayload) {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()

	deviceID := st.SSHDeviceID
	attempt := s.sshAttempts[deviceID]
	if attempt == nil || attempt.host != host {
		if host != nil {
			// A host Agent may still hold a login from an earlier controller session.
			s.sendSSHDisconnect(host, deviceID)
		}
		return
	}
	// Older host Agents omit the attempt ID.
	if st.AttemptID != "" && st.AttemptID != attempt.id {
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	switch {
	case st.State == proto.Offline && st.Error != "":
		s.failSSHAttempt(ctx, deviceID, attempt, errors.New(st.Error))
	case st.State == proto.Online && !attempt.online:
		now := time.Now().UnixMilli()
		err := s.store.Write(ctx, func(tx *sql.Tx) error {
			var gen uint64
			osVer := strings.TrimSpace(st.OSVersion)
			if osVer == "" {
				osVer = "Linux (SSH)"
			}
			arch := strings.TrimSpace(st.Arch)
			if arch == "" {
				arch = "amd64"
			}
			if err := tx.QueryRowContext(ctx, `UPDATE devices
				SET conn_state='online', conn_gen=conn_gen+1, os_version=?, arch=?, last_seen_at=?
				WHERE id=? RETURNING conn_gen`, osVer, arch, now, deviceID).Scan(&gen); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO device_sessions(device_id, conn_gen, connected_at, remote_ip, agent_ver)
				VALUES (?,?,?,'ssh','ssh')`, deviceID, gen, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials
				SET host_key_fp=CASE WHEN ?<>'' THEN ? ELSE host_key_fp END WHERE device_id=?`,
				st.HostKeyFP, st.HostKeyFP, deviceID); err != nil {
				return err
			}
			return workbench.RecordChange(ctx, tx, deviceID)
		})
		if err != nil {
			s.failSSHAttempt(ctx, deviceID, attempt, err)
			return
		}
		attempt.online = true
		attempt.lastActivity = time.Now()
		attempt.idleTimer = time.AfterFunc(sshIdleTimeout, func() { s.expireSSHIdle(deviceID, attempt) })
		attempt.result <- nil
	}
}

func (s *Service) expireSSHIdle(deviceID string, attempt *sshAttempt) {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	if s.ctx.Err() != nil || s.sshAttempts[deviceID] != attempt || attempt.idleTimer == nil {
		return
	}
	if attempt.activeCalls != 0 {
		attempt.idleTimer.Reset(sshIdleTimeout)
		return
	}
	if remaining := time.Until(attempt.lastActivity.Add(sshIdleTimeout)); remaining > 0 {
		attempt.idleTimer.Reset(remaining)
		return
	}

	const reason = "SSH 闲置 10 分钟，已自动断开"
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := s.store.Write(ctx, func(tx *sql.Tx) error {
		if err := setSSHOffline(ctx, tx, deviceID, reason); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	}); err != nil {
		if s.ctx.Err() == nil {
			slog.Error("expire idle ssh session", "device", deviceID, "error", err)
			attempt.idleTimer.Reset(5 * time.Second)
		}
		return
	}
	s.stopSSHAttempt(deviceID, attempt, errors.New(reason))
}

func setSSHOffline(ctx context.Context, tx *sql.Tx, deviceID, lastError string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials SET enabled=0, last_error=?, updated_at=? WHERE device_id=?`,
		lastError, time.Now().UnixMilli(), deviceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_sessions WHERE device_id=?`, deviceID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE devices SET conn_state='offline' WHERE id=?`, deviceID)
	return err
}

func (s *Service) failSSHAttempt(ctx context.Context, deviceID string, attempt *sshAttempt, cause error) {
	if err := s.store.Write(ctx, func(tx *sql.Tx) error {
		if err := setSSHOffline(ctx, tx, deviceID, truncateString(cause.Error(), 400)); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	}); err != nil && s.ctx.Err() == nil {
		slog.Error("mark ssh device offline", "device", deviceID, "error", err)
	}
	s.stopSSHAttempt(deviceID, attempt, cause)
}

func (s *Service) stopSSHAttempt(deviceID string, attempt *sshAttempt, cause error) {
	delete(s.sshAttempts, deviceID)
	if attempt.idleTimer != nil {
		attempt.idleTimer.Stop()
	}
	if attempt.host != nil {
		s.sendSSHDisconnect(attempt.host, deviceID)
		attempt.host.terminalMu.Lock()
		for id, terminal := range attempt.host.terminalSessions {
			if terminal.sshAttempt == attempt {
				attempt.host.removeTerminal(id)
			}
		}
		attempt.host.terminalMu.Unlock()
	} else if s.sshRunner != nil {
		s.sshRunner.Disconnect(deviceID)
	}
	s.closeLocalSSHTerminals(deviceID)
	if !attempt.online {
		attempt.result <- cause
	}
}

func (s *Service) sendSSHDisconnect(host *peer, deviceID string) {
	frame, err := proto.NewEnvelope(proto.SSHDisconnectMessage, proto.SSHDisconnectPayload{SSHDeviceID: deviceID})
	if err != nil {
		return
	}
	select {
	case <-host.sshDone:
		return
	default:
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case host.sshFrames <- frame:
	case <-host.sshDone:
	case <-timer.C:
		// Closing the Agent connection invalidates any sessions it still owns.
		_ = host.conn.CloseNow()
	}
}

func (s *Service) detachHostAgentSSH(host *peer) {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	for deviceID, attempt := range s.sshAttempts {
		if attempt.host == host {
			s.failSSHAttempt(ctx, deviceID, attempt, errors.New("宿主 Agent 已离线"))
		}
	}
}

func validateSSHParams(host string, port int, username, authKind, secret string, requireSecret bool) (string, int, string, string, error) {
	host = strings.TrimSpace(host)
	username = strings.TrimSpace(username)
	authKind = strings.TrimSpace(authKind)
	if authKind == "" {
		authKind = proto.SSHAuthPassword
	}
	if port == 0 {
		port = 22
	}
	if host == "" || len(host) > 255 || strings.ContainsAny(host, " \t\r\n\x00") || strings.ContainsFunc(host, unicode.IsControl) {
		return "", 0, "", "", ErrInvalid
	}
	if port < 1 || port > 65535 {
		return "", 0, "", "", ErrInvalid
	}
	if username == "" || len(username) > 128 || strings.ContainsFunc(username, unicode.IsControl) {
		return "", 0, "", "", ErrInvalid
	}
	if authKind != proto.SSHAuthPassword && authKind != proto.SSHAuthPrivateKey {
		return "", 0, "", "", ErrInvalid
	}
	if requireSecret && secret == "" {
		return "", 0, "", "", ErrInvalid
	}
	if len(secret) > 32768 {
		return "", 0, "", "", ErrInvalid
	}
	return host, port, username, authKind, nil
}

func (s *Service) CreateSSHDevice(ctx context.Context, userID int64, input SSHCreateRequest) (SSHCreateResult, error) {
	if s.settings == nil {
		return SSHCreateResult{}, errors.New("settings service is not configured")
	}
	name := strings.TrimSpace(input.Name)
	group := strings.TrimSpace(input.Group)
	if name == "" || utf8.RuneCountInString(name) > 80 || utf8.RuneCountInString(group) > 80 ||
		strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) }) ||
		strings.ContainsFunc(group, unicode.IsControl) {
		return SSHCreateResult{}, ErrInvalid
	}
	host, port, username, authKind, err := validateSSHParams(input.Host, input.Port, input.Username, input.AuthKind, input.Secret, true)
	if err != nil {
		return SSHCreateResult{}, err
	}
	hostAgentID := strings.TrimSpace(input.HostAgentID)

	deviceID, err := proto.NewID()
	if err != nil {
		return SSHCreateResult{}, err
	}
	sealed, err := s.settings.Seal([]byte("device-ssh-v1:"+deviceID), []byte(input.Secret))
	if err != nil {
		return SSHCreateResult{}, err
	}
	capsJSON, _ := json.Marshal(proto.TaskCapabilities(proto.Linux))
	toolsJSON, _ := json.Marshal(proto.LinuxTools)
	resJSON, _ := json.Marshal(proto.LinuxResources)
	now := time.Now().UnixMilli()

	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		var hostAgentVal any
		if hostAgentID != "" {
			var validHost bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM devices d JOIN device_credentials c ON c.device_id=d.id
				WHERE d.id=? AND d.id<>? AND d.is_demo=0 AND c.revoked_at IS NULL
				AND NOT EXISTS(SELECT 1 FROM device_ssh_credentials s WHERE s.device_id=d.id)
			)`, hostAgentID, workbench.FreeChatDeviceID).Scan(&validHost); err != nil {
				return err
			}
			if !validHost {
				return ErrInvalid
			}
			hostAgentVal = hostAgentID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices
			(id, machine_id, name, platform, arch, os_version, group_name, conn_state, helper_state, desktop_state,
			 last_seen_at, agent_ver, caps_hash, caps_json, task_protocol, mcp_tools, mcp_resources, name_customized)
			VALUES (?,?,?,'linux','amd64','Linux (SSH)',?,'offline','none','headless',?,'ssh',?,?,2,?,?,1)`,
			deviceID, "ssh:"+deviceID, name, group, now, proto.TaskCapabilitiesHash(proto.Linux),
			string(capsJSON), string(toolsJSON), string(resJSON)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_credentials(device_id, token_hash, rotated_at)
			VALUES (?,?,?)`, deviceID, "ssh-managed:"+deviceID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_ssh_credentials
			(device_id, host, port, username, auth_kind, secret_ciphertext, host_key_fp, host_agent_id, enabled, last_error, updated_at)
			VALUES (?,?,?,?,?,?,'',?,0,'',?)`,
			deviceID, host, port, username, authKind, sealed, hostAgentVal, now); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]any{"host": host, "port": port, "username": username, "host_agent_id": hostAgentID})
		if err := audit(ctx, tx, userID, deviceID, "ssh.create", string(detail), now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	})
	if err != nil {
		return SSHCreateResult{}, err
	}

	return SSHCreateResult{
		DeviceID:  deviceID,
		Name:      name,
		ConnState: proto.Offline,
		OSVersion: "Linux (SSH)",
		Arch:      "amd64",
	}, nil
}

func (s *Service) SSHStatus(ctx context.Context, deviceID string) (SSHConfigStatus, error) {
	var st SSHConfigStatus
	var hostAgentID, hostAgentName sql.NullString
	err := s.store.QueryRowContext(ctx, `SELECT s.host, s.port, s.username, s.auth_kind,
		s.host_agent_id, h.name, s.host_key_fp, s.enabled=1, s.last_error
		FROM device_ssh_credentials s LEFT JOIN devices h ON h.id=s.host_agent_id
		WHERE s.device_id=?`, deviceID).
		Scan(&st.Host, &st.Port, &st.Username, &st.AuthKind, &hostAgentID, &hostAgentName, &st.HostKeyFP, &st.Enabled, &st.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrMissing
	}
	if err != nil {
		return st, err
	}
	st.Configured = true
	st.HostAgentID = hostAgentID.String
	st.HostAgentName = hostAgentName.String
	return st, nil
}

func (s *Service) UpdateSSHConfig(ctx context.Context, userID int64, deviceID string, input SSHUpdateRequest) (SSHConfigStatus, error) {
	if s.settings == nil {
		return SSHConfigStatus{}, errors.New("settings service is not configured")
	}
	host, port, username, authKind, err := validateSSHParams(input.Host, input.Port, input.Username, input.AuthKind, input.Secret, false)
	if err != nil {
		return SSHConfigStatus{}, err
	}
	newHostAgent := strings.TrimSpace(input.HostAgentID)
	if newHostAgent == deviceID {
		return SSHConfigStatus{}, ErrInvalid
	}

	loginCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	s.sshMu.Lock()
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		var oldHost, fp string
		var oldPort int
		var sealed []byte
		err := tx.QueryRowContext(ctx, `SELECT host, port, secret_ciphertext, host_key_fp
			FROM device_ssh_credentials WHERE device_id=?`, deviceID).Scan(&oldHost, &oldPort, &sealed, &fp)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}

		var hostAgentVal any
		if newHostAgent != "" {
			var validHost bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM devices d JOIN device_credentials c ON c.device_id=d.id
				WHERE d.id=? AND d.id<>? AND d.is_demo=0 AND c.revoked_at IS NULL
				AND NOT EXISTS(SELECT 1 FROM device_ssh_credentials s WHERE s.device_id=d.id)
			)`, newHostAgent, workbench.FreeChatDeviceID).Scan(&validHost); err != nil {
				return err
			}
			if !validHost {
				return ErrInvalid
			}
			hostAgentVal = newHostAgent
		}

		if input.Secret != "" {
			sealed, err = s.settings.Seal([]byte("device-ssh-v1:"+deviceID), []byte(input.Secret))
			if err != nil {
				return err
			}
		}
		if host != oldHost || port != oldPort {
			fp = ""
		}
		if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials
			SET host=?, port=?, username=?, auth_kind=?, secret_ciphertext=?, host_key_fp=?, host_agent_id=?
			WHERE device_id=?`, host, port, username, authKind, sealed, fp, hostAgentVal, deviceID); err != nil {
			return err
		}
		if err := setSSHOffline(ctx, tx, deviceID, ""); err != nil {
			return err
		}
		if err := audit(ctx, tx, userID, deviceID, "ssh.update", "{}", time.Now().UnixMilli()); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	})
	if err != nil {
		s.sshMu.Unlock()
		return SSHConfigStatus{}, err
	}
	if attempt := s.sshAttempts[deviceID]; attempt != nil {
		s.stopSSHAttempt(deviceID, attempt, errors.New("SSH 配置已更新"))
	}
	var attempt *sshAttempt
	if input.Enabled != nil && *input.Enabled {
		// Retain support for API clients that explicitly request a connection on save.
		attempt, err = s.startSSHAttempt(loginCtx, userID, deviceID)
	}
	s.sshMu.Unlock()
	if attempt != nil {
		err = s.waitSSHAttempt(loginCtx, deviceID, attempt)
	}
	status, statusErr := s.SSHStatus(ctx, deviceID)
	if err != nil && statusErr == nil && status.LastError == "" {
		status.LastError = err.Error()
	}
	return status, statusErr
}

// startSSHAttempt must be called with sshMu held.
func (s *Service) startSSHAttempt(ctx context.Context, userID int64, deviceID string) (*sshAttempt, error) {
	if attempt := s.sshAttempts[deviceID]; attempt != nil && !attempt.online {
		return nil, errors.New("SSH 正在连接")
	}
	if s.settings == nil {
		return nil, errors.New("settings service is not configured")
	}

	var payload proto.SSHConnectPayload
	var sealed []byte
	var hostAgentID sql.NullString
	err := s.store.QueryRowContext(ctx, `SELECT s.host, s.port, s.username, s.auth_kind, s.secret_ciphertext, s.host_key_fp, s.host_agent_id
		FROM device_ssh_credentials s JOIN device_credentials c USING(device_id)
		WHERE s.device_id=? AND c.revoked_at IS NULL`, deviceID).
		Scan(&payload.Host, &payload.Port, &payload.Username, &payload.AuthKind, &sealed, &payload.HostKeyFP, &hostAgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.settings.Open([]byte("device-ssh-v1:"+deviceID), sealed)
	if err != nil {
		return nil, err
	}
	payload.AttemptID, err = proto.NewID()
	if err != nil {
		return nil, err
	}
	payload.SSHDeviceID = deviceID
	payload.Secret = string(plain)

	attempt := &sshAttempt{id: payload.AttemptID, result: make(chan error, 1)}
	var frame proto.Envelope
	if hostAgentID.String == "" {
		if s.sshRunner == nil {
			return nil, errors.New("服务端 SSH 执行引擎尚未初始化")
		}
	} else {
		s.connectionMu.Lock()
		attempt.host = s.connections[hostAgentID.String]
		s.connectionMu.Unlock()
		if attempt.host == nil {
			return nil, errors.New("选定的宿主 Agent 当前离线")
		}
		frame, err = proto.NewEnvelope(proto.SSHConnectMessage, payload)
		if err != nil {
			return nil, err
		}
	}

	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		if err := setSSHOffline(ctx, tx, deviceID, ""); err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials SET enabled=1 WHERE device_id=?`, deviceID); err != nil {
			return err
		}
		if err := audit(ctx, tx, userID, deviceID, "ssh.reconnect", "{}", now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	})
	if err != nil {
		return nil, err
	}
	if old := s.sshAttempts[deviceID]; old != nil {
		s.stopSSHAttempt(deviceID, old, errors.New("SSH 正在重新连接"))
	}
	s.sshAttempts[deviceID] = attempt
	if attempt.host == nil {
		s.sshRunner.Connect(payload)
		return attempt, nil
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case attempt.host.sshFrames <- frame:
		return attempt, nil
	case <-attempt.host.sshDone:
		err = errors.New("选定的宿主 Agent 连接已断开")
	case <-sendCtx.Done():
		err = errors.New("无法向宿主 Agent 发起 SSH 连接")
	}
	writeCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	s.failSSHAttempt(writeCtx, deviceID, attempt, err)
	return nil, err
}

// ConnectSSH logs in with the saved credentials or reuses an existing login.
func (s *Service) ConnectSSH(ctx context.Context, userID int64, deviceID string) error {
	return s.connectSSH(ctx, userID, deviceID, false)
}

func (s *Service) ReconnectSSH(ctx context.Context, userID int64, deviceID string) error {
	return s.connectSSH(ctx, userID, deviceID, true)
}

func (s *Service) connectSSH(ctx context.Context, userID int64, deviceID string, reconnect bool) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	s.sshMu.Lock()
	if current := s.sshAttempts[deviceID]; !reconnect && current != nil && current.online {
		current.lastActivity = time.Now()
		s.sshMu.Unlock()
		return nil
	}
	attempt, err := s.startSSHAttempt(ctx, userID, deviceID)
	s.sshMu.Unlock()
	if err != nil {
		return err
	}
	return s.waitSSHAttempt(ctx, deviceID, attempt)
}

func (s *Service) waitSSHAttempt(ctx context.Context, deviceID string, attempt *sshAttempt) error {
	select {
	case err := <-attempt.result:
		return err
	case <-ctx.Done():
		s.sshMu.Lock()
		defer s.sshMu.Unlock()
		if s.sshAttempts[deviceID] != attempt || attempt.online {
			return <-attempt.result
		}
		err := errors.New("等待 SSH 登录超时")
		if errors.Is(ctx.Err(), context.Canceled) {
			err = errors.New("SSH 登录已取消")
		}
		writeCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		defer cancel()
		s.failSSHAttempt(writeCtx, deviceID, attempt, err)
		return err
	}
}

func (s *Service) DisconnectSSH(ctx context.Context, userID int64, deviceID string) error {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM device_ssh_credentials WHERE device_id=?)`, deviceID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrMissing
		}
		if err := setSSHOffline(ctx, tx, deviceID, ""); err != nil {
			return err
		}
		if err := audit(ctx, tx, userID, deviceID, "ssh.disconnect", "{}", time.Now().UnixMilli()); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	})
	if err == nil {
		if attempt := s.sshAttempts[deviceID]; attempt != nil {
			s.stopSSHAttempt(deviceID, attempt, errors.New("SSH 设备已断开连接"))
		}
	}
	return err
}

func (s *Service) closeLocalSSHTerminals(deviceID string) {
	s.sshLocalMu.Lock()
	var targets []*TerminalSession
	for _, sess := range s.sshLocalSess {
		if sess.targetDeviceID == deviceID {
			targets = append(targets, sess)
		}
	}
	s.sshLocalMu.Unlock()
	for _, sess := range targets {
		sess.Close()
	}
}

// IsSSHDevice implements workbench.SSHExecutor.
func (s *Service) IsSSHDevice(ctx context.Context, deviceID string) bool {
	var exists bool
	_ = s.store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM device_ssh_credentials WHERE device_id=?)`, deviceID).Scan(&exists)
	return exists
}

// ExecuteSSHTool implements workbench.SSHExecutor.
// Routes the tool call to the selected host Agent (ssh.mcp_call -> ssh.mcp_result)
// or executes it directly on the server's local sshrunner when host_agent_id is empty.
func (s *Service) ExecuteSSHTool(ctx context.Context, deviceID string, call proto.MCPCall) (proto.MCPResult, error) {
	s.sshMu.Lock()
	attempt := s.sshAttempts[deviceID]
	if attempt == nil || !attempt.online {
		s.sshMu.Unlock()
		return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "SSH 设备已断开连接"}, nil
	}
	attempt.activeCalls++
	s.sshMu.Unlock()
	defer func() {
		s.sshMu.Lock()
		attempt.activeCalls--
		attempt.lastActivity = time.Now()
		s.sshMu.Unlock()
	}()

	p := attempt.host
	if p == nil {
		if s.sshRunner == nil {
			return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "服务端 SSH 执行引擎未启动"}, nil
		}
		return s.sshRunner.ExecuteTool(ctx, deviceID, call), nil
	}

	waitCh := make(chan proto.SSHMCPResultPayload, 1)
	s.sshPending.Store(call.CallID, sshCallWaiter{hostAgentID: p.id, ch: waitCh})
	defer s.sshPending.Delete(call.CallID)

	frame, err := proto.NewEnvelope(proto.SSHCallMessage, proto.SSHMCPCallPayload{
		CallID:         call.CallID,
		TargetDeviceID: deviceID,
		TaskID:         call.TaskID,
		Tool:           call.Tool,
		Arguments:      call.Arguments,
		Approved:       call.Approved,
	})
	if err != nil {
		return proto.MCPResult{}, err
	}

	select {
	case p.sshFrames <- frame:
	case <-p.sshDone:
		return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "宿主 Agent 连接已断开"}, nil
	case <-ctx.Done():
		return proto.MCPResult{}, ctx.Err()
	}

	select {
	case res := <-waitCh:
		return proto.MCPResult{
			CallID: call.CallID,
			TaskID: call.TaskID,
			Data:   res.Data,
			Error:  res.Error,
		}, nil
	case <-p.sshDone:
		return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "宿主 Agent 连接已断开"}, nil
	case <-ctx.Done():
		return proto.MCPResult{}, ctx.Err()
	}
}

func truncateString(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
