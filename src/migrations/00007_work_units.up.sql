CREATE TABLE IF NOT EXISTS work_units (
	id UUID PRIMARY KEY,
	execution_plan_id UUID NOT NULL REFERENCES execution_plans (id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL
		CHECK (kind IN ('ATOMIC','COMPOSITION','INTEGRATION','VALIDATION')),
	status TEXT NOT NULL DEFAULT 'PENDING'
		CHECK (status IN ('PENDING','READY','EXECUTING','INCOMPLETE','VALIDATING','DONE','FAILED','BLOCKED')),
	objective TEXT NOT NULL,
	implementation_instructions TEXT NOT NULL DEFAULT '',
	acceptance_criteria JSONB NOT NULL DEFAULT '[]',
	required_tests JSONB NOT NULL DEFAULT '[]',
	constraints JSONB NOT NULL DEFAULT '[]',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	started_at TIMESTAMPTZ,
	completed_at TIMESTAMPTZ,
	UNIQUE (execution_plan_id, sequence)
);
