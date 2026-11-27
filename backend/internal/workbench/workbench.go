package workbench

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/proto"
)

const (
	pageSize     = 60
	taskTTL      = 30 * time.Minute
	brainTaskTTL = 90 * time.Minute
)

var (
	ErrDeviceNotFound = errors.New("device not found")
	ErrTaskNotFound   = errors.New("task not found")
	ErrTaskControl    = errors.New("task control conflicts with current state")
	ErrUnknownCommand = errors.New("unknown command")
	ErrUnsupported    = errors.New("device does not support this command")
)

type Device struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Platform     proto.Platform  `json:"platform"`
	Arch         string          `json:"arch"`
	OSVersion    string          `json:"os_version"`
	Group        string          `json:"group"`
	ConnState    proto.ConnState `json:"conn_state"`
	HelperState  string          `json:"helper_state"`
	DesktopState string          `json:"desktop_state"`
	TaskState    string          `json:"task_state"`
	BatteryPct   *int            `json:"battery_pct"`
	Charging     *bool           `json:"charging"`
	LastSeenAt   int64           `json:"last_seen_at"`
	TaskProtocol bool            `json:"task_protocol"`
	Demo         bool            `json:"is_demo"`
	AutoApprove  bool            `json:"auto_approve"`
}

type Message struct {
	ID        int64           `json:"id"`
	Sender    string          `json:"sender"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt int64           `json:"created_at"`
}

type Page struct {
	Messages []Message `json:"messages"`
	HasOlder bool      `json:"has_older"`
}

type TaskCard struct {
	ID         string          `json:"id"`
	Source     string          `json:"source"`
	State      proto.TaskState `json:"state"`
	Title      string          `json:"title"`
	StepIndex  int             `json:"step_index"`
	StepTotal  int             `json:"step_total"`
	StepName   string          `json:"step_name"`
	Timeline   json.RawMessage `json:"timeline"`
	ExpiresAt  int64           `json:"expires_at"`
	StartedAt  *int64          `json:"started_at"`
	FinishedAt *int64          `json:"finished_at"`
	Lane       proto.Lane      `json:"lane"`
	ElapsedMS  int64           `json:"elapsed_ms"`
	ErrorCode  proto.ErrorCode `json:"error_code,omitempty"`
	Control    string          `json:"control,omitempty"`
	CancelAt   *int64          `json:"cancel_at"`
	Stalled    bool            `json:"stalled"`
}

type Command struct {
	Slash      string             `json:"slash"`
	Title      string             `json:"title"`
	Capability proto.CapabilityID `json:"capability"`
	Step       string             `json:"-"`
	Lane       proto.Lane         `json:"-"`
}

var commands = []Command{
	{Slash: "/battery", Title: "查电量", Capability: proto.DeviceBattery, Step: "读取电源状态", Lane: proto.BackgroundLane},
	{Slash: "/info", Title: "查设备信息", Capability: proto.DeviceInfo, Step: "读取设备信息", Lane: proto.BackgroundLane},
	{Slash: "/screenshot", Title: "截图", Capability: proto.ScreenCapture, Step: "截取主显示器", Lane: proto.UILane},
}

func Commands() []Command {
	return slices.Clone(commands)
}

func findCommand(text string) (Command, error) {
	for _, command := range commands {
		if strings.EqualFold(text, command.Slash) || text == command.Title {
			return command, nil
		}
	}
	if strings.HasPrefix(text, "/") {
		return Command{}, ErrUnknownCommand
	}
	return Command{}, nil
}

func (command Command) supported(device Device) bool {
	if device.Demo {
		return command.Capability != proto.ScreenCapture
	}
	for _, cap := range proto.TaskCapabilities(device.Platform) {
		if command.Capability == cap.ID {
			return true
		}
	}
	return false
}

func commandFor(capability proto.CapabilityID) Command {
	for _, command := range commands {
		if command.Capability == capability {
			return command
		}
	}
	return Command{Title: string(capability), Step: "读取设备信息"}
}

type Service struct {
	desktopUnlocker DesktopUnlocker
	store           *db.Store
	demo            bool
	fileDir         string
	config          *settings.Service
	dataDir         string
	brainMu         sync.Mutex
	brainSessions   map[string]map[string]context.CancelFunc
	refreshSeq      atomic.Int64
}

func New(store *db.Store, config *settings.Service, dataDir string, demo bool) (*Service, error) {
	dir := filepath.Join(dataDir, "files", "screenshots")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Service{
		store: store, config: config, demo: demo, fileDir: dir, dataDir: dataDir,
		brainSessions: make(map[string]map[string]context.CancelFunc),
	}, nil
}

const deviceFields = `
	d.id, d.name, d.platform, d.arch, d.os_version, d.group_name, d.conn_state, d.helper_state,
	d.desktop_state, CASE WHEN EXISTS (SELECT 1 FROM tasks t WHERE t.device_id=d.id AND t.state='waiting_user')
	THEN 'waiting_user' WHEN EXISTS (SELECT 1 FROM tasks t WHERE t.device_id=d.id AND t.state IN ('running','paused','cancelling'))
	THEN 'running' ELSE 'idle' END, d.battery_pct, d.charging, d.last_seen_at, d.task_protocol>=1, d.is_demo, d.auto_approve=1
`

type scanner interface {
	Scan(...any) error
}

func scanDevice(row scanner) (Device, error) {
	var d Device
	err := row.Scan(&d.ID, &d.Name, &d.Platform, &d.Arch, &d.OSVersion, &d.Group, &d.ConnState,
		&d.HelperState, &d.DesktopState, &d.TaskState, &d.BatteryPct, &d.Charging, &d.LastSeenAt, &d.TaskProtocol, &d.Demo, &d.AutoApprove)
	return d, err
}

func (s *Service) SetAutoApprove(ctx context.Context, userID int64, deviceID string, enabled bool) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM devices WHERE id=? AND (is_demo=0 OR ?))", deviceID, s.demo).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrDeviceNotFound
		}
		val := 0
		if enabled {
			val = 1
		}
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET auto_approve=? WHERE id=?", val, deviceID); err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		if enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE confirmations SET decision='approved', decided_at=?
				WHERE decision='pending' AND task_id IN (SELECT id FROM tasks WHERE device_id=? AND state IN ('waiting_user','paused'))`, now, deviceID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='running' WHERE device_id=? AND state='waiting_user'`, deviceID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(user_id, device_id, action, detail_json, created_at)
			VALUES (?, ?, 'device.policy.auto_approve', json_object('auto_approve', ?), ?)`, userID, deviceID, enabled, now); err != nil {
			return err
		}
		return RecordChange(ctx, tx, deviceID)
	})
}

