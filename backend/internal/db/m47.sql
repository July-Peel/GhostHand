ALTER TABLE devices ADD COLUMN machine_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX devices_machine_id ON devices(machine_id) WHERE machine_id <> '';
