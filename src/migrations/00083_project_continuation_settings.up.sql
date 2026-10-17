-- P14-26 (WBS 14.12, 14.13, 14.37, 14.38, 14.41): per-project continuation
-- settings. The context threshold itself is project_settings.context_threshold.
-- Absent row = defaults (fresh_session, estimates may reset, 8 / 4 / 3, 0, 60 s).
CREATE TABLE IF NOT EXISTS project_continuation_settings (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	unknown_context_policy TEXT NOT NULL CHECK (unknown_context_policy IN ('same_session', 'fresh_session')),
	estimated_may_reset BOOLEAN NOT NULL,
	max_continuations_per_work_unit INTEGER NOT NULL CHECK (max_continuations_per_work_unit >= 1),
	max_session_resets_per_work_unit INTEGER NOT NULL CHECK (max_session_resets_per_work_unit >= 0),
	max_no_progress_continuations INTEGER NOT NULL CHECK (max_no_progress_continuations >= 1),
	session_create_retries INTEGER NOT NULL CHECK (session_create_retries BETWEEN 0 AND 10),
	lease_seconds INTEGER NOT NULL CHECK (lease_seconds >= 1),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
