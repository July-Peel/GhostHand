ALTER TABLE devices ADD COLUMN agent_ver TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN caps_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN caps_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE devices ADD COLUMN conn_gen INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN agent_ack INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN controller_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN controller_ack INTEGER NOT NULL DEFAULT 0;

CREATE TABLE device_credentials (
    device_id TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    pending_hash TEXT,
    rotated_at INTEGER NOT NULL,
    revoked_at INTEGER
);

CREATE TABLE pairing_codes (
    code TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL,
    used_at INTEGER,
    device_id TEXT REFERENCES devices(id),
    enrollment_hash TEXT
);

CREATE TABLE device_sessions (
    id INTEGER PRIMARY KEY,
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    conn_gen INTEGER NOT NULL,
    connected_at INTEGER NOT NULL,
    disconnected_at INTEGER,
    reason TEXT,
    remote_ip TEXT NOT NULL,
    agent_ver TEXT NOT NULL,
    UNIQUE(device_id, conn_gen)
);
CREATE INDEX device_sessions_retention ON device_sessions(disconnected_at);

CREATE TABLE outbox (
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    envelope_json TEXT NOT NULL,
    PRIMARY KEY(device_id, seq)
);

CREATE TABLE device_inbox (
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    PRIMARY KEY(device_id, id),
    UNIQUE(device_id, seq)
);

CREATE TABLE agent_releases (
    id TEXT PRIMARY KEY,
    version TEXT NOT NULL,
    platform TEXT NOT NULL,
    arch TEXT NOT NULL,
    component TEXT NOT NULL,
    size INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    signature TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(platform, arch, component, version)
);

CREATE TABLE device_operations (
    id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    detail TEXT NOT NULL DEFAULT '',
    release_id TEXT REFERENCES agent_releases(id),
    created_at INTEGER NOT NULL,
    finished_at INTEGER,
    uploaded_at INTEGER,
    ticket_hash TEXT,
    ticket_expiry INTEGER
);
CREATE INDEX device_operations_recent ON device_operations(device_id, created_at DESC);
