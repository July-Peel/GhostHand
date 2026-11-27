ALTER TABLE confirmations ADD COLUMN target_device_id TEXT;
CREATE INDEX confirmations_pending_target ON confirmations(target_device_id, task_id) WHERE decision='pending';
