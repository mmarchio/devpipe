-- P11-19 (WBS 11.36): asynchronous precompute jobs.
CREATE TABLE IF NOT EXISTS precompute_jobs (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	trigger TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'COMPLETED', 'PARTIAL', 'FAILED', 'CANCELLED')),
	owner TEXT NOT NULL DEFAULT '',
	base_snapshot_id UUID NULL,
	state_key TEXT NOT NULL DEFAULT '',
	work_unit_id UUID NULL,
	depth INTEGER NOT NULL DEFAULT 0,
	max_candidates INTEGER NOT NULL DEFAULT 0,
	candidate_count INTEGER NOT NULL DEFAULT 0,
	hit_count INTEGER NOT NULL DEFAULT 0,
	built_count INTEGER NOT NULL DEFAULT 0,
	fragment_count INTEGER NOT NULL DEFAULT 0,
	template_count INTEGER NOT NULL DEFAULT 0,
	skipped_count INTEGER NOT NULL DEFAULT 0,
	failed_count INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	started_at TIMESTAMPTZ NULL,
	completed_at TIMESTAMPTZ NULL,
	heartbeat_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS precompute_jobs_project_idx ON precompute_jobs (project_id, created_at);
CREATE INDEX IF NOT EXISTS precompute_jobs_status_idx ON precompute_jobs (status);
