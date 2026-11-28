package proto

import "encoding/json"

const (
	SSHConnectMessage    MessageType = "ssh.connect"
	SSHDisconnectMessage MessageType = "ssh.disconnect"
	SSHStateMessage      MessageType = "ssh.state"
	SSHCallMessage       MessageType = "ssh.mcp_call"
	SSHResultMessage     MessageType = "ssh.mcp_result"
)

const (
	SSHAuthPassword   = "password"
	SSHAuthPrivateKey = "private_key"
)

type SSHConnectPayload struct {
	SSHDeviceID string `json:"ssh_device_id"`
	AttemptID   string `json:"attempt_id,omitempty"` // An explicit login that ends when the SSH connection drops.
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	AuthKind    string `json:"auth_kind"`
	Secret      string `json:"secret"`
	HostKeyFP   string `json:"host_key_fp,omitempty"`
}

type SSHDisconnectPayload struct {
	SSHDeviceID string `json:"ssh_device_id"`
}

type SSHStatePayload struct {
	SSHDeviceID string    `json:"ssh_device_id"`
	AttemptID   string    `json:"attempt_id,omitempty"`
	State       ConnState `json:"state"`
	OSVersion   string    `json:"os_version,omitempty"`
	Arch        string    `json:"arch,omitempty"`
	HostKeyFP   string    `json:"host_key_fp,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type SSHMCPCallPayload struct {
	CallID         string          `json:"call_id"`
	TargetDeviceID string          `json:"target_device_id"`
	TaskID         string          `json:"task_id"`
	Tool           string          `json:"tool_name"`
	Arguments      json.RawMessage `json:"arguments"`
	Approved       bool            `json:"approved,omitempty"`
}

type SSHMCPResultPayload struct {
	CallID         string          `json:"call_id"`
	TargetDeviceID string          `json:"target_device_id"`
	TaskID         string          `json:"task_id"`
	Data           json.RawMessage `json:"data,omitempty"`
	Error          string          `json:"error,omitempty"`
}
