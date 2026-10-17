-- P6-08 (WBS 6.5): per-state instruction marker. One row per state carries
-- the current-version pointer (0 until the first version is appended). The
-- state instruction is a distinct entity from the role system prompt.
CREATE TABLE IF NOT EXISTS state_instructions (
	id UUID PRIMARY KEY,
	state_id UUID NOT NULL UNIQUE REFERENCES workflow_states (id) ON DELETE CASCADE,
	current_version INTEGER NOT NULL DEFAULT 0 CHECK (current_version >= 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
