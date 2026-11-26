package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"ghosthand/proto"
)

const (
	maxMemLogEntries = 2000
	maxMemLogBytes   = 2 << 20
)

type memLogEntry struct {
	time time.Time
	line []byte
}

type memLogBuffer struct {
	mu      sync.Mutex
	entries []memLogEntry
	total   int
}

func (b *memLogBuffer) append(t time.Time, raw []byte) {
	cp := append([]byte(nil), bytes.TrimRight(raw, "\r\n")...)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, memLogEntry{time: t, line: cp})
	b.total += len(cp)
	for len(b.entries) > maxMemLogEntries || (b.total > maxMemLogBytes && len(b.entries) > 1) {
		b.total -= len(b.entries[0].line)
		b.entries = b.entries[1:]
	}
}

func (b *memLogBuffer) snapshot(since time.Time) [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]byte, 0, len(b.entries))
	for _, entry := range b.entries {
		if !entry.time.Before(since) {
			out = append(out, entry.line)
		}
	}
	return out
}

type memLogWriter struct {
	buf    *memLogBuffer
	stderr io.Writer
}

func (w *memLogWriter) Write(p []byte) (int, error) {
	w.buf.append(time.Now(), p)
	if w.stderr != nil {
		_, _ = w.stderr.Write(p)
	}
	return len(p), nil
}

func initMemoryLogger(service bool) *memLogBuffer {
	buf := &memLogBuffer{}
	var stderr io.Writer
	if !service {
		stderr = os.Stderr
	}
	writer := &memLogWriter{buf: buf, stderr: stderr}
	slog.SetDefault(slog.New(slog.NewJSONHandler(writer, nil)))
	return buf
}

func (a *agent) collectThenUpload(ctx context.Context, op proto.Operation) error {
	archive, err := a.collectLogs(op.Hours)
	if err != nil {
		return err
	}
	return retry(ctx, func() error {
		return a.uploadLogs(ctx, op.ID, archive)
	})
}

func (a *agent) collectLogs(hours int) ([]byte, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	lines := a.logs.snapshot(since)
	var out bytes.Buffer
	archive := gzip.NewWriter(&out)
	for _, line := range lines {
		if _, err := archive.Write(line); err != nil {
			return nil, err
		}
		if _, err := archive.Write([]byte{'\n'}); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (a *agent) uploadLogs(ctx context.Context, id string, archive []byte) error {
	cfg := *a.cfg.Load()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	path := "/api/agent/logs/" + id
	var ticket proto.LogTicket
	if err := postJSON(ctx, a.client, cfg, path+"/ticket", struct{}{}, &ticket, true); err != nil {
		return err
	}
	if ticket.Token == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Server+path+"/upload", bytes.NewReader(archive))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ticket.Token)
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return statusError(resp.StatusCode)
	}
	return nil
}
