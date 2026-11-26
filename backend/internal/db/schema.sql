CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_login_at INTEGER
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL,
    ip TEXT NOT NULL,
    ua TEXT NOT NULL
);

CREATE INDEX sessions_expiry ON sessions(expires_at);

CREATE TABLE devices (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    platform TEXT NOT NULL,
    arch TEXT NOT NULL,
    os_version TEXT NOT NULL,
    group_name TEXT NOT NULL,
    conn_state TEXT NOT NULL,
    helper_state TEXT NOT NULL,
    desktop_state TEXT NOT NULL,
    battery_pct INTEGER CHECK (battery_pct BETWEEN 0 AND 100),
    charging INTEGER CHECK (charging IN (0, 1)),
    last_seen_at INTEGER NOT NULL,
    is_demo INTEGER NOT NULL DEFAULT 0 CHECK (is_demo IN (0, 1))
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    created_by INTEGER NOT NULL REFERENCES users(id),
    capability TEXT NOT NULL,
    state TEXT NOT NULL,
    step_index INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER,
    next_at INTEGER
);

CREATE INDEX tasks_due ON tasks(next_at) WHERE next_at IS NOT NULL;
CREATE INDEX tasks_device_state ON tasks(device_id, state);

CREATE TABLE chat_messages (
    id INTEGER PRIMARY KEY,
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    sender TEXT NOT NULL,
    type TEXT NOT NULL,
    text TEXT NOT NULL DEFAULT '',
    task_id TEXT REFERENCES tasks(id) ON DELETE CASCADE,
    payload_json TEXT,
    created_at INTEGER NOT NULL
);

CREATE INDEX messages_device_id ON chat_messages(device_id, id);
CREATE UNIQUE INDEX messages_task_card ON chat_messages(task_id) WHERE type = 'task_card';

CREATE TABLE task_events (
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    step_key TEXT NOT NULL,
    status TEXT NOT NULL,
    detail TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, seq)
);

CREATE TABLE browser_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE
);

CREATE TABLE audit_logs (
    id INTEGER PRIMARY KEY,
    user_id INTEGER REFERENCES users(id),
    device_id TEXT REFERENCES devices(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);
