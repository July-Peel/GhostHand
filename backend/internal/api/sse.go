package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"ghosthand/backend/internal/auth"
)

func (s *Server) events(w http.ResponseWriter, r *http.Request, session auth.Session, token string) {
	ctx, cancel := context.WithDeadline(r.Context(), session.ExpiresAt)
	defer cancel()
	oldest, latest, err := s.workbench.EventWindow(ctx)
	if err != nil {
		internalError(w, r, err)
		return
	}
	lastRefresh := s.workbench.RefreshSeq()
	header := r.Header.Get("Last-Event-ID")
	last, err := strconv.ParseInt(header, 10, 64)
	refresh := err != nil || last < 0 || last > latest || last < oldest-1

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func(message string) error {
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		if _, err := fmt.Fprint(w, message); err != nil {
			return err
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		return controller.SetWriteDeadline(time.Time{})
	}
	if refresh {
		last = latest
		if err := send(fmt.Sprintf("retry: 3000\nid: %d\nevent: refresh\ndata: {}\n\n", last)); err != nil {
			return
		}
	} else if err := send("retry: 3000\n: connected\n\n"); err != nil {
		return
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for ctx.Err() == nil {
		changed := s.workbench.Changed()
		if _, err := s.auth.Session(ctx, token); err != nil {
			return
		}
		oldest, latest, err := s.workbench.EventWindow(ctx)
		if err != nil {
			return
		}
		if currentRefresh := s.workbench.RefreshSeq(); currentRefresh != lastRefresh {
			lastRefresh = currentRefresh
			last = latest
			if err := send(fmt.Sprintf("id: %d\nevent: refresh\ndata: {}\n\n", last)); err != nil {
				return
			}
			continue
		}
		if last < oldest-1 || last > latest {
			last = latest
			if err := send(fmt.Sprintf("id: %d\nevent: refresh\ndata: {}\n\n", last)); err != nil {
				return
			}
		}
		events, err := s.workbench.EventsAfter(ctx, last)
		if err != nil {
			return
		}
		for _, event := range events {
			payload, err := json.Marshal(event)
			if err != nil || send(fmt.Sprintf("id: %d\nevent: device.changed\ndata: %s\n\n", event.ID, payload)) != nil {
				return
			}
			last = event.ID
		}
		if len(events) == 100 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-keepalive.C:
			if _, err := s.auth.Session(ctx, token); err != nil || send(": heartbeat\n\n") != nil {
				return
			}
		}
	}
}
