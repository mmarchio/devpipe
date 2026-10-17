-- P12-49 (WBS 12.53): worker audit events. Payloads are sanitized (no
-- prompt/response content, no credentials). Append-only.
CREATE TABLE IF NOT EXISTS worker_events (
	id UUID PRIMARY KEY,
	seq BIGINT GENERATED ALWAYS AS IDENTITY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	session_id UUID NULL REFERENCES worker_sessions (id) ON DELETE CASCADE,
	dispatch_id UUID NULL REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	type TEXT NOT NULL CHECK (type IN ('WORKER_CONNECTED', 'WORKER_DISCONNECTED', 'WORKER_RECONNECTED', 'WORKER_CONNECTION_FAILED',
		'SESSION_CREATED', 'SESSION_RESET', 'SESSION_LOST', 'SESSION_CLOSED',
		'WORKER_REQUEST_STARTED', 'WORKER_REQUEST_COMPLETED', 'WORKER_REQUEST_FAILED', 'WORKER_REQUEST_CANCELLED',
		'WORKER_DELIVERY_UNKNOWN', 'WORKER_CWD_CHANGED', 'WORKER_CONTEXT_OBSERVED')),
	payload JSONB NOT NULL DEFAULT '{}'::jsonb,
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS worker_events_project_idx ON worker_events (project_id, seq);
CREATE INDEX IF NOT EXISTS worker_events_dispatch_idx ON worker_events (dispatch_id);
DROP TRIGGER IF EXISTS worker_events_immutable ON worker_events;
CREATE TRIGGER worker_events_immutable BEFORE UPDATE ON worker_events
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