func (s *Service) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.store.QueryContext(ctx, `SELECT `+deviceFields+`
		FROM devices d WHERE d.is_demo=0 OR ?
		ORDER BY CASE d.conn_state WHEN 'online' THEN 0 WHEN 'unstable' THEN 1 WHEN 'sleeping' THEN 2 ELSE 3 END,
		         d.group_name, d.name, d.id
	`, s.demo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := make([]Device, 0)
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (s *Service) DemoEnabled() bool {
	return s.demo
}

func (s *Service) Messages(ctx context.Context, deviceID string, before int64) (Page, error) {
	var visible bool
	if err := s.store.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM devices WHERE id=? AND (is_demo=0 OR ?))",
		deviceID, s.demo).Scan(&visible); err != nil {
		return Page{}, err
	}
	if !visible {
		return Page{}, ErrDeviceNotFound
	}
	rows, err := s.store.QueryContext(ctx, `
		SELECT m.id, m.sender, m.type, m.text,
		       CASE WHEN m.type='confirm_card' THEN json_set(m.payload_json,'$.decision',
		         COALESCE((SELECT decision FROM confirmations WHERE id=json_extract(m.payload_json,'$.id')), 'cancelled'))
		         ELSE m.payload_json END, m.created_at,
		       COALESCE(t.id, ''), COALESCE(t.capability, ''), COALESCE(t.state, ''),
		       COALESCE(t.step_index, 0), COALESCE(t.expires_at, 0), t.started_at, t.finished_at,
		       COALESCE(t.step_total, 0), COALESCE(t.step_name, ''), COALESCE(t.lane, ''),
		       COALESCE(t.elapsed_ms, 0), COALESCE(t.error_code, ''), COALESCE(t.control, ''), t.cancel_at,
		       COALESCE(t.title,''),COALESCE(t.stalled,0),COALESCE(t.source,''),
		       CASE WHEN t.id IS NULL THEN '[]' ELSE (
		           SELECT json_group_array(json_object(
		               'seq', seq, 'step_key', step_key, 'step_name', step_name, 'status', status,
		               'detail', detail, 'elapsed_ms', elapsed_ms, 'attempt', attempt, 'created_at', created_at
		           ))
		           FROM (SELECT seq, step_key, step_name, status, detail, elapsed_ms, attempt, created_at
		                 FROM task_events WHERE task_id=t.id ORDER BY seq)
		       ) END
		FROM chat_messages m LEFT JOIN tasks t ON t.id=m.task_id AND m.type='task_card'
		WHERE m.device_id=? AND (?=0 OR m.id<?)
		ORDER BY m.id DESC LIMIT ?
	`, deviceID, before, before, pageSize+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Messages: make([]Message, 0)}
	for rows.Next() {
		var message Message
		var card TaskCard
		var payload *string
		var capability proto.CapabilityID
		var timeline string
		if err := rows.Scan(&message.ID, &message.Sender, &message.Type, &message.Text, &payload, &message.CreatedAt,
			&card.ID, &capability, &card.State, &card.StepIndex, &card.ExpiresAt, &card.StartedAt,
			&card.FinishedAt, &card.StepTotal, &card.StepName, &card.Lane,
			&card.ElapsedMS, &card.ErrorCode, &card.Control, &card.CancelAt, &card.Title, &card.Stalled, &card.Source, &timeline); err != nil {
			return Page{}, err
		}
		switch {
		case card.ID != "":
			if card.Title == "" {
				card.Title = commandFor(capability).Title
			}
			card.Timeline = json.RawMessage(timeline)
			if card.StepName == "" {
				switch card.StepIndex {
				case 1:
					card.StepName = commandFor(capability).Step
				case 2:
					card.StepName = "整理结果"
				}
			}
			message.Payload, err = json.Marshal(card)
			if err != nil {
				return Page{}, err
			}
		case payload != nil:
			message.Payload = json.RawMessage(*payload)
		}
		page.Messages = append(page.Messages, message)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Messages) > pageSize {
		page.HasOlder = true
		page.Messages = page.Messages[:pageSize]
	}
	slices.Reverse(page.Messages)
	return page, nil
}

