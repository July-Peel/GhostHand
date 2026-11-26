package proto

import "encoding/json"

const (
	BrowserExtensionStatusRequest MessageType = "browser.ext_status"
	BrowserExtensionStatusResult  MessageType = "browser.ext_status_result"
	RDPPreflight                  MessageType = "rdp.preflight"
	RDPUnlock                     MessageType = "rdp.unlock"
	RDPLock                       MessageType = "rdp.lock"
	RDPResult                     MessageType = "rdp.result"
)

const (
	ErrRDPNotSupported      ErrorCode = "E_RDP_NOT_SUPPORTED"
	ErrRDPDisabled          ErrorCode = "E_RDP_DISABLED"
	ErrRDPPortClosed        ErrorCode = "E_RDP_PORT_CLOSED"
	ErrRDPNotServiceMode    ErrorCode = "E_RDP_NOT_SERVICE_MODE"
	ErrRDPUserMismatch      ErrorCode = "E_RDP_USER_MISMATCH"
	ErrRDPAuthFailed        ErrorCode = "E_RDP_AUTH_FAILED"
	ErrRDPAccountLocked     ErrorCode = "E_RDP_ACCOUNT_LOCKED"
	ErrRDPAccessDenied      ErrorCode = "E_RDP_ACCESS_DENIED"
	ErrRDPTsconFailed       ErrorCode = "E_RDP_TSCON_FAILED"
	ErrRDPTicketExpired     ErrorCode = "E_RDP_TICKET_EXPIRED"
	ErrRDPTimeout           ErrorCode = "E_RDP_TIMEOUT"
	ErrRDPClientUnavailable ErrorCode = "E_RDP_CLIENT_UNAVAILABLE"
	ErrRDPCertificate       ErrorCode = "E_RDP_CERTIFICATE"
	ErrRDPFirewall          ErrorCode = "E_RDP_FIREWALL"
	ErrRDPConnection        ErrorCode = "E_RDP_CONNECTION"
)

const ErrRDPBlankPasswordRestricted ErrorCode = "E_RDP_BLANK_PASSWORD_RESTRICTED"

type RDPCredential struct {
	Username string `json:"username"`
	Domain   string `json:"domain"`
	Password []byte `json:"password"`
}

type RDPPreflightDetail struct {
	Edition                 string            `json:"edition"`
	OSVersion               string            `json:"os_version"`
	Supported               bool              `json:"rdp_supported"`
	Enabled                 bool              `json:"rdp_enabled"`
	TermServiceRunning      bool              `json:"termservice_running"`
	NLARequired             bool              `json:"nla_required"`
	PortReachable           bool              `json:"port_reachable"`
	ServiceMode             bool              `json:"service_mode"`
	ConsoleSession          RDPConsoleSession `json:"console_session"`
	SleepACMinutes          *int              `json:"sleep_ac_minutes"`
	SleepDCMinutes          *int              `json:"sleep_dc_minutes"`
	FirewallInboundOpen     *bool             `json:"firewall_rdp_inbound_open"`
	ClientAvailable         bool              `json:"client_available"`
	CertificateAvailable    bool              `json:"certificate_available"`
	SingleSession           bool              `json:"single_session"`
	BlankPasswordRestricted bool              `json:"blank_password_restricted"`
}

type RDPConsoleSession struct {
	ID       uint32 `json:"id"`
	State    string `json:"state"`
	Username string `json:"username"`
}

type RDPUnlockDetail struct {
	SessionID       uint32 `json:"session_id"`
	ElapsedMS       int64  `json:"elapsed_ms"`
	AlreadyUnlocked bool   `json:"already_unlocked"`
}

type DesktopPolicy struct {
	KeepSystemAwake bool `json:"keep_system_awake"`
}

type DesktopRequest struct {
	OperationID string `json:"operation_id"`
	Ticket      string `json:"ticket,omitempty"`
	Port        int    `json:"port,omitempty"`
	SessionID   uint32 `json:"session_id,omitempty"`
	ExpiresAt   int64  `json:"expires_at"`
}

type DesktopResult struct {
	OperationID string          `json:"operation_id"`
	Kind        MessageType     `json:"kind"`
	OK          bool            `json:"ok"`
	Stage       string          `json:"stage,omitempty"`
	Code        ErrorCode       `json:"code,omitempty"`
	Detail      json.RawMessage `json:"detail,omitempty"`
}

type BrowserExtensionStatus struct {
	Browser            string `json:"browser"`
	ExtensionID        string `json:"extension_id"`
	Version            string `json:"version"`
	Connected          bool   `json:"connected"`
	Installed          bool   `json:"installed"`
	Registered         bool   `json:"registered"`
	BundlePresent      bool   `json:"bundle_present"`
	AlreadyInstalled   bool   `json:"already_installed,omitempty"`
	AlreadyUninstalled bool   `json:"already_uninstalled,omitempty"`
	Error              string `json:"error,omitempty"`
}
