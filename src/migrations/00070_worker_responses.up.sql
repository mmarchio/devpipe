-- P12-46 (WBS 12.31): the raw worker response, byte-exact, stored before
-- any Phase 13 interpretation. One per dispatch; immutable.
CREATE TABLE IF NOT EXISTS worker_responses (
	id UUID PRIMARY KEY,
	dispatch_id UUID NOT NULL UNIQUE REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL CHECK (length(content_hash) = 64),
	provider_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
	input_tokens BIGINT NULL,
	output_tokens BIGINT NULL,
	context_tokens BIGINT NULL,
	context_window BIGINT NULL,
	usage_estimated BOOLEAN NOT NULL DEFAULT false,
	started_at TIMESTAMPTZ NOT NULL,
	completed_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS worker_responses_project_idx ON worker_responses (project_id, created_at);
DROP TRIGGER IF EXISTS worker_responses_immutable ON worker_responses;
CREATE TRIGGER worker_responses_immutable BEFORE UPDATE ON worker_responses
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
