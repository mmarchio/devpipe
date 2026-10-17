-- P5-07 (WBS 5.8): instruction content versioning (append-only), mirroring
-- role_system_prompts. The instructions row carries the current pointer.
ALTER TABLE instructions
	ADD COLUMN IF NOT EXISTS current_version INTEGER NOT NULL DEFAULT 1 CHECK (current_version > 0);
CREATE TABLE IF NOT EXISTS instruction_versions (
	id UUID PRIMARY KEY,
	instruction_id UUID NOT NULL REFERENCES instructions (id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (instruction_id, version)
);
