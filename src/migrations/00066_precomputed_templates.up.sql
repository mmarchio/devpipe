-- P11-24A (PLAN Decision 5, user requirement): pending precomputed prompt
-- templates — a projected prompt whose ACTIVE_TASK is a placeholder. A
-- template is never a worker prompt; it is resolved (placeholder replaced
-- with the real value, then fully re-validated) when the unit is actually
-- processed. At most one PENDING template per (project, normalized key).
CREATE TABLE IF NOT EXISTS precomputed_templates (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	projection_id UUID NOT NULL REFERENCES precompute_projections (id) ON DELETE CASCADE,
	work_unit_id UUID NOT NULL,
	state_key TEXT NOT NULL,
	normalized_key TEXT NOT NULL,
	placeholder_token TEXT NOT NULL,
	canonical_prompt_id UUID NOT NULL REFERENCES canonical_prompts (id) ON DELETE CASCADE,
	compressed_prompt_id UUID NULL REFERENCES compressed_prompts (id) ON DELETE CASCADE,
	content_source TEXT NOT NULL CHECK (content_source IN ('COMPRESSED', 'CANONICAL')),
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'RESOLVED', 'STALE', 'FAILED')),
	resolved_entry_id UUID NULL,
	reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	resolved_at TIMESTAMPTZ NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS precomputed_templates_pending_key
	ON precomputed_templates (project_id, normalized_key) WHERE status = 'PENDING';
-- Template compression runs (no worker prompt) are a distinct run outcome.
ALTER TABLE compression_runs DROP CONSTRAINT IF EXISTS compression_runs_outcome_check;
ALTER TABLE compression_runs ADD CONSTRAINT compression_runs_outcome_check
	CHECK (outcome IN ('PENDING', 'BYPASSED', 'COMPRESSED', 'FALLBACK_CANONICAL', 'FAILED', 'TEMPLATE'));
