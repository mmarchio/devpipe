-- P6-09 (WBS 6.6): per-state required state-file categories — the state-file
-- types a state requires, supplementing the role-level requirements
-- (role_state_files, WBS 5.12). Phase 6 stores requirements only; union-dedup
-- resolution at prompt composition is a later concern.
CREATE TABLE IF NOT EXISTS state_file_requirements (
	id UUID PRIMARY KEY,
	state_id UUID NOT NULL REFERENCES workflow_states (id) ON DELETE CASCADE,
	category TEXT NOT NULL,
	ordinal INTEGER NOT NULL DEFAULT 0 CHECK (ordinal >= 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (state_id, category)
);
