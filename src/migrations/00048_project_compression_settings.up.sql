-- P9-05 (WBS 9.2 + 9.31): compression-specific project settings extending the
-- project configuration (provider/model/context window/threshold live in
-- project_settings). An absent row means the documented defaults. CHECKs
-- mirror compression.Settings.ValidateCompressionFields.
CREATE TABLE IF NOT EXISTS project_compression_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	mode TEXT NOT NULL CHECK (mode IN ('AUTO', 'ALWAYS', 'NEVER')),
	target_ratio DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (target_ratio >= 0 AND target_ratio <= 1),
	target_tokens INTEGER NOT NULL DEFAULT 0 CHECK (target_tokens >= 0),
	minimum_savings DOUBLE PRECISION NOT NULL CHECK (minimum_savings >= 0 AND minimum_savings < 1),
	max_attempts INTEGER NOT NULL CHECK (max_attempts >= 1 AND max_attempts <= 10),
	validation_mode TEXT NOT NULL CHECK (validation_mode IN ('DETERMINISTIC', 'HYBRID')),
	timeout_ms BIGINT NOT NULL CHECK (timeout_ms > 0),
	backoff_base_ms BIGINT NOT NULL CHECK (backoff_base_ms >= 0),
	backoff_max_ms BIGINT NOT NULL CHECK (backoff_max_ms >= backoff_base_ms),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
