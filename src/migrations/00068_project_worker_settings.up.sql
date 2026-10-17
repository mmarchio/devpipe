-- P12-44 (WBS 12.22/12.56/12.57): per-project worker session policy,
-- same-session concurrency policy and dispatch lease timing. Absent row =
-- defaults (SINGLE_ACTIVE_PER_PROJECT, REJECT, 60 s lease).
CREATE TABLE IF NOT EXISTS project_worker_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	session_policy TEXT NOT NULL CHECK (session_policy IN ('SINGLE_ACTIVE_PER_PROJECT', 'MULTIPLE_ACTIVE')),
	concurrent_send TEXT NOT NULL CHECK (concurrent_send IN ('REJECT', 'WAIT')),
	lease_ms BIGINT NOT NULL CHECK (lease_ms >= 1000),
	wait_ms BIGINT NOT NULL CHECK (wait_ms >= 0),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
