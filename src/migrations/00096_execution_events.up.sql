-- P15-30 (WBS 15.104): append-only execution bootstrap/lifecycle events.
-- execution_id is NULL for events recorded before an execution exists
-- (requests, readiness failures).
CREATE TABLE IF NOT EXISTS execution_events (
	id UUID PRIMARY KEY,
	seq BIGSERIAL NOT NULL UNIQUE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	execution_id UUID NULL REFERENCES executions (id) ON DELETE CASCADE,
	type TEXT NOT NULL CHECK (type IN ('EXECUTION_BOOTSTRAP_REQUESTED', 'EXECUTION_READINESS_VALIDATED',
		'EXECUTION_CONFIGURATION_SNAPSHOTTED', 'EXECUTION_STATE_FILES_INITIALIZED', 'EXECUTION_RUNTIME_INITIALIZED',
		'EXECUTION_CONTEXT_PREPARED', 'EXECUTION_PROMPT_PREPARED', 'EXECUTION_BOOTSTRAP_COMPLETED',
		'EXECUTION_BOOTSTRAP_FAILED', 'EXECUTION_STARTED')),
	payload JSONB NOT NULL DEFAULT '{}',
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS execution_events_project_idx ON execution_events (project_id, created_at);
CREATE INDEX IF NOT EXISTS execution_events_execution_idx ON execution_events (execution_id, created_at);
DROP TRIGGER IF EXISTS execution_events_immutable ON execution_events;
CREATE TRIGGER execution_events_immutable BEFORE UPDATE ON execution_events
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
