-- P12-50 (WBS 12.47): non-authoritative tool activity telemetry. It never
-- drives work-unit status.
CREATE TABLE IF NOT EXISTS worker_tool_events (
	id UUID PRIMARY KEY,
	seq BIGINT GENERATED ALWAYS AS IDENTITY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	dispatch_id UUID NOT NULL REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	tool_name TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('STARTED', 'COMPLETED', 'FAILED')),
	started_at TIMESTAMPTZ NOT NULL,
	finished_at TIMESTAMPTZ NULL,
	duration_ms BIGINT NULL CHECK (duration_ms >= 0),
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS worker_tool_events_dispatch_idx ON worker_tool_events (dispatch_id, seq);
DROP TRIGGER IF EXISTS worker_tool_events_immutable ON worker_tool_events;
CREATE TRIGGER worker_tool_events_immutable BEFORE UPDATE ON worker_tool_events
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
