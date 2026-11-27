package settings

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/storage"
)

type Kind string

const (
	WebDAV Kind = "webdav"
	LLM    Kind = "llm"
)

type ValidationError string

func (e ValidationError) Error() string { return string(e) }

type WebDAVSetupError struct{ err error }

func (e WebDAVSetupError) Error() string { return e.err.Error() }
func (e WebDAVSetupError) Unwrap() error { return e.err }

// Configuration is internal. The authenticated settings API opts in to returning credentials.
type Configuration struct {
	Endpoint     string
	Label        string // WebDAV username or LLM model.
	Secret       string `json:"-"`
	Instructions string
}

func (c Configuration) FreeChatEnabled() bool {
	return c.Endpoint != "" && c.Label != "" && strings.TrimSpace(c.Secret) != ""
}

type Update struct {
	Endpoint     string
	Label        string
	Secret       *string // nil keeps the credential, an empty string removes it.
	Instructions *string // nil keeps the saved prompt, an empty string uses the default.
}

type Service struct {
	store *db.Store
	aead  cipher.AEAD
}

func New(ctx context.Context, store *db.Store, dir string) (*Service, error) {
	key, err := loadKey(ctx, store, filepath.Join(dir, "settings.key"))
	if err != nil {
		return nil, err
	}
	defer clear(key)
	if len(key) != 32 {
		return nil, errors.New("settings.key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, aead: aead}, nil
}

func loadKey(ctx context.Context, store *db.Store, path string) ([]byte, error) {
	if key, err := os.ReadFile(path); !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	var hasCredentials bool
	if err := store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM integration_settings WHERE secret IS NOT NULL)
		OR EXISTS(SELECT 1 FROM attachments WHERE storage_ref IS NOT NULL)
		OR EXISTS(SELECT 1 FROM chat_attachment_deletions WHERE storage_ref IS NOT NULL)
		OR EXISTS(SELECT 1 FROM device_rdp_credentials)`).
		Scan(&hasCredentials); err != nil {
		return nil, err
	}
	if hasCredentials {
		return nil, errors.New("missing settings.key for saved credentials")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	saved := false
	defer func() {
		file.Close()
		if !saved {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(key); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	saved = true
	return key, nil
}

func (s *Service) Seal(aad, plain []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, aad), nil
}

func (s *Service) SealedSize(plainSize int) int {
	return s.aead.NonceSize() + plainSize + s.aead.Overhead()
}

func (s *Service) Open(aad, sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < s.SealedSize(0) {
		return nil, errors.New("invalid encrypted credential")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], aad)
}

func (s *Service) Read(ctx context.Context) (map[Kind]Configuration, error) {
	rows, err := s.store.QueryContext(ctx, "SELECT kind, endpoint, label, secret, instructions FROM integration_settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[Kind]Configuration, 2)
	for rows.Next() {
		var kind Kind
		var config Configuration
		var sealed []byte
		if err := rows.Scan(&kind, &config.Endpoint, &config.Label, &sealed, &config.Instructions); err != nil {
			return nil, err
		}
		config.Secret, err = s.openSecret(kind, sealed)
		if err != nil {
			return nil, err
		}
		items[kind] = config
	}
	return items, rows.Err()
}

func (s *Service) openSecret(kind Kind, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("invalid encrypted settings credential")
	}
	secret, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte(kind))
	if err != nil {
		return "", fmt.Errorf("open %s credential: %w", kind, err)
	}
	return string(secret), nil
}

func (s *Service) Save(ctx context.Context, userID int64, kind Kind, input Update) error {
	input.Endpoint = strings.TrimSpace(input.Endpoint)
	input.Label = strings.TrimSpace(input.Label)
	if input.Instructions != nil {
		instructions := strings.TrimSpace(*input.Instructions)
		input.Instructions = &instructions
	}
	destination, err := validate(kind, input)
	if err != nil {
		return err
	}

	var checkedSecret []byte
	if kind == WebDAV && destination != nil {
		checkedSecret, err = s.prepareWebDAV(ctx, input, destination)
		if err != nil {
			return err
		}
	}
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		if destination == nil && kind == LLM {
			// A prompt remains editable even when the model endpoint is disabled.
			if err := writeConfig(ctx, tx, kind, "", "", nil, input.Instructions); err != nil {
				return err
			}
		} else if destination == nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM integration_settings WHERE kind=?", kind); err != nil {
				return err
			}
		} else if err := s.saveConfig(ctx, tx, kind, input, destination, checkedSecret); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO audit_logs(user_id, action, detail_json, created_at)
			VALUES (?, 'settings.update', json_object('kind', ?), ?)
		`, userID, kind, time.Now().UnixMilli())
		return err
	})
}

