-- P5-10 (WBS 5.11): role allowed-state definitions — which state keys a role
-- may be assigned to. Phase 6 will define/execute the states; Phase 5 stores
-- the associations only.
CREATE TABLE IF NOT EXISTS role_allowed_states (
	id UUID PRIMARY KEY,
	role_id UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
	state_key TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (role_id, state_key)
);
