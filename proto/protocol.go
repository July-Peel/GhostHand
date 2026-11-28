// Package proto defines the GhostHand device protocol shared by the controller and Core.
package proto

import (
	"embed"
	"encoding/json"
)

const Version = 1

// Schemas contains the v1 JSON Schemas for non-Go consumers.
//
//go:embed schema/*.json
var Schemas embed.FS

type MessageType string

const (
	Hello              MessageType = "hello"
	Welcome            MessageType = "welcome"
	Ping               MessageType = "ping"
	Pong               MessageType = "pong"
	CapsReport         MessageType = "caps.report"
	StateUpdate        MessageType = "state.update"
	StateSleeping      MessageType = "state.sleeping"
	ConfigUpdate       MessageType = "config.update"
	TaskAssign         MessageType = "task.assign"
	TaskPause          MessageType = "task.pause"
	TaskResume         MessageType = "task.resume"
	TaskCancel         MessageType = "task.cancel"
	TaskConfirmReply   MessageType = "task.confirm_reply"
	TaskAccepted       MessageType = "task.accepted"
	TaskProgress       MessageType = "task.progress"
	TaskHeartbeat      MessageType = "task.heartbeat"
	TaskConfirmRequest MessageType = "task.confirm_request"
	TaskResult         MessageType = "task.result"
	UploadRequest      MessageType = "upload.request"
	UploadTicket       MessageType = "upload.ticket"
	FlowSync           MessageType = "flow.sync"
	AgentUpdate        MessageType = "agent.update"
	AgentRestart       MessageType = "agent.restart"
	HelperRestart      MessageType = "helper.restart"
	DeviceReboot       MessageType = "device.reboot"
	LogCollect         MessageType = "log.collect"
	AgentUpdateResult  MessageType = "agent.update_result"
	LogReady           MessageType = "log.ready"
	HelperState        MessageType = "helper.state"
	MCPToolCall        MessageType = "mcp.tool_call"
	MCPToolResult      MessageType = "mcp.tool_result"
	MCPTaskDisplay     MessageType = "mcp.task_display"
	MCPTaskFinish      MessageType = "mcp.task_finish"
)

type Envelope struct {
	Version   int             `json:"v"`
	Type      MessageType     `json:"type"`
	ID        string          `json:"id"`  // ULID used for deduplication.
	Seq       uint64          `json:"seq"` // Monotonic sender sequence.
	Ack       uint64          `json:"ack"` // Highest contiguous sequence received.
	Timestamp int64           `json:"ts"`  // Unix milliseconds.
	Payload   json.RawMessage `json:"payload"`
}

type ErrorCode string

const (
	ErrAuth              ErrorCode = "E_AUTH"
	ErrVersion           ErrorCode = "E_VERSION"
	ErrBusy              ErrorCode = "E_BUSY"
	ErrCapUnsupported    ErrorCode = "E_CAP_UNSUPPORTED"
	ErrHelperUnavailable ErrorCode = "E_HELPER_UNAVAILABLE"
	ErrDesktopLocked     ErrorCode = "E_DESKTOP_LOCKED"
	ErrUINotFound        ErrorCode = "E_UI_NOT_FOUND"
	ErrTimeout           ErrorCode = "E_TIMEOUT"
	ErrCancelled         ErrorCode = "E_CANCELLED"
	ErrUpload            ErrorCode = "E_UPLOAD"
	ErrFlowInvalid       ErrorCode = "E_FLOW_INVALID"
	ErrExternalUnknown   ErrorCode = "E_EXTERNAL_UNKNOWN"
	ErrInternal          ErrorCode = "E_INTERNAL"
)

type Platform string
type ConnState string
type Lane string
type TaskState string

const (
	Windows Platform = "windows"
	Android Platform = "android"
	Linux   Platform = "linux"

	Online   ConnState = "online"
	Unstable ConnState = "unstable"
	Sleeping ConnState = "sleeping"
	Offline  ConnState = "offline"

	UILane         Lane = "ui"
	BackgroundLane Lane = "bg"

	Queued      TaskState = "queued"
	Dispatched  TaskState = "dispatched"
	Running     TaskState = "running"
	Paused      TaskState = "paused"
	WaitingUser TaskState = "waiting_user"
	Cancelling  TaskState = "cancelling"
	Cancelled   TaskState = "cancelled"
	Expired     TaskState = "expired"
	Succeeded   TaskState = "succeeded"
	Failed      TaskState = "failed"
	Unknown     TaskState = "unknown"
)

