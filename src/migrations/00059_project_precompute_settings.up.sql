-- P11-18 (WBS 11.3/11.34/11.39/11.40): per-project precompute settings. An
-- absent row means the documented defaults.
CREATE TABLE IF NOT EXISTS project_precompute_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	enabled BOOLEAN NOT NULL,
	depth INTEGER NOT NULL CHECK (depth BETWEEN 1 AND 5),
	max_candidates INTEGER NOT NULL CHECK (max_candidates BETWEEN 1 AND 64),
	concurrency INTEGER NOT NULL CHECK (concurrency BETWEEN 1 AND 16),
	compression_enabled BOOLEAN NOT NULL,
	static_fragments BOOLEAN NOT NULL,
	during_worker_execution BOOLEAN NOT NULL,
	same_session_compression BOOLEAN NOT NULL,
	plan_next_phase BOOLEAN NOT NULL,
	max_tokens INTEGER NOT NULL DEFAULT 0 CHECK (max_tokens >= 0),
	max_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (max_duration_ms >= 0),
	resource_policy TEXT NOT NULL CHECK (resource_policy IN ('ALWAYS', 'IDLE_ONLY', 'REMOTE_ONLY_WHILE_WORKER_ACTIVE', 'DISABLED_DURING_LOCAL_WORKER')),
	worker_is_local BOOLEAN NOT NULL DEFAULT false,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
