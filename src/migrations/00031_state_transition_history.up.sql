-- P6-21 (WBS 6.28/6.29): append-only state-transition history plus the
-- per-project state-entry sequence source.
-- 1. The per-project sequence counter lives on the project row (persisted,
--    survives restart; WBS 6.29 T6.29.4). Allocated under a project-scoped
--    advisory lock with a unique (project_id, seq) belt-and-braces.
ALTER TABLE projects
	ADD COLUMN state_entry_seq INTEGER NOT NULL DEFAULT 0 CHECK (state_entry_seq >= 0);
-- 2. Append-only history: no update/delete path at the service layer
--    (WBS 6.28). seq is assigned by the append (0 = "allocate now").
CREATE TABLE IF NOT EXISTS state_transition_history (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	state_machine_id UUID NOT NULL REFERENCES state_machines (id),
	state_machine_version INTEGER NOT NULL CHECK (state_machine_version > 0),
	from_state_id UUID NOT NULL REFERENCES workflow_states (id),
	to_state_id UUID NOT NULL REFERENCES workflow_states (id),
	outcome TEXT NOT NULL,
	phase_id UUID NULL REFERENCES phases (id) ON DELETE SET NULL,
	work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE SET NULL,
	runtime_version INTEGER NOT NULL CHECK (runtime_version > 0),
	seq INTEGER NOT NULL CHECK (seq > 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT state_transition_history_project_seq_uq UNIQUE (project_id, seq)
);
