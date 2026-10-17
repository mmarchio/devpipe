-- P6-20 (WBS 6.23): runtime state. One row per project records the
-- current logical workflow position: the bound machine family + version,
-- the current state, and optional phase / work-unit associations.
-- version protects the row under optimistic concurrency (WBS 6.24).
CREATE TABLE IF NOT EXISTS runtime_state (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL UNIQUE REFERENCES projects (id) ON DELETE CASCADE,
	state_machine_id UUID NOT NULL REFERENCES state_machines (id),
	state_machine_version INTEGER NOT NULL CHECK (state_machine_version > 0),
	current_state_id UUID NOT NULL REFERENCES workflow_states (id),
	phase_id UUID NULL REFERENCES phases (id) ON DELETE SET NULL,
	work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE SET NULL,
	entered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
