-- P5-08 (WBS 5.9): ordered role → instruction assignments. Position is
-- deterministic (explicit, server-assigned on insert = MAX(position)+1).
-- Removing an assignment is a row delete — the instruction row is retained.
CREATE TABLE IF NOT EXISTS role_instruction_assignments (
	id UUID PRIMARY KEY,
	role_id UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
	instruction_id UUID NOT NULL REFERENCES instructions (id) ON DELETE CASCADE,
	position INTEGER NOT NULL CHECK (position > 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (role_id, instruction_id),
	UNIQUE (role_id, position)
);
