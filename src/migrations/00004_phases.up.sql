CREATE TABLE IF NOT EXISTS phases (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	phase_order INTEGER NOT NULL CHECK (phase_order > 0),
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'PENDING'
		CHECK (status IN ('PENDING','PLANNING','READY','EXECUTING','VALIDATING','DONE','FAILED')),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	started_at TIMESTAMPTZ,
	completed_at TIMESTAMPTZ,
	UNIQUE (project_id, phase_order)
);
