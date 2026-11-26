ALTER TABLE devices ADD COLUMN task_protocol INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN lane TEXT NOT NULL DEFAULT 'bg';
ALTER TABLE tasks ADD COLUMN step_total INTEGER NOT NULL DEFAULT 2;
ALTER TABLE tasks ADD COLUMN step_name TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN elapsed_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN control TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN cancel_at INTEGER;
ALTER TABLE tasks ADD COLUMN assign_seq INTEGER;
ALTER TABLE task_events ADD COLUMN step_name TEXT NOT NULL DEFAULT '';
ALTER TABLE task_events ADD COLUMN elapsed_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE task_events ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0;

CREATE INDEX tasks_device_lane ON tasks(device_id, lane, state);
CREATE INDEX tasks_expiry ON tasks(expires_at) WHERE state IN ('queued', 'dispatched');

CREATE TABLE screenshot_tickets (
    task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE TABLE attachments (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    mime TEXT NOT NULL,
    size INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
