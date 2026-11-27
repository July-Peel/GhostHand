package workbench

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func (s *Service) RunMaintenance(ctx context.Context) {
	for ctx.Err() == nil {
		now := time.Now().UTC()
		if err := s.backup(ctx, now); err != nil && ctx.Err() == nil {
			slog.Error("database backup", "error", err)
		}
		if err := s.prune(ctx, now.AddDate(0, 0, -30).UnixMilli()); err != nil && ctx.Err() == nil {
			slog.Error("data retention", "error", err)
		}
		due := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		timer := time.NewTimer(time.Until(due))
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (s *Service) backup(ctx context.Context, now time.Time) error {
	dir := filepath.Join(s.dataDir, "backup")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	final := filepath.Join(dir, "ghosthand-"+now.Format("20060102")+".db")
	if _, err := os.Stat(final); errors.Is(err, os.ErrNotExist) {
		staging, err := os.MkdirTemp(dir, ".backup-*")
		if err != nil {
			return err
		}
		defer os.Remove(staging)
		temp := filepath.Join(staging, "snapshot.db")
		defer os.Remove(temp)
		if err := s.store.Backup(ctx, temp); err != nil {
			return err
		}
		if err := os.Chmod(temp, 0600); err != nil {
			return err
		}
		if err := os.Rename(temp, final); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "ghosthand-????????.db"))
	if err != nil {
		return err
	}
	files = slices.DeleteFunc(files, func(file string) bool {
		date := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "ghosthand-"), ".db")
		_, err := time.Parse("20060102", date)
		return err != nil
	})
	slices.Sort(files)
	for _, file := range files[:max(0, len(files)-7)] {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type staleAttachment struct {
	AttachmentFiles
	device, task string
}

func (s *Service) prune(ctx context.Context, before int64) error {
	if err := s.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_events WHERE created_at<?
			AND task_id IN (SELECT id FROM tasks WHERE finished_at IS NOT NULL)`, before); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM screenshot_tickets WHERE expires_at<=?", time.Now().UnixMilli())
		return err
	}); err != nil {
		return err
	}
	var cursor string
	var cleanupErr error
	for ctx.Err() == nil {
		rows, err := s.store.QueryContext(ctx, `SELECT a.id,t.device_id,t.id,a.storage_backend,a.storage_key,a.thumb_key,a.storage_ref
			FROM attachments a JOIN tasks t ON t.id=a.task_id WHERE a.created_at<? AND t.finished_at IS NOT NULL AND a.id>?
			ORDER BY a.id LIMIT 50`, before, cursor)
		if err != nil {
			return errors.Join(cleanupErr, err)
		}
		var batch []staleAttachment
		for rows.Next() {
			var a staleAttachment
			if err := rows.Scan(&a.ID, &a.device, &a.task, &a.Kind, &a.Key, &a.Thumb, &a.Ref); err != nil {
				rows.Close()
				return errors.Join(cleanupErr, err)
			}
			batch = append(batch, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return errors.Join(cleanupErr, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, a := range batch {
			cursor = a.ID
			if err := s.pruneAttachment(ctx, a); err != nil {
				cleanupErr = err
			}
		}
	}
	return cleanupErr
}

func (s *Service) pruneAttachment(ctx context.Context, a staleAttachment) error {
	if err := s.removeAttachmentFiles(ctx, a.AttachmentFiles); err != nil {
		return err
	}
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM attachments WHERE id=?", a.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE device_id=? AND type='media'
			AND json_extract(payload_json,'$.id')=?`, a.device, a.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `WITH matches AS (
			SELECT m.id,j.key FROM chat_messages m, json_each(m.payload_json,'$.attachments') j
			WHERE m.device_id=? AND m.task_id=? AND m.sender='user' AND m.type='text' AND json_extract(j.value,'$.id')=?
		) UPDATE chat_messages SET payload_json=json_remove(payload_json,'$.attachments['||matches.key||']')
		  FROM matches WHERE chat_messages.id=matches.id`, a.device, a.task, a.ID); err != nil {
			return err
		}
		return RecordChange(ctx, tx, a.device)
	})
}
