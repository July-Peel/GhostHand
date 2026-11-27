package proto

import (
	"encoding/json"
	"regexp"
)

const MaxScreenshotSize = 2 << 20

var TaskIDPattern = regexp.MustCompile("^t_[0-9a-f]{32}$")

type TaskAssignment struct {
	TaskID     string       `json:"task_id"`
	Capability CapabilityID `json:"capability"`
	Lane       Lane         `json:"lane"`
	ExpiresAt  int64        `json:"expires_at"`
	Title      string       `json:"title,omitempty"`
}

type TaskControl struct {
	TaskID string `json:"task_id"`
	Force  bool   `json:"force,omitempty"`
}

type TaskStatus struct {
	TaskID        string    `json:"task_id"`
	State         TaskState `json:"state"`
	StepIndex     int       `json:"step_index"`
	HeartbeatOnly bool      `json:"heartbeat_only,omitempty"`
}

type ScreenshotTicket struct {
	TaskID       string `json:"task_id"`
	RequestID    string `json:"request_id"`
	Token        string `json:"token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	AttachmentID string `json:"attachment_id,omitempty"`
	UploadID     string `json:"upload_id,omitempty"`
}

type ScreenshotRequest struct {
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id"`
	UploadID  string `json:"upload_id,omitempty"`
}

type ScreenshotResult struct {
	AttachmentID string `json:"attachment_id"`
}

func TaskCapabilities(platform Platform) []Capability {
	caps := []Capability{
		{ID: DeviceInfo, Version: 1, Lane: BackgroundLane, ParamsSchema: emptyParams},
	}
	if platform == Windows {
		caps = append(caps,
			Capability{ID: DeviceBattery, Version: 1, Lane: BackgroundLane, ParamsSchema: emptyParams},
			Capability{ID: ScreenCapture, Version: 1, Lane: UILane, ParamsSchema: emptyParams},
			Capability{ID: BrainTask, Version: 1, Lane: UILane, ParamsSchema: emptyParams})
	} else if platform == Linux {
		caps = append(caps,
			Capability{ID: BrainTask, Version: 1, Lane: UILane, ParamsSchema: emptyParams})
	}
	return caps
}

var emptyParams = json.RawMessage(`{"type":"object","additionalProperties":false}`)

func TaskCapabilitiesHash(platform Platform) string {
	switch platform {
	case Windows:
		return "m5-tasks-v2-windows-browser"
	case Linux:
		return "m5-tasks-v2-linux"
	default:
		return "m4-tasks-v1-" + string(platform)
	}
}

const (
	WindowsUninstallCapsHash         = "m5-tasks-v2-windows-browser-uninstall"
	PreviousWindowsUninstallCapsHash = "m45-tasks-v2-windows-uninstall"
	LegacyWindowsUninstallCapsHash   = "m4-tasks-v1-windows-uninstall"
	LinuxUninstallCapsHash           = "m5-tasks-v2-linux-uninstall"
)

func SupportsUninstall(hash string) bool {
	return hash == WindowsUninstallCapsHash || hash == PreviousWindowsUninstallCapsHash ||
		hash == "m45-tasks-v1-windows-uninstall" || hash == LegacyWindowsUninstallCapsHash ||
		hash == LinuxUninstallCapsHash
}

func SupportsBrain(platform Platform, hash string) bool {
	if platform == Windows {
		return hash == TaskCapabilitiesHash(platform) || hash == "m45-tasks-v2-windows" || hash == "m45-tasks-v1-windows" || hash == "m4-tasks-v1-windows" || SupportsUninstall(hash)
	}
	if platform == Linux {
		return hash == TaskCapabilitiesHash(platform) || hash == LinuxUninstallCapsHash
	}
	return false
}

func SupportsTasks(platform Platform, hash string) bool {
	return hash == TaskCapabilitiesHash(platform) || SupportsBrain(platform, hash) || hash == "m3-tasks-v1-"+string(platform)
}
