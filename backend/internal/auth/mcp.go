package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"ghosthand/proto"
)

func (s *Service) MCPTokenStatus(ctx context.Context, user int64) (int64, error) {
	var created int64
	err := s.store.QueryRowContext(ctx, "SELECT created_at FROM mcp_credentials WHERE user_id=?", user).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return created, err
}

func (s *Service) IssueMCPToken(ctx context.Context, user int64) (string, int64, error) {
	token, err := proto.NewToken()
	if err != nil {
		return "", 0, err
	}
	hash, err := proto.TokenHash(token)
	if err != nil {
		return "", 0, err
	}
	now := time.Now().UnixMilli()
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_credentials(user_id,token_hash,created_at) VALUES (?,?,?)
			ON CONFLICT(user_id) DO UPDATE SET token_hash=excluded.token_hash,created_at=excluded.created_at`, user, hash, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO audit_logs(user_id,action,created_at) VALUES (?,'mcp.token.issue',?)", user, now)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	return token, now, nil
}

func (s *Service) RevokeMCPToken(ctx context.Context, user int64) error {
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM mcp_credentials WHERE user_id=?", user); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO audit_logs(user_id,action,created_at) VALUES (?,'mcp.token.revoke',?)", user, time.Now().UnixMilli())
		return err
	})
}

func (s *Service) MCPUser(ctx context.Context, token string) (int64, error) {
	hash, err := proto.TokenHash(token)
	if err != nil {
		return 0, ErrSession
	}
	var user int64
	err = s.store.QueryRowContext(ctx, "SELECT user_id FROM mcp_credentials WHERE token_hash=?", hash).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSession
	}
	return user, err
}
