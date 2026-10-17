-- P9-48 (WBS 9.32/9.33): the worker-ready prompt — the exact content to be
-- sent to the worker, its hash, and its source. A COMPRESSED worker prompt
-- always references its (validated) compressed prompt; a CANONICAL one never
-- does. Immutable: the historical hash cannot change (T9.33.4).
CREATE TABLE IF NOT EXISTS worker_prompts (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	canonical_prompt_id UUID NOT NULL REFERENCES canonical_prompts (id) ON DELETE CASCADE,
	compressed_prompt_id UUID NULL REFERENCES compressed_prompts (id) ON DELETE CASCADE,
	run_id UUID NOT NULL UNIQUE REFERENCES compression_runs (id) ON DELETE CASCADE,
	source TEXT NOT NULL CHECK (source IN ('CANONICAL', 'COMPRESSED')),
	content TEXT NOT NULL,
	hash TEXT NOT NULL,
	estimated_tokens INTEGER NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	CHECK ((source = 'COMPRESSED') = (compressed_prompt_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS worker_prompts_canonical_idx ON worker_prompts (canonical_prompt_id);
DROP TRIGGER IF EXISTS worker_prompts_immutable ON worker_prompts;
CREATE TRIGGER worker_prompts_immutable BEFORE UPDATE ON worker_prompts
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
