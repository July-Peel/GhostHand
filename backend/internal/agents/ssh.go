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
		go s.handleLocalSSHState(st)
	})
	rows, err := s.store.QueryContext(ctx, `SELECT device_id, host, port, username, auth_kind, secret_ciphertext, host_key_fp
		FROM device_ssh_credentials WHERE enabled=1 AND (host_agent_id IS NULL OR host_agent_id='')`)
	if err != nil {
		slog.Error("load direct ssh targets", "error", err)
		return
	}
	var configs []proto.SSHConnectPayload
	for rows.Next() {
		var cfg proto.SSHConnectPayload
		var sealed []byte
		if err := rows.Scan(&cfg.SSHDeviceID, &cfg.Host, &cfg.Port, &cfg.Username, &cfg.AuthKind, &sealed, &cfg.HostKeyFP); err != nil {
			continue
		}
		plain, err := s.settings.Open([]byte("device-ssh-v1:"+cfg.SSHDeviceID), sealed)
		if err != nil {
			slog.Warn("decrypt ssh secret", "device", cfg.SSHDeviceID, "error", err)
			continue
		}
		cfg.Secret = string(plain)
		configs = append(configs, cfg)
	}
	rows.Close()
	for _, cfg := range configs {
		s.sshRunner.Connect(cfg)
	}
}

func (s *Service) CloseSSH() {
	if s.sshRunner != nil {
		s.sshRunner.Close()
	}
}

func (s *Service) handleLocalSSHState(st proto.SSHStatePayload) {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	s.applySSHState(ctx, st)
}

