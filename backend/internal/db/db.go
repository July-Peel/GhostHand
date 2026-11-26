package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

//go:embed settings.sql
var settingsSchema string

//go:embed m2.sql
var m2Schema string

//go:embed m3.sql
var m3Schema string

//go:embed m35.sql
var m35Schema string

//go:embed m45.sql
var m45Schema string

//go:embed m46.sql
var m46Schema string

//go:embed m47.sql
var m47Schema string

//go:embed m48.sql
var m48Schema string

type Mutation func(*sql.Tx) error

type writeRequest struct {
	ctx    context.Context
	mutate Mutation
	result chan error
}

type Store struct {
	*sql.DB
	writer  *sql.DB
	writes  chan writeRequest
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	changed chan struct{}
}

func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(filepath.Join(dir, "ghosthand.db"))
	if err != nil {
		return nil, err
	}
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	uri := url.URL{
		Scheme: "file", Path: path,
		RawQuery: "_pragma=busy_timeout%285000%29&_pragma=foreign_keys%28ON%29",
	}
	writer, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	if err := migrate(ctx, writer); err != nil {
		writer.Close()
		return nil, err
	}

	uri.RawQuery += "&mode=ro"
	readers, err := sql.Open("sqlite", uri.String())
	if err != nil {
		writer.Close()
		return nil, err
	}
	readers.SetMaxOpenConns(4)
	readers.SetMaxIdleConns(1)
	store := &Store{
		DB: readers, writer: writer, writes: make(chan writeRequest),
		stop: make(chan struct{}), done: make(chan struct{}), changed: make(chan struct{}),
	}
	go store.writeLoop()
	return store, nil
}

func migrate(ctx context.Context, writer *sql.DB) error {
	var journalMode string
	if err := writer.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return err
	}
	if journalMode != "wal" {
		return fmt.Errorf("SQLite journal mode: %s", journalMode)
	}

	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > 9 {
		return fmt.Errorf("unsupported SQLite schema version %d", version)
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err := tx.ExecContext(ctx, settingsSchema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version=2"); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err := tx.ExecContext(ctx, m2Schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version=3"); err != nil {
			return err
		}
	}
	if version < 4 {
		if _, err := tx.ExecContext(ctx, m3Schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version=4"); err != nil {
			return err
		}
	}
	if version < 5 {
		if _, err := tx.ExecContext(ctx, m35Schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version=5"); err != nil {
			return err
		}
	}
	if version < 6 {
		if _, err := tx.ExecContext(ctx, m45Schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version=6"); err != nil {
			return err
		}
	}
	if version < 7 {
		if _, err := tx.ExecContext(ctx, m46Schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version=7"); err != nil {
			return err
		}
	}
	if version < 8 {
		if _, err := tx.ExecContext(ctx, m47Schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version=8"); err != nil {
			return err
		}
	}
	if version < 9 {
		if _, err := tx.ExecContext(ctx, m48Schema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version=9"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.writer.ExecContext(ctx, "VACUUM INTO ?", path)
	return err
}

func (s *Store) Close() error {
	close(s.stop)
	<-s.done
	return errors.Join(s.DB.Close(), s.writer.Close())
}

// Write serializes SQLite transactions and broadcasts committed changes.
func (s *Store) Write(ctx context.Context, mutate Mutation) error {
	request := writeRequest{ctx: ctx, mutate: mutate, result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return sql.ErrConnDone
	case s.writes <- request:
	}
	return <-request.result
}

// Changed closes when a write commits. Call before reading to avoid missing an update.
func (s *Store) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Store) writeLoop() {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case request := <-s.writes:
			err := s.apply(request)
			if err == nil {
				s.mu.Lock()
				close(s.changed)
				s.changed = make(chan struct{})
				s.mu.Unlock()
			}
			request.result <- err
		}
	}
}

func (s *Store) apply(request writeRequest) error {
	tx, err := s.writer.BeginTx(request.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := request.mutate(tx); err != nil {
		return err
	}
	return tx.Commit()
}
