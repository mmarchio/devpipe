-- P12-43 (WBS 12.17/12.22): the overseer's knowledge of external worker
-- sessions. The session itself lives in the worker (Hermes); a row records
-- what the overseer last confirmed. working_directory NULL = UNKNOWN (WBS
-- 12.26). Under SINGLE_ACTIVE_PER_PROJECT at most one ACTIVE row exists per
-- project (partial unique index); only ACTIVE may change status, and a
-- non-ACTIVE status is final (history retained, WBS 12.17.4).
CREATE TABLE IF NOT EXISTS worker_sessions (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	provider TEXT NOT NULL CHECK (provider <> ''),
	provider_session_id TEXT NOT NULL CHECK (provider_session_id <> ''),
	status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'RESET', 'LOST', 'CLOSED', 'FAILED')),
	session_policy TEXT NOT NULL CHECK (session_policy IN ('SINGLE_ACTIVE_PER_PROJECT', 'MULTIPLE_ACTIVE')),
	working_directory TEXT NULL CHECK (working_directory IS NULL OR working_directory <> ''),
	usage JSONB NOT NULL DEFAULT '{}'::jsonb,
	status_reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	last_seen_at TIMESTAMPTZ NOT NULL,
	closed_at TIMESTAMPTZ NULL,
	UNIQUE (id, project_id),
	UNIQUE (project_id, provider, provider_session_id),
	CHECK ((status = 'ACTIVE') = (closed_at IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS worker_sessions_single_active
	ON worker_sessions (project_id) WHERE status = 'ACTIVE' AND session_policy = 'SINGLE_ACTIVE_PER_PROJECT';
CREATE INDEX IF NOT EXISTS worker_sessions_project_idx ON worker_sessions (project_id, created_at);

CREATE OR REPLACE FUNCTION worker_session_guard() RETURNS trigger AS $$
BEGIN
	IF NEW.id <> OLD.id OR NEW.project_id <> OLD.project_id OR NEW.provider <> OLD.provider
		OR NEW.provider_session_id <> OLD.provider_session_id OR NEW.session_policy <> OLD.session_policy
		OR NEW.created_at <> OLD.created_at THEN
		RAISE EXCEPTION 'worker session identity is immutable' USING ERRCODE = 'check_violation';
	END IF;
	IF OLD.status <> 'ACTIVE' AND (NEW.status <> OLD.status OR NEW.working_directory IS DISTINCT FROM OLD.working_directory) THEN
		RAISE EXCEPTION 'worker session % is final', OLD.status USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS worker_sessions_guard ON worker_sessions;
CREATE TRIGGER worker_sessions_guard BEFORE UPDATE ON worker_sessions
	FOR EACH ROW EXECUTE FUNCTION worker_session_guard();
