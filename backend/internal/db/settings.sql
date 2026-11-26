CREATE TABLE integration_settings (
    kind TEXT PRIMARY KEY CHECK (kind IN ('webdav', 'llm')),
    endpoint TEXT NOT NULL,
    label TEXT NOT NULL,
    secret BLOB,
    updated_at INTEGER NOT NULL
);
