-- P12-47 (WBS 12.50): context telemetry observations per dispatch/session.
-- seq orders observations; estimated + source keep provider facts and
-- overseer estimates apart (WBS 12.15). Append-only.
CREATE TABLE IF NOT EXISTS worker_usage_observations (
	id UUID PRIMARY KEY,
	seq BIGINT GENERATED ALWAYS AS IDENTITY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	session_id UUID NOT NULL REFERENCES worker_sessions (id) ON DELETE CASCADE,
	dispatch_id UUID NULL REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	input_tokens BIGINT NULL,
	output_tokens BIGINT NULL,
	context_tokens BIGINT NULL,
	context_window BIGINT NULL CHECK (context_window IS NULL OR context_window > 0),
	estimated BOOLEAN NOT NULL,
	source TEXT NOT NULL CHECK (source IN ('PROVIDER_ACTUAL', 'PROVIDER_DERIVED', 'ESTIMATED', 'UNKNOWN')),
	observed_at TIMESTAMPTZ NOT NULL,
	CHECK (estimated = (source = 'ESTIMATED'))
);
CREATE INDEX IF NOT EXISTS worker_usage_session_idx ON worker_usage_observations (session_id, seq);
CREATE INDEX IF NOT EXISTS worker_usage_project_idx ON worker_usage_observations (project_id, seq);
DROP TRIGGER IF EXISTS worker_usage_immutable ON worker_usage_observations;
CREATE TRIGGER worker_usage_immutable BEFORE UPDATE ON worker_usage_observations
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
