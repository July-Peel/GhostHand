package proto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"time"
)

const (
	AgentRollback   MessageType = "agent.rollback"
	AgentUninstall  MessageType = "agent.uninstall"
	OperationResult MessageType = "operation.result"
)

const (
	MaxReleaseSize      = 96 << 20
	MaxLogSize          = 8 << 20
	AgentSocketPath     = "/api/agent/connect"
	CoreUpdatePath      = "/api/agent/core-updates/"
	CoreUpdateComponent = "core-binary"
	MaxCoreUpdateSize   = 256 << 20
)

type Enrollment struct {
	Key       string   `json:"key"`
	MachineID string   `json:"machine_id"`
	Token     string   `json:"token"`
	Name      string   `json:"name"`
	Group     string   `json:"group,omitempty"`
	Platform  Platform `json:"platform"`
	Arch      string   `json:"arch"`
	OSVersion string   `json:"os_version"`
}

type EnrollmentResult struct {
	DeviceID  string `json:"device_id"`
	UpdateKey string `json:"update_key"`
}

type Release struct {
	ID        string   `json:"id"`
	Version   string   `json:"version"`
	Platform  Platform `json:"platform"`
	Arch      string   `json:"arch"`
	Component string   `json:"component"`
	Size      int64    `json:"size"`
	SHA256    string   `json:"sha256"`
	Signature string   `json:"signature"`
}

func (r Release) SignedBytes() []byte {
	manifest := r
	manifest.Signature = ""
	data, _ := json.Marshal(manifest)
	return append([]byte("ghosthand-release-v1\n"), data...)
}

type Operation struct {
	ID      string   `json:"id"`
	Hours   int      `json:"hours,omitempty"`
	Release *Release `json:"release,omitempty"`
}

type OperationOutcome struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
	Detail  string `json:"detail"`
}

type LogTicket struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

type CapabilitiesReport struct {
	CapsReportPayload
	Hash string `json:"hash"`
}

func NewEnvelope(kind MessageType, payload any) (Envelope, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	id, err := NewID()
	return Envelope{Version: Version, Type: kind, ID: id, Timestamp: time.Now().UnixMilli(), Payload: data}, err
}

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[6:]); err != nil {
		return "", err
	}
	n := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	number := new(big.Int).SetBytes(b[:])
	divisor := big.NewInt(32)
	var remainder big.Int
	var encoded [26]byte
	for i := len(encoded) - 1; i >= 0; i-- {
		number.QuoRem(number, divisor, &remainder)
		encoded[i] = alphabet[remainder.Int64()]
	}
	return string(encoded[:]), nil
}

func NewToken() (string, error) {
	var b [32]byte
	_, err := rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:]), err
}

func TokenHash(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return "", errors.New("invalid token")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
