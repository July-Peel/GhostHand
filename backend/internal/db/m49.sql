ALTER TABLE integration_settings ADD COLUMN instructions TEXT NOT NULL DEFAULT '';

INSERT INTO devices(id, name, platform, arch, os_version, group_name, conn_state,
    helper_state, desktop_state, last_seen_at)
VALUES ('__chat__', '随意闲聊', 'chat', '', '', '', 'offline', 'none', 'headless', 0);

CREATE TABLE chat_attachment_deletions (
    id TEXT PRIMARY KEY,
    storage_backend TEXT NOT NULL,
    storage_key TEXT NOT NULL,
    thumb_key TEXT NOT NULL,
    storage_ref BLOB
);