func (s *Service) handleAgentSSHState(hostAgentID string, st proto.SSHStatePayload) {
	if len(st.SSHDeviceID) != 26 {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	var assigned sql.NullString
	err := s.store.QueryRowContext(ctx, `SELECT host_agent_id FROM device_ssh_credentials WHERE device_id=?`, st.SSHDeviceID).Scan(&assigned)
	if err != nil || !assigned.Valid || assigned.String != hostAgentID {
		return
	}
	s.applySSHState(ctx, st)
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

func (s *Service) applySSHState(ctx context.Context, st proto.SSHStatePayload) {
	now := time.Now().UnixMilli()
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM device_ssh_credentials WHERE device_id=?)`, st.SSHDeviceID).Scan(&exists); err != nil || !exists {
			return err
		}
		if st.State == proto.Online {
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
				WHERE id=? RETURNING conn_gen`, osVer, arch, now, st.SSHDeviceID).Scan(&gen); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM device_sessions WHERE device_id=?`, st.SSHDeviceID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO device_sessions(device_id, conn_gen, connected_at, remote_ip, agent_ver)
				VALUES (?,?,?,'ssh','ssh')`, st.SSHDeviceID, gen, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials
				SET last_error='', host_key_fp=CASE WHEN ?<>'' THEN ? ELSE host_key_fp END
				WHERE device_id=?`, st.HostKeyFP, st.HostKeyFP, st.SSHDeviceID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `DELETE FROM device_sessions WHERE device_id=?`, st.SSHDeviceID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE devices SET conn_state='offline' WHERE id=?`, st.SSHDeviceID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials SET last_error=? WHERE device_id=?`,
				truncateString(st.Error, 400), st.SSHDeviceID); err != nil {
				return err
			}
		}
		return workbench.RecordChange(ctx, tx, st.SSHDeviceID)
	})
	if err != nil && s.ctx.Err() == nil {
		slog.Error("apply ssh state", "device", st.SSHDeviceID, "error", err)
	}
	s.notifySSHWaiters(st)
}

func (s *Service) registerSSHWaiter(deviceID string) (chan proto.SSHStatePayload, func()) {
	ch := make(chan proto.SSHStatePayload, 1)
	s.sshWaitMu.Lock()
	s.sshWaiters[deviceID] = append(s.sshWaiters[deviceID], ch)
	s.sshWaitMu.Unlock()
	cleanup := func() {
		s.sshWaitMu.Lock()
		list := s.sshWaiters[deviceID]
		for i, item := range list {
			if item == ch {
				s.sshWaiters[deviceID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(s.sshWaiters[deviceID]) == 0 {
			delete(s.sshWaiters, deviceID)
		}
		s.sshWaitMu.Unlock()
	}
	return ch, cleanup
}

func (s *Service) notifySSHWaiters(st proto.SSHStatePayload) {
	s.sshWaitMu.Lock()
	list := append([]chan proto.SSHStatePayload(nil), s.sshWaiters[st.SSHDeviceID]...)
	s.sshWaitMu.Unlock()
	for _, ch := range list {
		select {
		case ch <- st:
		default:
		}
	}
}

func (s *Service) syncHostAgentSSH(ctx context.Context, p *peer) {
	if s.settings == nil {
		return
	}
	rows, err := s.store.QueryContext(ctx, `SELECT device_id, host, port, username, auth_kind, secret_ciphertext, host_key_fp
		FROM device_ssh_credentials WHERE host_agent_id=? AND enabled=1`, p.id)
	if err != nil {
		slog.Error("load delegated ssh targets", "host_agent", p.id, "error", err)
		return
	}
	var configs []proto.SSHConnectPayload
	for rows.Next() {
		var cfg proto.SSHConnectPayload
		var sealed []byte
		if err := rows.Scan(&cfg.SSHDeviceID, &cfg.Host, &cfg.Port, &cfg.Username, &cfg.AuthKind, &sealed, &cfg.HostKeyFP); err != nil {
			continue
		}
		plain, err := s.settings.Open([]byte("device-ssh-v1:"+cfg.SSHDeviceID), sealed)
		if err != nil {
			slog.Warn("decrypt delegated ssh secret", "device", cfg.SSHDeviceID, "error", err)
			continue
		}
		cfg.Secret = string(plain)
		configs = append(configs, cfg)
	}
	rows.Close()
	for _, cfg := range configs {
		frame, err := proto.NewEnvelope(proto.SSHConnectMessage, cfg)
		if err != nil {
			continue
		}
		select {
		case p.sshFrames <- frame:
		case <-p.sshDone:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Service) detachHostAgentSSH(hostAgentID string) {
	// Fail any in-flight delegated SSH tool calls immediately.
	s.sshPending.Range(func(key, value any) bool {
		waiter := value.(sshCallWaiter)
		if waiter.hostAgentID == hostAgentID {
			select {
			case waiter.ch <- proto.SSHMCPResultPayload{Error: "宿主 Agent 连接已断开"}:
			default:
			}
		}
		return true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.store.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT device_id FROM device_ssh_credentials WHERE host_agent_id=?`, hostAgentID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM device_sessions WHERE device_id=?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE devices SET conn_state='offline' WHERE id=?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials SET last_error='宿主 Agent 已离线' WHERE device_id=?`, id); err != nil {
				return err
			}
			if err := workbench.RecordChange(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
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
			VALUES (?,?,?,?,?,?,'',?,1,'',?)`,
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

	waitCh, cleanup := s.registerSSHWaiter(deviceID)
	defer cleanup()

	payload := proto.SSHConnectPayload{
		SSHDeviceID: deviceID,
		Host:        host,
		Port:        port,
		Username:    username,
		AuthKind:    authKind,
		Secret:      input.Secret,
	}
	if err := s.dispatchSSHConnect(ctx, hostAgentID, payload); err != nil {
		s.applySSHState(ctx, proto.SSHStatePayload{
			SSHDeviceID: deviceID,
			State:       proto.Offline,
			Error:       err.Error(),
		})
		return SSHCreateResult{
			DeviceID:  deviceID,
			Name:      name,
			ConnState: proto.Offline,
			OSVersion: "Linux (SSH)",
			Arch:      "amd64",
			LastError: err.Error(),
		}, nil
	}

	result := SSHCreateResult{
		DeviceID:  deviceID,
		Name:      name,
		ConnState: proto.Offline,
		OSVersion: "Linux (SSH)",
		Arch:      "amd64",
	}
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case st := <-waitCh:
		result.ConnState = st.State
		if st.OSVersion != "" {
			result.OSVersion = st.OSVersion
		}
		if st.Arch != "" {
			result.Arch = st.Arch
		}
		result.LastError = st.Error
	case <-timer.C:
	case <-ctx.Done():
	}
	return result, nil
}

