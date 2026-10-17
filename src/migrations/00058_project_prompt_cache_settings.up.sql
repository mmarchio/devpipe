-- P10-20 (WBS 10.34/10.35/10.57/10.69): per-project prompt-cache policy.
-- An absent row means the documented defaults.
CREATE TABLE IF NOT EXISTS project_prompt_cache_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	enabled BOOLEAN NOT NULL DEFAULT true,
	retention_policy TEXT NOT NULL DEFAULT 'RETAIN_ALL' CHECK (retention_policy IN ('RETAIN_ALL', 'RETAIN_DAYS', 'RETAIN_LATEST_N')),
	retention_value INTEGER NOT NULL DEFAULT 0 CHECK (retention_value >= 0),
	negative_window_ms BIGINT NOT NULL DEFAULT 30000 CHECK (negative_window_ms >= 0),
	lease_ms BIGINT NOT NULL DEFAULT 120000 CHECK (lease_ms > 0),
	cache_same_session_continuation BOOLEAN NOT NULL DEFAULT false,
	bypass_on_cache_error BOOLEAN NOT NULL DEFAULT true,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
