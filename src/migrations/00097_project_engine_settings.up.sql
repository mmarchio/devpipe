-- P16-54 (WBS 16.38, 16.43–16.57, 16.90): per-project autonomous-execution
-- engine settings + final-hardening configuration. Absent row = defaults.
CREATE TABLE IF NOT EXISTS project_engine_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	max_fix_attempts INTEGER NOT NULL CHECK (max_fix_attempts BETWEEN 1 AND 100),
	max_plan_attempts INTEGER NOT NULL CHECK (max_plan_attempts BETWEEN 1 AND 100),
	checkpoint_policy TEXT NOT NULL CHECK (checkpoint_policy IN ('REQUIRED', 'BEST_EFFORT', 'DISABLED')),
	unit_validation JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(unit_validation) = 'array'),
	phase_validation JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(phase_validation) = 'array'),
	final_validation JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(final_validation) = 'array'),
	validation_timeout_seconds INTEGER NOT NULL CHECK (validation_timeout_seconds BETWEEN 1 AND 86400),
	output_cap_bytes INTEGER NOT NULL CHECK (output_cap_bytes BETWEEN 1024 AND 16777216),
	step_budget INTEGER NOT NULL CHECK (step_budget BETWEEN 1 AND 1000),
	driver_lease_seconds INTEGER NOT NULL CHECK (driver_lease_seconds BETWEEN 1 AND 3600),
	atomic_policy JSONB NOT NULL CHECK (jsonb_typeof(atomic_policy) = 'object'),
	hardening JSONB NOT NULL CHECK (jsonb_typeof(hardening) = 'object'),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