func (s *Service) Send(ctx context.Context, userID int64, deviceID, text string, images []UploadImage) error {
	if len(images) > MaxChatImages {
		return ErrChatImageCount
	}
	command, err := findCommand(text)
	if len(images) > 0 && (command.Capability != "" || errors.Is(err, ErrUnknownCommand)) {
		return ErrCommandImages
	}
	if err != nil {
		return err
	}
	var configs map[settings.Kind]settings.Configuration
	if command.Capability == "" {
		configs, err = s.config.Read(ctx)
		if err != nil {
			return err
		}
	}
	var stored []storedImage
	if len(images) > 0 {
		var exists bool
		if err := s.store.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM devices WHERE id=? AND (is_demo=0 OR ?))",
			deviceID, s.demo).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrDeviceNotFound
		}
		stored, err = s.stageChatImages(ctx, images, configs[settings.WebDAV])
		if err != nil {
			return err
		}
	}
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		device, err := scanDevice(tx.QueryRowContext(ctx, "SELECT "+deviceFields+
			" FROM devices d WHERE d.id=? AND (d.is_demo=0 OR ?)", deviceID, s.demo))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeviceNotFound
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if len(stored) > 0 {
			if err := insertChatImageTask(ctx, tx, device, userID, text, stored, brainUnavailable(device, configs[settings.LLM]), now); err != nil {
				return err
			}
			return RecordChange(ctx, tx, deviceID)
		}
		if command.Capability != "" && !command.supported(device) {
			return ErrUnsupported
		}
		if command.Capability == proto.DeviceBattery {
			var duplicate bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE device_id=?
				AND capability=? AND created_at>=? AND state NOT IN ('cancelled','expired','failed','unknown','cancelling'))`,
				deviceID, command.Capability, now.Add(-5*time.Second).UnixMilli()).Scan(&duplicate); err != nil || duplicate {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_messages(device_id, sender, type, text, created_at) VALUES (?, 'user', 'text', ?, ?)
		`, deviceID, text, now.UnixMilli()); err != nil {
			return err
		}
		if command.Capability != "" {
			if err := createTask(ctx, tx, userID, device, command, "", now); err != nil {
				return err
			}
		} else if err := s.queueBrain(ctx, tx, device, userID, text, configs[settings.LLM], now); err != nil {
			return err
		}
		return RecordChange(ctx, tx, deviceID)
	})
	if err != nil {
		s.deleteStagedImages(ctx, stored)
	}
	return err
}

