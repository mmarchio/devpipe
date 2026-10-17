-- P5-11 (WBS 5.12): role state-file requirements — the types of state files
-- a role generally requires. Identifiers/configuration only in this phase;
-- materialization occurs later. Ordered, deduped, removal isolated.
CREATE TABLE IF NOT EXISTS role_state_files (
	id UUID PRIMARY KEY,
	role_id UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
	state_file_key TEXT NOT NULL,
	position INT NOT NULL DEFAULT 1,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (role_id, state_file_key),
	UNIQUE (role_id, position)
);
