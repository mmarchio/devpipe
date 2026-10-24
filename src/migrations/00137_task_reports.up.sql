-- Phase 19: the team's structured result of a dispatch, submitted through
-- the overseer MCP server (submit_result). Under DATABASE state authority
-- the check states read these rows instead of the statefiles. Immutable:
-- a report is evidence.
CREATE TABLE IF NOT EXISTS task_reports (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	dispatch_id UUID NOT NULL REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE CASCADE,
	role_key TEXT NOT NULL CHECK (role_key <> ''),
	state_key TEXT NOT NULL CHECK (state_key <> ''),
	kind TEXT NOT NULL CHECK (kind IN ('IMPLEMENT', 'TEST', 'ACCEPTANCE', 'PLAN', 'ANALYSIS')),
	status TEXT NOT NULL CHECK (status IN ('COMPLETE', 'INCOMPLETE', 'BLOCKED', 'FAILED')),
	criteria_met JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(criteria_met) = 'array'),
	summary TEXT NOT NULL,
	evidence JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(evidence) = 'object'),
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (dispatch_id, kind)
);
CREATE INDEX IF NOT EXISTS task_reports_execution_idx ON task_reports (execution_id, created_at);
CREATE INDEX IF NOT EXISTS task_reports_unit_idx ON task_reports (work_unit_id, created_at);
DROP TRIGGER IF EXISTS task_reports_immutable ON task_reports;
CREATE TRIGGER task_reports_immutable BEFORE UPDATE ON task_reports
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