func (s *Service) stageChatImages(ctx context.Context, images []UploadImage, remote settings.Configuration) ([]storedImage, error) {
	stored := make([]storedImage, 0, len(images))
	now := time.Now()
	for _, image := range images {
		file, err := s.saveUserAttachment(ctx, image, remote, now)
		if err != nil {
			s.deleteStagedImages(ctx, stored)
			return nil, err
		}
		stored = append(stored, file)
	}
	return stored, nil
}

func (s *Service) deleteStagedImages(ctx context.Context, stored []storedImage) {
	for _, image := range stored {
		s.deleteAttachmentFiles(ctx, image.AttachmentFiles)
	}
}

func insertChatImageTask(ctx context.Context, tx *sql.Tx, device Device, user int64, prompt string, images []storedImage, unavailable string, now time.Time) error {
	title := limitText(prompt, 80)
	if title == "" {
		title = "图片分析任务"
	}
	task, err := insertTask(ctx, tx, user, device, brainCommand, prompt, "brain", title, now, brainTaskTTL, true)
	if err != nil {
		return err
	}
	cards := make([]imageCard, 0, len(images))
	for i, image := range images {
		if err := image.insert(ctx, tx, task, fmt.Sprintf("user_%d", i), now); err != nil {
			return err
		}
		cards = append(cards, imageCard{ID: image.ID, Name: image.name, Size: image.size})
	}
	payload, err := json.Marshal(userMediaPayload{Attachments: cards})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,text,task_id,payload_json,created_at)
		VALUES (?,'user','text',?,?,?,?)`, device.ID, prompt, task, string(payload), now.UnixMilli()); err != nil {
		return err
	}
	if unavailable != "" {
		if err := finishTask(ctx, tx, task, proto.Failed, "", now.UnixMilli()); err != nil {
			return err
		}
		return addReply(ctx, tx, device.ID, task, unavailable, now.UnixMilli())
	}
	return addTaskCard(ctx, tx, device.ID, task, now)
}

// ClearMessages deletes all chat history, tasks, and active LLM sessions for a device.
func (s *Service) ClearMessages(ctx context.Context, userID int64, deviceID string) error {
	var files []AttachmentFiles
	var stopped []string
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM devices WHERE id=? AND (is_demo=0 OR ?))",
			deviceID, s.demo).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrDeviceNotFound
		}

		rows, err := tx.QueryContext(ctx, `SELECT t.id, t.control='force'
			FROM tasks t JOIN devices d ON d.id=t.device_id
			WHERE t.device_id=? AND d.is_demo=0 AND t.assign_seq IS NOT NULL
			AND t.state IN ('queued','dispatched','running','paused','waiting_user','cancelling')`, deviceID)
		if err != nil {
			return err
		}
		type activeAssign struct {
			id    string
			force bool
		}
		var active []activeAssign
		for rows.Next() {
			var item activeAssign
			if err := rows.Scan(&item.id, &item.force); err != nil {
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
			if err := enqueueTaskCancel(ctx, tx, deviceID, item.id, item.force, true); err != nil {
				return err
			}
			stopped = append(stopped, item.id)
		}

		files, err = DeviceAttachmentFiles(ctx, tx, deviceID)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, "DELETE FROM chat_messages WHERE device_id=?", deviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE device_id=?", deviceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(user_id, device_id, action, detail_json, created_at)
			VALUES (?, ?, 'chat.clear', '{}', ?)`, userID, deviceID, time.Now().UnixMilli()); err != nil {
			return err
		}
		return RecordChange(ctx, tx, deviceID)
	})
	if err != nil {
		return err
	}
	for _, task := range stopped {
		s.cancelBrainSession(deviceID, task)
	}
	for _, file := range files {
		s.deleteAttachmentFiles(ctx, file)
	}
	return nil
}

