package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ghosthand/backend/internal/db"
	"golang.org/x/crypto/argon2"
)

const (
	SessionTTL = 24 * time.Hour
	hashPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"
)

var (
	ErrCredentials    = errors.New("invalid username or password")
	ErrSession        = errors.New("session is missing or expired")
	ErrRateLimit      = errors.New("login rate limit exceeded")
	ErrPasswordPolicy = errors.New("invalid new password")
	ErrSamePassword   = errors.New("new password matches current password")
)

type Session struct {
	UserID    int64
	Username  string
	ExpiresAt time.Time
}

type attempts struct {
	until time.Time
	count int
}

type Service struct {
	store    *db.Store
	hashSlot chan struct{}
	mu       sync.Mutex
	attempts map[string]attempts
	pruneAt  time.Time
}

func New(store *db.Store) *Service {
	return &Service{store: store, hashSlot: make(chan struct{}, 1), attempts: make(map[string]attempts)}
}

// Bootstrap creates the initial administrator once. An empty password can be
// generated for a local deployment and returned to the caller for first login.
func (s *Service) Bootstrap(ctx context.Context, username, password string, local bool) (string, error) {
	var exists bool
	if err := s.store.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM users)").Scan(&exists); err != nil || exists {
		return "", err
	}
	if username == "" || utf8.RuneCountInString(username) > 64 {
		return "", errors.New("GH_ADMIN_USER must be 1–64 characters")
	}
	var generated string
	if password == "" && local {
		var err error
		generated, err = randomToken(24)
		if err != nil {
			return "", err
		}
		password = generated
	}
	if !validPassword(password) {
		return "", errors.New("GH_ADMIN_PASSWORD must be at least 12 characters and at most 256 bytes")
	}
	stored, err := encodePassword(password)
	if err != nil {
		return "", err
	}
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users(username, password_hash, created_at)
			SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)
		`, username, stored, time.Now().UnixMilli())
		return err
	})
	return generated, err
}

func (s *Service) Login(ctx context.Context, username, password, ip, agent string) (Session, string, error) {
	if !s.allow(ip, time.Now()) {
		return Session{}, "", ErrRateLimit
	}
	select {
	case s.hashSlot <- struct{}{}:
		defer func() { <-s.hashSlot }()
	default:
		s.releaseAttempt(ip)
		return Session{}, "", ErrRateLimit
	}

	var session Session
	var stored string
	err := s.store.QueryRowContext(ctx, "SELECT id, username, password_hash FROM users WHERE username=?", username).
		Scan(&session.UserID, &session.Username, &stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.releaseAttempt(ip)
		return Session{}, "", err
	}
	if !checkPassword(password, stored) || session.UserID == 0 {
		return Session{}, "", ErrCredentials
	}

	token, err := randomToken(32)
	if err != nil {
		s.releaseAttempt(ip)
		return Session{}, "", err
	}
	now := time.Now()
	session.ExpiresAt = now.Add(SessionTTL)
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", now.UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO sessions(id, user_id, expires_at, ip, ua) VALUES (?, ?, ?, ?, ?)",
			digest(token), session.UserID, session.ExpiresAt.UnixMilli(), ip, agent); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET last_login_at=? WHERE id=?", now.UnixMilli(), session.UserID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO audit_logs(user_id, action, created_at) VALUES (?, 'auth.login', ?)",
			session.UserID, now.UnixMilli())
		return err
	})
	s.releaseAttempt(ip)
	return session, token, err
}

func (s *Service) Session(ctx context.Context, token string) (Session, error) {
	if len(token) != 43 {
		return Session{}, ErrSession
	}
	var session Session
	var expires int64
	err := s.store.QueryRowContext(ctx, `
		SELECT u.id, u.username, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id = ? AND s.expires_at > ?
	`, digest(token), time.Now().UnixMilli()).Scan(&session.UserID, &session.Username, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSession
	}
	session.ExpiresAt = time.UnixMilli(expires)
	return session, err
}

func (s *Service) Logout(ctx context.Context, session Session, token string) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", digest(token))
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err != nil || deleted == 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO audit_logs(user_id, action, created_at) VALUES (?, 'auth.logout', ?)",
			session.UserID, time.Now().UnixMilli())
		return err
	})
}

func CSRF(token string) string {
	return digest("ghosthand-csrf\x00" + token)
}

// ChangePassword revokes every session for the account after committing the new hash.
func (s *Service) ChangePassword(ctx context.Context, session Session, token, current, password string) error {
	if !validPassword(password) {
		return ErrPasswordPolicy
	}
	limitKey := fmt.Sprintf("password:%d", session.UserID)
	if !s.allow(limitKey, time.Now()) {
		return ErrRateLimit
	}
	failed := false
	defer func() {
		if !failed {
			s.releaseAttempt(limitKey)
		}
	}()
	select {
	case s.hashSlot <- struct{}{}:
		defer func() { <-s.hashSlot }()
	default:
		return ErrRateLimit
	}

	var stored string
	err := s.store.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id=?", session.UserID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSession
	}
	if err != nil {
		return err
	}
	if current == "" || len(current) > 256 || !checkPassword(current, stored) {
		failed = true
		return ErrCredentials
	}
	if current == password {
		return ErrSamePassword
	}
	newHash, err := encodePassword(password)
	if err != nil {
		return err
	}
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		updated, err := tx.ExecContext(ctx, `
			UPDATE users SET password_hash=? WHERE id=? AND password_hash=?
			AND EXISTS (SELECT 1 FROM sessions WHERE id=? AND user_id=users.id AND expires_at>?)
		`, newHash, session.UserID, stored, digest(token), now)
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrSession
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=?", session.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM mcp_credentials WHERE user_id=?", session.UserID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO audit_logs(user_id, action, created_at) VALUES (?, 'auth.password_change', ?)
		`, session.UserID, now)
		return err
	})
}

