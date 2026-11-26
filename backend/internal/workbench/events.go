package workbench

import (
	"context"
	"database/sql"
)

const eventRetention = 2000

type Event struct {
	ID       int64  `json:"-"`
	DeviceID string `json:"device_id"`
}

func RecordChange(ctx context.Context, tx *sql.Tx, deviceID string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO browser_events(device_id) VALUES (?)", deviceID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM browser_events
		WHERE id <= (SELECT MAX(id) FROM browser_events)-?`, eventRetention)
	return err
}

func (s *Service) EventWindow(ctx context.Context) (oldest, latest int64, err error) {
	err = s.store.QueryRowContext(ctx, "SELECT COALESCE(MIN(id), 0), COALESCE(MAX(id), 0) FROM browser_events").
		Scan(&oldest, &latest)
	return oldest, latest, err
}

func (s *Service) EventsAfter(ctx context.Context, id int64) ([]Event, error) {
	rows, err := s.store.QueryContext(ctx, `
		SELECT e.id, e.device_id FROM browser_events e JOIN devices d ON d.id=e.device_id
		WHERE e.id>? AND (d.is_demo=0 OR ?) ORDER BY e.id LIMIT 100
	`, id, s.demo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.DeviceID); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Service) Changed() <-chan struct{} {
	return s.store.Changed()
}
