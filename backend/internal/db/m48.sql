ALTER TABLE devices ADD COLUMN name_customized INTEGER NOT NULL DEFAULT 0 CHECK (name_customized IN (0, 1));

CREATE TABLE device_rdp_credentials (
    device_id TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    username TEXT NOT NULL,
    domain TEXT NOT NULL DEFAULT '',
    password_sealed BLOB NOT NULL,
    rdp_port INTEGER NOT NULL DEFAULT 3389 CHECK (rdp_port BETWEEN 1 AND 65535),
    auto_unlock INTEGER NOT NULL DEFAULT 1 CHECK (auto_unlock IN (0, 1)),
    auto_relock INTEGER NOT NULL DEFAULT 1 CHECK (auto_relock IN (0, 1)),
    keep_system_awake INTEGER NOT NULL DEFAULT 0 CHECK (keep_system_awake IN (0, 1)),
    auth_fail_count INTEGER NOT NULL DEFAULT 0,
    suspended INTEGER NOT NULL DEFAULT 0 CHECK (suspended IN (0, 1)),
    last_result TEXT NOT NULL DEFAULT '',
    last_unlock_at INTEGER NOT NULL DEFAULT 0,
    relock_until INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);