func (s *Service) MarkRefresh() {
	s.refreshSeq.Add(1)
}

func (s *Service) RefreshSeq() int64 {
	return s.refreshSeq.Load()
}

// CleanupDeviceState cancels active LLM sessions, deletes attachment files, and notifies browsers to refresh.
func (s *Service) CleanupDeviceState(ctx context.Context, deviceID string, files []AttachmentFiles) {
	s.cancelDeviceBrainSessions(deviceID)
	for _, file := range files {
		s.deleteAttachmentFiles(ctx, file)
	}
	s.MarkRefresh()
}

func createTask(ctx context.Context, tx *sql.Tx, userID int64, device Device, command Command, prompt string, now time.Time) error {
	source, title := "quick", command.Title
	ttl := taskTTL
	if command.Capability == proto.BrainTask {
		source, title = "brain", limitText(prompt, 80)
		ttl = brainTaskTTL
	}
	_, err := insertTask(ctx, tx, userID, device, command, prompt, source, title, now, ttl, false)
	return err
}

func insertTask(ctx context.Context, tx *sql.Tx, userID int64, device Device, command Command, prompt, source, title string,
	now time.Time, ttl time.Duration, quiet bool) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	id := "t_" + hex.EncodeToString(bytes[:])
	expires := now.Add(ttl)
	var due any
	if device.Demo {
		due = expires.UnixMilli()
		if device.ConnState == proto.Online {
			due = now.Add(400 * time.Millisecond).UnixMilli()
		}
	}
	total := 1
	if device.Demo || command.Lane == proto.UILane {
		total = 2
	}
	if command.Capability == proto.BrainTask {
		total = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks(id, device_id, created_by, capability, state, created_at, expires_at, next_at, lane, step_total, source, prompt, title)
		VALUES (?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, device.ID, userID, command.Capability, now.UnixMilli(), expires.UnixMilli(), due, command.Lane, total, source, prompt, title); err != nil {
		return "", err
	}
	if !quiet {
		if err := addTaskCard(ctx, tx, device.ID, id, now); err != nil {
			return "", err
		}
	}
	if err := addTaskEvent(ctx, tx, id, "queue", "queued", "任务已入队", now.UnixMilli()); err != nil {
		return "", err
	}
	return id, auditTask(ctx, tx, userID, device.ID, id, "task.create", now.UnixMilli())
}

func addTaskCard(ctx context.Context, tx *sql.Tx, device, task string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO chat_messages(device_id,sender,type,task_id,created_at)
		VALUES (?,'agent','task_card',?,?)`, device, task, now.UnixMilli())
	return err
}

func auditTask(ctx context.Context, tx *sql.Tx, userID int64, deviceID, taskID, action string, now int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_logs(user_id, device_id, action, detail_json, created_at)
		VALUES (?, ?, ?, json_object('task_id', ?), ?)
	`, userID, deviceID, action, taskID, now)
	return err
}

func addTaskEvent(ctx context.Context, tx *sql.Tx, taskID, step, status, detail string, now int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO task_events(task_id, seq, step_key, status, detail, created_at)
		SELECT ?, COALESCE(MAX(seq), 0)+1, ?, ?, ?, ? FROM task_events WHERE task_id=?
	`, taskID, step, status, detail, now, taskID)
	return err
}