func (s *Service) dispatchSSHConnect(ctx context.Context, hostAgentID string, payload proto.SSHConnectPayload) error {
	if hostAgentID == "" {
		if s.sshRunner == nil {
			return errors.New("服务端 SSH 执行引擎尚未初始化")
		}
		s.sshRunner.Connect(payload)
		return nil
	}
	s.connectionMu.Lock()
	p := s.connections[hostAgentID]
	s.connectionMu.Unlock()
	if p == nil {
		return errors.New("选定的宿主 Agent 当前离线，待其上线后将自动发起 SSH 登录")
	}
	frame, err := proto.NewEnvelope(proto.SSHConnectMessage, payload)
	if err != nil {
		return err
	}
	select {
	case p.sshFrames <- frame:
		return nil
	case <-p.sshDone:
		return errors.New("选定的宿主 Agent 连接已断开")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) dispatchSSHDisconnect(hostAgentID, sshDeviceID string) {
	if hostAgentID == "" {
		if s.sshRunner != nil {
			s.sshRunner.Disconnect(sshDeviceID)
		}
		return
	}
	s.connectionMu.Lock()
	p := s.connections[hostAgentID]
	s.connectionMu.Unlock()
	if p == nil {
		return
	}
	frame, err := proto.NewEnvelope(proto.SSHDisconnectMessage, proto.SSHDisconnectPayload{SSHDeviceID: sshDeviceID})
	if err != nil {
		return
	}
	select {
	case p.sshFrames <- frame:
	case <-p.sshDone:
	default:
	}
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

	var oldHostAgent string
	var oldHost string
	var oldPort int
	var oldFP string
	var oldSealed []byte
	var enabled bool
	var secretPlain string
	now := time.Now().UnixMilli()

	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		var oldHostNull sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT host, port, secret_ciphertext, host_key_fp, host_agent_id, enabled=1
			FROM device_ssh_credentials WHERE device_id=?`, deviceID).
			Scan(&oldHost, &oldPort, &oldSealed, &oldFP, &oldHostNull, &enabled)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		oldHostAgent = oldHostNull.String

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

		sealed := oldSealed
		if input.Secret != "" {
			sealed, err = s.settings.Seal([]byte("device-ssh-v1:"+deviceID), []byte(input.Secret))
			if err != nil {
				return err
			}
			secretPlain = input.Secret
		} else {
			plain, err := s.settings.Open([]byte("device-ssh-v1:"+deviceID), oldSealed)
			if err != nil {
				return err
			}
			secretPlain = string(plain)
		}

		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		fp := oldFP
		if host != oldHost || port != oldPort {
			fp = ""
		}
		enInt := 0
		if enabled {
			enInt = 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials
			SET host=?, port=?, username=?, auth_kind=?, secret_ciphertext=?, host_key_fp=?, host_agent_id=?, enabled=?, last_error='', updated_at=?
			WHERE device_id=?`, host, port, username, authKind, sealed, fp, hostAgentVal, enInt, now, deviceID); err != nil {
			return err
		}
		if err := audit(ctx, tx, userID, deviceID, "ssh.update", "{}", now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	})
	if err != nil {
		return SSHConfigStatus{}, err
	}

	if oldHostAgent != newHostAgent || !enabled {
		s.dispatchSSHDisconnect(oldHostAgent, deviceID)
	}
	if !enabled {
		s.closeLocalSSHTerminals(deviceID)
		s.applySSHState(ctx, proto.SSHStatePayload{SSHDeviceID: deviceID, State: proto.Offline})
	} else {
		fp := oldFP
		if host != oldHost || port != oldPort {
			fp = ""
		}
		if err := s.dispatchSSHConnect(ctx, newHostAgent, proto.SSHConnectPayload{
			SSHDeviceID: deviceID,
			Host:        host,
			Port:        port,
			Username:    username,
			AuthKind:    authKind,
			Secret:      secretPlain,
			HostKeyFP:   fp,
		}); err != nil {
			s.applySSHState(ctx, proto.SSHStatePayload{SSHDeviceID: deviceID, State: proto.Offline, Error: err.Error()})
		}
	}
	return s.SSHStatus(ctx, deviceID)
}