func validPassword(password string) bool {
	return utf8.RuneCountInString(password) >= 12 && len(password) <= 256
}

func encodePassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return hashPrefix + encode(salt) + "$" + encode(passwordHash(password, salt)), nil
}

func (s *Service) allow(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !now.Before(s.pruneAt) {
		for key, entry := range s.attempts {
			if !now.Before(entry.until) {
				delete(s.attempts, key)
			}
		}
		s.pruneAt = now.Add(time.Minute)
	}
	entry := s.attempts[ip]
	if !now.Before(entry.until) {
		entry = attempts{until: now.Add(time.Minute)}
	}
	if entry.count >= 5 {
		return false
	}
	entry.count++
	s.attempts[ip] = entry
	return true
}

func (s *Service) releaseAttempt(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.attempts[ip]
	if entry.count <= 1 {
		delete(s.attempts, ip)
		return
	}
	entry.count--
	s.attempts[ip] = entry
}

func passwordHash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
}

func checkPassword(password, stored string) bool {
	salt, expected := make([]byte, 16), make([]byte, 32)
	var valid bool
	if content, ok := strings.CutPrefix(stored, hashPrefix); ok {
		saltPart, hashPart, split := strings.Cut(content, "$")
		decodedSalt, saltErr := base64.RawStdEncoding.DecodeString(saltPart)
		decodedHash, hashErr := base64.RawStdEncoding.DecodeString(hashPart)
		if split && saltErr == nil && hashErr == nil && len(decodedSalt) == len(salt) && len(decodedHash) == len(expected) {
			salt, expected, valid = decodedSalt, decodedHash, true
		}
	}
	matches := subtle.ConstantTimeCompare(passwordHash(password, salt), expected)
	return valid && matches == 1
}

func randomToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func encode(data []byte) string {
	return base64.RawStdEncoding.EncodeToString(data)
}

func digest(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}
