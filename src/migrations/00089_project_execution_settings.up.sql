-- P15-23 (WBS 15.18, 15.20, 15.51, 15.60): per-project execution/bootstrap
-- settings. Absent row = defaults (git not required, dirty WARN, no
-- materialization, 100 control transitions, 60 s bootstrap lease).
CREATE TABLE IF NOT EXISTS project_execution_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	require_git_repository BOOLEAN NOT NULL,
	dirty_policy TEXT NOT NULL CHECK (dirty_policy IN ('ALLOW', 'WARN', 'REJECT')),
	materialize_state_files BOOLEAN NOT NULL,
	max_bootstrap_control_transitions INTEGER NOT NULL CHECK (max_bootstrap_control_transitions BETWEEN 1 AND 1000),
	bootstrap_lease_seconds INTEGER NOT NULL CHECK (bootstrap_lease_seconds BETWEEN 1 AND 3600),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
