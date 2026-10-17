-- P11-19 (WBS 11.37): per-candidate execution records.
CREATE TABLE IF NOT EXISTS precompute_candidates (
	id UUID PRIMARY KEY,
	job_id UUID NOT NULL REFERENCES precompute_jobs (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	state_key TEXT NOT NULL,
	role_key TEXT NOT NULL DEFAULT '',
	phase_id UUID NULL,
	work_unit_id UUID NULL,
	paths JSONB NOT NULL DEFAULT '[]'::jsonb,
	depth INTEGER NOT NULL DEFAULT 0,
	next_unit BOOLEAN NOT NULL DEFAULT false,
	readiness TEXT NOT NULL CHECK (readiness IN ('FULLY_KNOWN', 'PARTIALLY_KNOWN', 'UNKNOWN')),
	priority INTEGER NOT NULL,
	missing JSONB NOT NULL DEFAULT '[]'::jsonb,
	result TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	cache_key TEXT NOT NULL DEFAULT '',
	cache_entry_id UUID NULL,
	fragment_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
	template_id UUID NULL,
	projection_id UUID NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS precompute_candidates_job_idx ON precompute_candidates (job_id, priority);
CREATE INDEX IF NOT EXISTS precompute_candidates_project_idx ON precompute_candidates (project_id);
