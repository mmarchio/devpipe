-- P18-57 (WBS 18.108–18.110, decision U9): an audit verification failure
-- (INVALID / INCOMPLETE) blocks new work until an operator acknowledges it.
-- One row per acknowledged failure fingerprint (the failing chains, their
-- status, first failing seq and gaps); a different failure needs a new
-- acknowledgement. Rows are immutable. The acknowledgement is itself an
-- audited action (ACKNOWLEDGE_AUDIT_FAILURE).
ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_action_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_action_check CHECK (action IN ('PROJECT_CREATE', 'PROJECT_UPDATE', 'PROJECT_DELETE',
	'LIFECYCLE_START', 'LIFECYCLE_PAUSE', 'LIFECYCLE_RESUME', 'LIFECYCLE_CANCEL', 'ENGINE_STEP',
	'PHASE_APPEND', 'PHASE_UPDATE', 'WBS_EDIT', 'CONFIG_EDIT', 'ROLE_EDIT', 'WORKFLOW_EDIT',
	'STATE_FILE_EDIT', 'TEMPLATE_EDIT', 'DRIFT_RESOLVE', 'RECOVERY_ACTION', 'WORKER_OPERATE', 'MAINTENANCE',
	'ACKNOWLEDGE_AUDIT_FAILURE'));

CREATE TABLE IF NOT EXISTS audit_failure_acknowledgements (
	id UUID PRIMARY KEY,
	fingerprint TEXT NOT NULL UNIQUE CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
	status TEXT NOT NULL CHECK (status IN ('INVALID', 'INCOMPLETE')),
	failure JSONB NOT NULL,
	reason TEXT NOT NULL CHECK (length(btrim(reason)) > 0 AND length(reason) <= 2000),
	acknowledged_by TEXT NOT NULL CHECK (acknowledged_by <> ''),
	acknowledged_by_name TEXT NOT NULL DEFAULT '',
	request_id TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL
);
DROP TRIGGER IF EXISTS audit_failure_acknowledgements_immutable ON audit_failure_acknowledgements;
CREATE TRIGGER audit_failure_acknowledgements_immutable BEFORE UPDATE OR DELETE ON audit_failure_acknowledgements
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
