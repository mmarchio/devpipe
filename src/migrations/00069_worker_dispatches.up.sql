-- P12-45 (WBS 12.30/12.32/12.33/12.58): one persisted send attempt. The
-- row is written PENDING before anything is transmitted (WBS 12.30.1). The
-- session must belong to the same project (composite FK, WBS 12.55). One
-- logical operation (runtime state, work unit, worker prompt) may have
-- several attempts; each attempt is unique (idempotency, WBS 12.33) and a
-- terminal status is never reopened (attempt history retained, WBS 12.58).
-- Prompt content is never copied here: prompt id + hash only.
CREATE TABLE IF NOT EXISTS worker_dispatches (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	session_id UUID NOT NULL,
	phase_id UUID NULL REFERENCES phases (id) ON DELETE SET NULL,
	work_unit_id UUID NULL,
	worker_prompt_id UUID NOT NULL REFERENCES worker_prompts (id) ON DELETE CASCADE,
	prompt_hash TEXT NOT NULL CHECK (prompt_hash <> ''),
	runtime_state_id UUID NOT NULL REFERENCES runtime_state (id) ON DELETE CASCADE,
	state_key TEXT NOT NULL DEFAULT '',
	role_key TEXT NOT NULL DEFAULT '',
	request_id UUID NOT NULL UNIQUE,
	operation_id TEXT NOT NULL,
	attempt INTEGER NOT NULL CHECK (attempt > 0),
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'SENDING', 'WAITING', 'COMPLETED', 'FAILED', 'CANCELLED', 'UNKNOWN')),
	delivery TEXT NULL CHECK (delivery IN ('DEFINITELY_NOT_SENT', 'SENT_RESPONSE_RECEIVED', 'DELIVERY_UNKNOWN')),
	retry_class TEXT NULL CHECK (retry_class IN ('SAFE_TO_RETRY', 'UNSAFE_TO_RETRY', 'REQUIRES_RECOVERY')),
	error_code TEXT NOT NULL DEFAULT '',
	error_message TEXT NOT NULL DEFAULT '',
	provider_code TEXT NOT NULL DEFAULT '',
	retry_after_ms BIGINT NULL CHECK (retry_after_ms >= 0),
	diagnostic TEXT NOT NULL DEFAULT '',
	owner TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	started_at TIMESTAMPTZ NULL,
	completed_at TIMESTAMPTZ NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	FOREIGN KEY (session_id, project_id) REFERENCES worker_sessions (id, project_id) ON DELETE CASCADE,
	CHECK (status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'UNKNOWN') OR completed_at IS NOT NULL),
	CHECK (status <> 'UNKNOWN' OR delivery = 'DELIVERY_UNKNOWN')
);
CREATE UNIQUE INDEX IF NOT EXISTS worker_dispatches_idempotency
	ON worker_dispatches (runtime_state_id, COALESCE(work_unit_id, '00000000-0000-0000-0000-000000000000'::uuid), worker_prompt_id, attempt);
CREATE INDEX IF NOT EXISTS worker_dispatches_project_idx ON worker_dispatches (project_id, created_at);
CREATE INDEX IF NOT EXISTS worker_dispatches_session_idx ON worker_dispatches (session_id, created_at);
CREATE INDEX IF NOT EXISTS worker_dispatches_operation_idx ON worker_dispatches (operation_id, attempt);
CREATE INDEX IF NOT EXISTS worker_dispatches_inflight_idx ON worker_dispatches (status) WHERE status IN ('PENDING', 'SENDING', 'WAITING');

CREATE OR REPLACE FUNCTION worker_dispatch_guard() RETURNS trigger AS $$
BEGIN
	IF NEW.id <> OLD.id OR NEW.project_id <> OLD.project_id OR NEW.session_id <> OLD.session_id
		OR NEW.worker_prompt_id <> OLD.worker_prompt_id OR NEW.prompt_hash <> OLD.prompt_hash
		OR NEW.runtime_state_id <> OLD.runtime_state_id OR NEW.request_id <> OLD.request_id
		OR NEW.operation_id <> OLD.operation_id OR NEW.attempt <> OLD.attempt OR NEW.created_at <> OLD.created_at THEN
		RAISE EXCEPTION 'worker dispatch identity is immutable' USING ERRCODE = 'check_violation';
	END IF;
	IF OLD.status IN ('COMPLETED', 'FAILED', 'CANCELLED', 'UNKNOWN') THEN
		RAISE EXCEPTION 'worker dispatch % is final', OLD.status USING ERRCODE = 'check_violation';
	END IF;
	IF NOT ((OLD.status = NEW.status)
		OR (OLD.status = 'PENDING' AND NEW.status IN ('SENDING', 'FAILED', 'CANCELLED'))
		OR (OLD.status = 'SENDING' AND NEW.status IN ('WAITING', 'FAILED', 'CANCELLED', 'UNKNOWN'))
		OR (OLD.status = 'WAITING' AND NEW.status IN ('COMPLETED', 'FAILED', 'CANCELLED', 'UNKNOWN'))) THEN
		RAISE EXCEPTION 'illegal worker dispatch transition % -> %', OLD.status, NEW.status USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS worker_dispatches_guard ON worker_dispatches;
CREATE TRIGGER worker_dispatches_guard BEFORE UPDATE ON worker_dispatches
	FOR EACH ROW EXECUTE FUNCTION worker_dispatch_guard();
