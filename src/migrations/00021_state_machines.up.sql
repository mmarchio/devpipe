-- P6-05 (WBS 6.1): state-machine definitions. A project may retain
-- historical machine versions; activation is a later concern (P6-17+).
CREATE TABLE IF NOT EXISTS state_machines (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
	enabled BOOLEAN NOT NULL DEFAULT true,
	initial_state_id UUID NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- initial_state_id's FK is added in 00022, once workflow_states exists.