func (s *Service) ReconnectSSH(ctx context.Context, userID int64, deviceID string) error {
	if s.settings == nil {
		return errors.New("settings service is not configured")
	}
	var payload proto.SSHConnectPayload
	var sealed []byte
	var hostAgentID sql.NullString
	err := s.store.QueryRowContext(ctx, `SELECT host, port, username, auth_kind, secret_ciphertext, host_key_fp, host_agent_id
		FROM device_ssh_credentials WHERE device_id=?`, deviceID).
		Scan(&payload.Host, &payload.Port, &payload.Username, &payload.AuthKind, &sealed, &payload.HostKeyFP, &hostAgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMissing
	}
	if err != nil {
		return err
	}
	plain, err := s.settings.Open([]byte("device-ssh-v1:"+deviceID), sealed)
	if err != nil {
		return err
	}
	payload.SSHDeviceID = deviceID
	payload.Secret = string(plain)

	if err := s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials SET enabled=1, last_error='', updated_at=? WHERE device_id=?`, now, deviceID); err != nil {
			return err
		}
		if err := audit(ctx, tx, userID, deviceID, "ssh.reconnect", "{}", now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	}); err != nil {
		return err
	}

	s.dispatchSSHDisconnect(hostAgentID.String, deviceID)
	if err := s.dispatchSSHConnect(ctx, hostAgentID.String, payload); err != nil {
		s.applySSHState(ctx, proto.SSHStatePayload{SSHDeviceID: deviceID, State: proto.Offline, Error: err.Error()})
		return err
	}
	return nil
}

func (s *Service) DisconnectSSH(ctx context.Context, userID int64, deviceID string) error {
	var hostAgentID sql.NullString
	err := s.store.QueryRowContext(ctx, `SELECT host_agent_id FROM device_ssh_credentials WHERE device_id=?`, deviceID).Scan(&hostAgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMissing
	}
	if err != nil {
		return err
	}
	if err := s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE device_ssh_credentials SET enabled=0, last_error='', updated_at=? WHERE device_id=?`, now, deviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM device_sessions WHERE device_id=?`, deviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET conn_state='offline' WHERE id=?`, deviceID); err != nil {
			return err
		}
		if err := audit(ctx, tx, userID, deviceID, "ssh.disconnect", "{}", now); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, deviceID)
	}); err != nil {
		return err
	}
	s.dispatchSSHDisconnect(hostAgentID.String, deviceID)
	s.closeLocalSSHTerminals(deviceID)
	return nil
}

func (s *Service) disconnectSSHIfManaged(ctx context.Context, deviceID string) {
	var hostAgentID sql.NullString
	if err := s.store.QueryRowContext(ctx, `SELECT host_agent_id FROM device_ssh_credentials WHERE device_id=?`, deviceID).Scan(&hostAgentID); err == nil {
		s.dispatchSSHDisconnect(hostAgentID.String, deviceID)
		s.closeLocalSSHTerminals(deviceID)
	}
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
	var hostAgentID sql.NullString
	var enabled bool
	err := s.store.QueryRowContext(ctx, `SELECT host_agent_id, enabled=1 FROM device_ssh_credentials WHERE device_id=?`, deviceID).
		Scan(&hostAgentID, &enabled)
	if err != nil {
		return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "SSH 设备不存在"}, nil
	}
	if !enabled {
		return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "SSH 设备已断开连接"}, nil
	}

	if !hostAgentID.Valid || hostAgentID.String == "" {
		if s.sshRunner == nil {
			return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "服务端 SSH 执行引擎未启动"}, nil
		}
		return s.sshRunner.ExecuteTool(ctx, deviceID, call), nil
	}

	hostID := hostAgentID.String
	s.connectionMu.Lock()
	p := s.connections[hostID]
	s.connectionMu.Unlock()
	if p == nil {
		return proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID, Error: "选定的宿主 Agent 当前离线"}, nil
	}

	waitCh := make(chan proto.SSHMCPResultPayload, 1)
	s.sshPending.Store(call.CallID, sshCallWaiter{hostAgentID: hostID, ch: waitCh})
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
