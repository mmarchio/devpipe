-- P6-05 (WBS 6.2): workflow states. kind is a closed set (CHECK
-- constraint); key is unique within a machine (UNIQUE (machine_id, key)) -
-- the same key is allowed across machines (T6.2.4).
CREATE TABLE IF NOT EXISTS workflow_states (
	id UUID PRIMARY KEY,
	machine_id UUID NOT NULL REFERENCES state_machines (id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL CHECK (kind IN ('ACTION','DECISION','VALIDATION','CONTROL','TERMINAL')),
	terminal BOOLEAN NOT NULL DEFAULT false,
	enabled BOOLEAN NOT NULL DEFAULT true,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (machine_id, key)
);
-- A machine's initial state must reference an existing state.
ALTER TABLE state_machines
	ADD CONSTRAINT state_machines_initial_state_fk
	FOREIGN KEY (initial_state_id) REFERENCES workflow_states (id) ON DELETE SET NULL;
