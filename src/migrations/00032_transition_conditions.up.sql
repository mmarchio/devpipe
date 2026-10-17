-- P6-27 (WBS 6.37): persist a transition's structured condition (T6.37.2).
-- One condition per transition (00023 has no condition surface; this is the
-- new persistence). The condition is the P6-04 domain shape:
-- (field, operator, value) with the value being exactly one of a literal
-- or a context-field reference.
CREATE TABLE IF NOT EXISTS transition_conditions (
	transition_id UUID PRIMARY KEY REFERENCES state_transitions (id) ON DELETE CASCADE,
	field TEXT NOT NULL,
	operator TEXT NOT NULL,
	literal JSONB NULL,
	context_field TEXT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CHECK ((literal IS NOT NULL) <> (context_field IS NOT NULL))
);
