CREATE TABLE IF NOT EXISTS device_ssh_credentials (
    device_id          TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    host               TEXT NOT NULL,
    port               INTEGER NOT NULL DEFAULT 22 CHECK (port BETWEEN 1 AND 65535),
    username           TEXT NOT NULL,
    auth_kind          TEXT NOT NULL DEFAULT 'password' CHECK (auth_kind IN ('password', 'private_key')),
    secret_ciphertext  BLOB NOT NULL,
    host_key_fp        TEXT NOT NULL DEFAULT '',
    host_agent_id      TEXT REFERENCES devices(id) ON DELETE SET NULL,
    enabled            INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    last_error         TEXT NOT NULL DEFAULT '',
    updated_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_device_ssh_host_agent ON device_ssh_credentials(host_agent_id);