func (s *Service) prepareWebDAV(ctx context.Context, input Update, destination *url.URL) ([]byte, error) {
	var sealed []byte
	var password string
	if input.Secret != nil {
		password = *input.Secret
	} else {
		var err error
		sealed, err = savedSecret(ctx, s.store, WebDAV, destination)
		if err != nil {
			return nil, err
		}
		password, err = s.openSecret(WebDAV, sealed)
		if err != nil {
			return nil, err
		}
	}
	if password != "" && input.Label == "" {
		return nil, ValidationError("使用 WebDAV 密码时，请填写用户名。")
	}
	dav := storage.WebDAV{Endpoint: destination.String(), Username: input.Label, Password: password}
	if err := dav.EnsureCollection(ctx); err != nil {
		return nil, WebDAVSetupError{err: err}
	}
	return sealed, nil
}

func (s *Service) SealWebDAV(id string, config Configuration) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	data, err := json.Marshal([3]string{config.Endpoint, config.Label, config.Secret})
	if err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, data, []byte("webdav-attachment:"+id)), nil
}

func (s *Service) OpenWebDAV(id string, sealed []byte) (Configuration, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return Configuration{}, errors.New("invalid WebDAV storage reference")
	}
	data, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte("webdav-attachment:"+id))
	if err != nil {
		return Configuration{}, err
	}
	var values [3]string
	if err := json.Unmarshal(data, &values); err != nil {
		return Configuration{}, err
	}
	return Configuration{Endpoint: values[0], Label: values[1], Secret: values[2]}, nil
}

type credentialQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func savedSecret(ctx context.Context, q credentialQuery, kind Kind, destination *url.URL) ([]byte, error) {
	var previous string
	var sealed []byte
	err := q.QueryRowContext(ctx, "SELECT endpoint, secret FROM integration_settings WHERE kind=?", kind).
		Scan(&previous, &sealed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if len(sealed) > 0 {
		saved, err := url.Parse(previous)
		if err != nil || origin(saved) != origin(destination) {
			return nil, ValidationError("服务地址的协议、主机或端口变更时，请重新填写或清除凭证。")
		}
	}
	return sealed, nil
}

func (s *Service) saveConfig(ctx context.Context, tx *sql.Tx, kind Kind, input Update, destination *url.URL, checkedSecret []byte) error {
	var sealed []byte
	switch {
	case input.Secret == nil:
		var err error
		sealed, err = savedSecret(ctx, tx, kind, destination)
		if err != nil {
			return err
		}
		if kind == WebDAV && !bytes.Equal(sealed, checkedSecret) {
			return ValidationError("WebDAV 凭证已更新，请刷新设置后重试。")
		}
	case *input.Secret != "":
		nonce := make([]byte, s.aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		sealed = s.aead.Seal(nonce, nonce, []byte(*input.Secret), []byte(kind))
	}
	if kind == WebDAV && input.Label == "" && len(sealed) != 0 {
		return ValidationError("使用 WebDAV 密码时，请填写用户名。")
	}
	var credential any
	if len(sealed) != 0 {
		credential = sealed
	}
	return writeConfig(ctx, tx, kind, destination.String(), input.Label, credential, input.Instructions)
}

func writeConfig(ctx context.Context, tx *sql.Tx, kind Kind, endpoint, label string, credential any, instructions *string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO integration_settings(kind, endpoint, label, secret, updated_at, instructions)
		VALUES (?, ?, ?, ?, ?, COALESCE(?, ''))
		ON CONFLICT(kind) DO UPDATE SET endpoint=excluded.endpoint, label=excluded.label,
		    secret=excluded.secret, updated_at=excluded.updated_at, instructions=COALESCE(?, integration_settings.instructions)
	`, kind, endpoint, label, credential, time.Now().UnixMilli(), instructions, instructions)
	return err
}

func validate(kind Kind, input Update) (*url.URL, error) {
	if input.Instructions != nil && (utf8.RuneCountInString(*input.Instructions) > 16000 || strings.ContainsRune(*input.Instructions, '\x00')) {
		return nil, ValidationError("系统提示词最多 16000 个字符，且不能包含空字节。")
	}
	if len(input.Label) > 256 || strings.ContainsAny(input.Label, "\r\n\x00") {
		return nil, ValidationError("用户名或模型名格式无效。")
	}
	if kind == WebDAV && strings.Contains(input.Label, ":") {
		return nil, ValidationError("WebDAV 用户名不能包含冒号。")
	}
	if input.Secret != nil && (len(*input.Secret) > 4096 || strings.ContainsAny(*input.Secret, "\r\n\x00")) {
		return nil, ValidationError("凭证格式无效或过长。")
	}
	if input.Endpoint == "" {
		if input.Label != "" || (input.Secret != nil && *input.Secret != "") {
			return nil, ValidationError("请填写服务地址。")
		}
		return nil, nil
	}
	if kind == LLM && input.Label == "" {
		return nil, ValidationError("请选择模型。")
	}
	u, err := url.Parse(input.Endpoint)
	if len(input.Endpoint) > 2048 || err != nil || u.Hostname() == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.ForceQuery || u.RawQuery != "" || strings.Contains(input.Endpoint, "#") {
		return nil, ValidationError("请输入完整的 HTTP 或 HTTPS 地址，凭证填写在单独的字段中。")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number <= 0 || number > 65535 {
			return nil, ValidationError("服务端口无效。")
		}
	}
	return u, nil
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "|" + strings.ToLower(u.Hostname()) + "|" + port
}
