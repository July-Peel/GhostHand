ALTER TABLE tasks ADD COLUMN source TEXT NOT NULL DEFAULT 'quick';
ALTER TABLE tasks ADD COLUMN prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN last_heartbeat_at INTEGER;
ALTER TABLE tasks ADD COLUMN stalled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN step_timeout_ms INTEGER NOT NULL DEFAULT 30000;
ALTER TABLE tasks ADD COLUMN brain_started_at INTEGER;
UPDATE tasks SET last_heartbeat_at=started_at WHERE started_at IS NOT NULL;
CREATE INDEX tasks_heartbeat ON tasks(device_id,last_heartbeat_at) WHERE stalled=0 AND state IN ('running','paused','waiting_user','cancelling');
CREATE INDEX task_events_retention ON task_events(created_at);

ALTER TABLE devices ADD COLUMN mcp_tools TEXT NOT NULL DEFAULT '[]';
ALTER TABLE devices ADD COLUMN mcp_resources TEXT NOT NULL DEFAULT '[]';

DELETE FROM device_inbox
WHERE seq <= (SELECT agent_ack-200 FROM devices WHERE id=device_inbox.device_id);

ALTER TABLE screenshot_tickets RENAME TO screenshot_tickets_v3;
CREATE TABLE screenshot_tickets (
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    upload_id TEXT NOT NULL DEFAULT '',
    token_hash TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    PRIMARY KEY(task_id,upload_id)
);
INSERT INTO screenshot_tickets(task_id,token_hash,expires_at)
SELECT task_id,token_hash,expires_at FROM screenshot_tickets_v3;
DROP TABLE screenshot_tickets_v3;

ALTER TABLE attachments RENAME TO attachments_v3;
CREATE TABLE attachments (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    upload_id TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    mime TEXT NOT NULL,
    size INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    storage_backend TEXT NOT NULL,
    storage_key TEXT NOT NULL,
    thumb_key TEXT NOT NULL,
    storage_ref BLOB,
    UNIQUE(task_id,upload_id)
);
INSERT INTO attachments(id,task_id,name,mime,size,sha256,created_at,storage_backend,storage_key,thumb_key)
SELECT id,task_id,name,mime,size,sha256,created_at,'local',id||'.jpg',id||'.thumb.jpg' FROM attachments_v3;
DROP TABLE attachments_v3;
CREATE INDEX attachments_retention ON attachments(created_at);

CREATE TABLE brain_calls (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    tool TEXT NOT NULL,
    result_json TEXT,
    created_at INTEGER NOT NULL
);
CREATE INDEX brain_calls_task ON brain_calls(task_id);
CREATE TABLE confirmations (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    summary TEXT NOT NULL,
    decision TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    decided_at INTEGER
);
