package proto

const (
	TerminalOpenMessage   MessageType = "terminal.open"
	TerminalInputMessage  MessageType = "terminal.input"
	TerminalResizeMessage MessageType = "terminal.resize"
	TerminalCloseMessage  MessageType = "terminal.close"
	TerminalOutputMessage MessageType = "terminal.output"
	TerminalExitMessage   MessageType = "terminal.exit"
)

const (
	TerminalChunkSize   = 16 << 10
	MaxTerminalSessions = 8
	// A JSON string can take up to six bytes per input byte after escaping.
	MaxTerminalFrameSize = 6*TerminalChunkSize + (1 << 10)
)

type TerminalShell string

const (
	TerminalShellCMD        TerminalShell = "cmd"
	TerminalShellPowerShell TerminalShell = "powershell"
	TerminalShellSh         TerminalShell = "sh"
)

func (s TerminalShell) Valid() bool {
	return s == TerminalShellCMD || s == TerminalShellPowerShell || s == TerminalShellSh
}

// TerminalFrame is carried on the live device connection with Seq and Ack zero.
type TerminalFrame struct {
	SessionID      string        `json:"session_id"`
	TargetDeviceID string        `json:"target_device_id,omitempty"`
	Shell          TerminalShell `json:"shell,omitempty"`
	Cols           int           `json:"cols,omitempty"`
	Rows           int           `json:"rows,omitempty"`
	Data           string        `json:"data,omitempty"`
	ExitCode       *int          `json:"exit_code,omitempty"`
	Error          string        `json:"error,omitempty"`
}

func TerminalDimensions(cols, rows int) (int, int) {
	return min(max(cols, 20), 500), min(max(rows, 5), 200)
}
