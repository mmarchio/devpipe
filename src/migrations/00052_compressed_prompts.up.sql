-- P9-47 (WBS 9.13/9.14): compressed outputs, persisted separately from the
-- canonical prompt (never overwriting it). One row per attempt that produced
-- output; validation_status records the pipeline verdict. Immutable.
CREATE TABLE IF NOT EXISTS compressed_prompts (
	id UUID PRIMARY KEY,
	canonical_prompt_id UUID NOT NULL REFERENCES canonical_prompts (id) ON DELETE CASCADE,
	attempt_id UUID NOT NULL UNIQUE REFERENCES compression_attempts (id) ON DELETE CASCADE,
	instruction_version INTEGER NOT NULL,
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	attempt INTEGER NOT NULL CHECK (attempt >= 1),
	content TEXT NOT NULL,
	hash TEXT NOT NULL,
	canonical_tokens INTEGER NOT NULL,
	compressed_tokens INTEGER NOT NULL,
	validation_status TEXT NOT NULL CHECK (validation_status IN ('PENDING', 'PASSED', 'FAILED', 'INEFFECTIVE')),
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS compressed_prompts_canonical_idx ON compressed_prompts (canonical_prompt_id);
DROP TRIGGER IF EXISTS compressed_prompts_immutable ON compressed_prompts;
CREATE TRIGGER compressed_prompts_immutable BEFORE UPDATE ON compressed_prompts
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
