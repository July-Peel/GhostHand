package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ghosthand/backend/internal/workbench"
)

func (s *Service) RenameDevice(ctx context.Context, user int64, device, raw string, includeDemo bool) (string, error) {
	if device == workbench.FreeChatDeviceID {
		return "", ErrMissing
	}
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > 80 ||
		strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) }) {
		return "", ErrInvalid
	}
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var old string
		err := tx.QueryRowContext(ctx, "SELECT name FROM devices WHERE id=? AND (is_demo=0 OR ?)", device, includeDemo).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET name=?,name_customized=1 WHERE id=?", name, device); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]string{"old_name": old, "new_name": name})
		if err := audit(ctx, tx, user, device, "device.rename", string(detail), time.Now().UnixMilli()); err != nil {
			return err
		}
		return workbench.RecordChange(ctx, tx, device)
	})
	return name, err
}
