-- P17-23 (WBS 17.103–17.106): audit log of human / API-originated
-- control-plane actions. Project / execution / phase / unit references are
-- plain ids (no FK): the history survives the deletion of what it
-- describes. Rows are immutable (no UPDATE, no DELETE).
CREATE TABLE IF NOT EXISTS audit_events (
	id UUID PRIMARY KEY,
	seq BIGSERIAL NOT NULL UNIQUE,
	created_at TIMESTAMPTZ NOT NULL,
	project_id UUID NULL,
	project_name TEXT NOT NULL DEFAULT '',
	execution_id UUID NULL,
	phase_id UUID NULL,
	work_unit_id UUID NULL,
	actor_id TEXT NOT NULL CHECK (actor_id <> ''),
	actor_name TEXT NOT NULL CHECK (actor_name <> ''),
	actor_type TEXT NOT NULL CHECK (actor_type IN ('LOCAL_ADMIN', 'SYSTEM', 'USER')),
	action TEXT NOT NULL CHECK (action IN ('PROJECT_CREATE', 'PROJECT_UPDATE', 'PROJECT_DELETE',
		'LIFECYCLE_START', 'LIFECYCLE_PAUSE', 'LIFECYCLE_RESUME', 'LIFECYCLE_CANCEL', 'ENGINE_STEP',
		'PHASE_APPEND', 'PHASE_UPDATE', 'WBS_EDIT', 'CONFIG_EDIT', 'ROLE_EDIT', 'WORKFLOW_EDIT',
		'STATE_FILE_EDIT', 'TEMPLATE_EDIT', 'DRIFT_RESOLVE', 'RECOVERY_ACTION', 'WORKER_OPERATE', 'MAINTENANCE')),
	resource_kind TEXT NOT NULL CHECK (resource_kind <> ''),
	resource_id TEXT NOT NULL DEFAULT '',
	result TEXT NOT NULL CHECK (result IN ('SUCCEEDED', 'REJECTED', 'FAILED')),
	http_status INT NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
	request_id TEXT NOT NULL CHECK (request_id <> ''),
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	metadata JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS audit_events_project_idx ON audit_events (project_id, seq);
CREATE INDEX IF NOT EXISTS audit_events_action_idx ON audit_events (action, seq);
CREATE INDEX IF NOT EXISTS audit_events_created_idx ON audit_events (created_at);
CREATE INDEX IF NOT EXISTS audit_events_execution_idx ON audit_events (execution_id, seq) WHERE execution_id IS NOT NULL;
DROP TRIGGER IF EXISTS audit_events_immutable ON audit_events;
CREATE TRIGGER audit_events_immutable BEFORE UPDATE OR DELETE ON audit_events
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