type DeviceState struct {
	HelperState  string `json:"helper_state,omitempty"`
	DesktopState string `json:"desktop_state,omitempty"`
	BatteryPct   *int   `json:"battery_pct,omitempty"`
	Charging     *bool  `json:"charging,omitempty"`
	HeartbeatSec *int   `json:"heartbeat_sec,omitempty"`
}

type RunningTask struct {
	TaskID    string    `json:"task_id"`
	State     TaskState `json:"state"`
	StepIndex int       `json:"step_index"`
}

type HelloPayload struct {
	DeviceID           string        `json:"device_id"`
	Token              string        `json:"token"`
	AgentVersion       string        `json:"agent_ver"`
	CoreSHA256         string        `json:"core_sha256,omitempty"`
	Platform           Platform      `json:"platform"`
	LastAck            uint64        `json:"last_ack"`
	Fresh              bool          `json:"fresh,omitempty"`
	CapsHash           string        `json:"caps_hash"`
	RunningTasks       []RunningTask `json:"running_tasks"`
	TaskInventory      bool          `json:"task_inventory,omitempty"`
	DesktopOperations  bool          `json:"desktop_operations,omitempty"`
	TerminalOperations bool          `json:"terminal_operations,omitempty"`
	PendingTaskResults []string      `json:"pending_task_results,omitempty"`
	State              DeviceState   `json:"state"`
}

type WelcomePayload struct {
	ConnGen       uint64         `json:"conn_gen"`
	HeartbeatSec  int            `json:"heartbeat_sec"`
	ServerTime    int64          `json:"server_time"`
	LastAck       uint64         `json:"last_ack"`
	NeedCaps      bool           `json:"need_caps"`
	TokenRotate   string         `json:"token_rotate,omitempty"`
	TaskHeartbeat bool           `json:"task_heartbeat,omitempty"`
	CoreUpdateID  string         `json:"core_update_id,omitempty"`
	DesktopPolicy *DesktopPolicy `json:"desktop_policy,omitempty"`
}

type CapabilityID string

const (
	DeviceInfo    CapabilityID = "device.info"
	DeviceBattery CapabilityID = "device.battery"
	ScreenCapture CapabilityID = "screen.capture"
)

type Capability struct {
	ID                CapabilityID    `json:"id"`
	Version           int             `json:"version"`
	Lane              Lane            `json:"lane"`
	DisabledByDefault bool            `json:"disabled_by_default,omitempty"`
	ParamsSchema      json.RawMessage `json:"params_schema"`
}

type CapsReportPayload struct {
	Capabilities []Capability  `json:"capabilities"`
	FlowsCached  []string      `json:"flows_cached"`
	Tools        []MCPTool     `json:"tools,omitempty"`
	Resources    []MCPResource `json:"resources,omitempty"`
}

type TaskRef struct {
	TaskID string `json:"task_id"`
}

type TaskProgressPayload struct {
	TaskID       string  `json:"task_id"`
	StepIndex    int     `json:"step_index"`
	StepTotal    int     `json:"step_total"`
	StepKey      string  `json:"step_key"`
	StepName     string  `json:"step_name"`
	Status       string  `json:"status"`
	Attempt      int     `json:"attempt"`
	ElapsedMS    int64   `json:"elapsed_ms"`
	Detail       string  `json:"detail"`
	AttachmentID *string `json:"attachment_id"`
	TimeoutMS    int64   `json:"timeout_ms,omitempty"`
}

type TaskResultPayload struct {
	TaskID    string          `json:"task_id"`
	State     TaskState       `json:"state"`
	Result    json.RawMessage `json:"result,omitempty"`
	ErrorCode ErrorCode       `json:"error_code,omitempty"`
}
