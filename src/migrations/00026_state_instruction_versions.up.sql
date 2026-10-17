-- P6-08 (WBS 6.5): state-instruction content versioning (append-only),
-- mirroring role_system_prompts / instruction_versions. The
-- state_instructions row carries the current-version pointer; historical
-- versions are never overwritten (T6.5.3).
CREATE TABLE IF NOT EXISTS state_instruction_versions (
	id UUID PRIMARY KEY,
	state_id UUID NOT NULL REFERENCES workflow_states (id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (state_id, version)
);
