-- P9-46 (WBS 9.26/9.29/9.30/9.43): one prompt-preparation run over a canonical
-- prompt — request identity, configuration hash, decision outcome + reason
-- (bypass / fallback decisions are recorded here), dropped optional
-- components. A run is created PENDING and finalized exactly once.
CREATE TABLE IF NOT EXISTS compression_runs (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	canonical_prompt_id UUID NOT NULL REFERENCES canonical_prompts (id) ON DELETE CASCADE,
	request_identity TEXT NOT NULL,
	config_hash TEXT NOT NULL,
	instruction_version INTEGER NULL REFERENCES compression_instructions (version),
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL,
	target_tokens INTEGER NOT NULL,
	canonical_tokens INTEGER NOT NULL,
	outcome TEXT NOT NULL CHECK (outcome IN ('PENDING', 'BYPASSED', 'COMPRESSED', 'FALLBACK_CANONICAL', 'FAILED')),
	reason TEXT NOT NULL DEFAULT '',
	dropped JSONB NOT NULL DEFAULT '[]'::jsonb,
	attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	created_at TIMESTAMPTZ NOT NULL,
	finished_at TIMESTAMPTZ NULL
);
CREATE INDEX IF NOT EXISTS compression_runs_canonical_idx ON compression_runs (canonical_prompt_id);
CREATE INDEX IF NOT EXISTS compression_runs_identity_idx ON compression_runs (request_identity);
CREATE INDEX IF NOT EXISTS compression_runs_project_idx ON compression_runs (project_id);
