-- P9-46 (WBS 9.26): every compression attempt, persisted as it completes —
-- failed attempts and their validation results are retained for debugging.
-- Append-only (an UPDATE is rejected by trigger).
CREATE TABLE IF NOT EXISTS compression_attempts (
	id UUID PRIMARY KEY,
	run_id UUID NOT NULL REFERENCES compression_runs (id) ON DELETE CASCADE,
	canonical_prompt_id UUID NOT NULL REFERENCES canonical_prompts (id) ON DELETE CASCADE,
	number INTEGER NOT NULL CHECK (number >= 1),
	status TEXT NOT NULL CHECK (status IN ('ACCEPTED', 'VALIDATION_FAILED', 'INEFFECTIVE', 'PROVIDER_ERROR')),
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	error_kind TEXT NOT NULL DEFAULT '',
	error_message TEXT NOT NULL DEFAULT '',
	validation JSONB NULL,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	finish_reason TEXT NOT NULL DEFAULT '',
	provider_request_id TEXT NOT NULL DEFAULT '',
	duration_ms BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (run_id, number)
);
CREATE INDEX IF NOT EXISTS compression_attempts_canonical_idx ON compression_attempts (canonical_prompt_id);

CREATE OR REPLACE FUNCTION overseer_reject_update() RETURNS trigger AS $$
BEGIN
	RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS compression_attempts_immutable ON compression_attempts;
CREATE TRIGGER compression_attempts_immutable BEFORE UPDATE ON compression_attempts
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
