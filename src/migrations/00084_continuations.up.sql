-- P14-27 (WBS 14.1, 14.15, 14.16, 14.54, 14.91, 14.92): persistent
-- continuation operations. Bound to one project / phase / work unit /
-- runtime state; source dispatch (and source result for an INCOMPLETE
-- trigger) must belong to the same unit; one attempt number per unit; at
-- most one nonterminal continuation per work unit; versioned updates and
-- a lease for multi-instance ownership.
CREATE TABLE IF NOT EXISTS continuations (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	phase_id UUID NOT NULL REFERENCES phases (id) ON DELETE CASCADE,
	work_unit_id UUID NOT NULL REFERENCES work_units (id) ON DELETE CASCADE,
	runtime_state_id UUID NOT NULL REFERENCES runtime_state (id) ON DELETE CASCADE,
	runtime_state_version INTEGER NOT NULL CHECK (runtime_state_version > 0),
	source_dispatch_id UUID NOT NULL REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	source_result_id UUID NULL REFERENCES worker_results (id) ON DELETE CASCADE,
	trigger TEXT NOT NULL CHECK (trigger IN ('RESULT_INCOMPLETE', 'CONTEXT_LIMIT', 'SESSION_LOST', 'MANUAL_RETRY')),
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'DECIDING', 'SAME_SESSION', 'RESET_REQUIRED', 'CREATING_SESSION',
		'RESTORING_CONTEXT', 'RESTORING_CWD', 'READY', 'DISPATCHING', 'WAITING', 'COMPLETED', 'BLOCKED', 'FAILED',
		'CANCELLED', 'RECOVERY_REQUIRED')),
	strategy TEXT NULL CHECK (strategy IN ('same_session', 'fresh_session')),
	decision_reason TEXT NULL CHECK (decision_reason IN ('below_threshold', 'at_threshold', 'above_threshold',
		'context_unknown', 'context_limit', 'session_lost', 'operator_override')),
	decision JSONB NULL,
	attempt INTEGER NOT NULL CHECK (attempt > 0),
	captured_status JSONB NOT NULL,
	status_hash TEXT NOT NULL CHECK (length(status_hash) = 64),
	progress_key TEXT NOT NULL CHECK (length(progress_key) = 64),
	status_unknown BOOLEAN NOT NULL DEFAULT false,
	no_progress_streak INTEGER NOT NULL DEFAULT 1 CHECK (no_progress_streak >= 0),
	scope_expanded BOOLEAN NOT NULL DEFAULT false,
	source_snapshot_id UUID NULL REFERENCES context_snapshots (id) ON DELETE SET NULL,
	snapshot_id UUID NULL REFERENCES context_snapshots (id) ON DELETE SET NULL,
	role_key TEXT NOT NULL,
	state_key TEXT NOT NULL,
	session_id UUID NULL REFERENCES worker_sessions (id) ON DELETE SET NULL,
	old_session_id UUID NULL REFERENCES worker_sessions (id) ON DELETE SET NULL,
	new_session_id UUID NULL REFERENCES worker_sessions (id) ON DELETE SET NULL,
	session_confirmed_at TIMESTAMPTZ NULL,
	cwd_confirmed_at TIMESTAMPTZ NULL,
	session_create_failures INTEGER NOT NULL DEFAULT 0 CHECK (session_create_failures >= 0),
	worker_prompt_id UUID NULL REFERENCES worker_prompts (id) ON DELETE SET NULL,
	dispatch_id UUID NULL REFERENCES worker_dispatches (id) ON DELETE SET NULL,
	recovery_reason TEXT NULL CHECK (recovery_reason IN ('DELIVERY_UNKNOWN', 'NO_PROGRESS', 'CONTINUATION_LIMIT',
		'RESET_LIMIT', 'SESSION_CREATE_FAILED', 'CWD_RESTORE_FAILED', 'BINDING_CHANGED', 'DISPATCH_REJECTED',
		'SESSION_LOST_AFTER_SEND', 'RESULT_REJECTED', 'DISPATCH_MISSING')),
	override_strategy TEXT NULL CHECK (override_strategy IN ('same_session', 'fresh_session')),
	retry_of_id UUID NULL REFERENCES continuations (id) ON DELETE SET NULL,
	failure_message TEXT NOT NULL DEFAULT '',
	version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
	lease_owner TEXT NULL,
	lease_expires_at TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	completed_at TIMESTAMPTZ NULL,
	UNIQUE (work_unit_id, attempt),
	CHECK (trigger <> 'RESULT_INCOMPLETE' OR source_result_id IS NOT NULL),
	CHECK (status <> 'RECOVERY_REQUIRED' OR recovery_reason IS NOT NULL),
	CHECK ((strategy IS NULL) = (decision IS NULL)),
	CHECK ((status IN ('COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED')) = (completed_at IS NOT NULL)),
	CHECK (session_confirmed_at IS NULL OR new_session_id IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS continuations_one_active_per_unit ON continuations (work_unit_id)
	WHERE status NOT IN ('COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED');
CREATE INDEX IF NOT EXISTS continuations_project_idx ON continuations (project_id, created_at);
CREATE INDEX IF NOT EXISTS continuations_claimable_idx ON continuations (status, lease_expires_at)
	WHERE status NOT IN ('COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED');

-- Bindings are checked by the database even when service validation is
-- bypassed (WBS 14.91) and are immutable after insert.
CREATE OR REPLACE FUNCTION continuation_binding_guard() RETURNS trigger AS $$
DECLARE d worker_dispatches%ROWTYPE;
DECLARE r worker_results%ROWTYPE;
BEGIN
	IF TG_OP = 'UPDATE' THEN
		IF NEW.id <> OLD.id OR NEW.project_id <> OLD.project_id OR NEW.phase_id <> OLD.phase_id
			OR NEW.work_unit_id <> OLD.work_unit_id OR NEW.runtime_state_id <> OLD.runtime_state_id
			OR NEW.source_dispatch_id <> OLD.source_dispatch_id OR NEW.source_result_id IS DISTINCT FROM OLD.source_result_id
			OR NEW.trigger <> OLD.trigger OR NEW.attempt <> OLD.attempt OR NEW.status_hash <> OLD.status_hash
			OR NEW.captured_status <> OLD.captured_status OR NEW.created_at <> OLD.created_at THEN
			RAISE EXCEPTION 'continuation binding is immutable' USING ERRCODE = 'check_violation';
		END IF;
		IF OLD.status IN ('COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED') AND NEW.status <> OLD.status THEN
			RAISE EXCEPTION 'continuation % is final', OLD.status USING ERRCODE = 'check_violation';
		END IF;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM phases WHERE id = NEW.phase_id AND project_id = NEW.project_id)
		OR NOT EXISTS (SELECT 1 FROM runtime_state WHERE id = NEW.runtime_state_id AND project_id = NEW.project_id) THEN
		RAISE EXCEPTION 'continuation phase/runtime state belongs to another project' USING ERRCODE = 'check_violation';
	END IF;
	SELECT * INTO d FROM worker_dispatches WHERE id = NEW.source_dispatch_id;
	IF NOT FOUND OR d.project_id <> NEW.project_id OR d.work_unit_id IS DISTINCT FROM NEW.work_unit_id
		OR d.phase_id IS DISTINCT FROM NEW.phase_id THEN
		RAISE EXCEPTION 'continuation source dispatch belongs to another project/unit' USING ERRCODE = 'check_violation';
	END IF;
	IF NEW.source_result_id IS NOT NULL THEN
		SELECT * INTO r FROM worker_results WHERE id = NEW.source_result_id;
		IF NOT FOUND OR r.project_id <> NEW.project_id OR r.work_unit_id IS DISTINCT FROM NEW.work_unit_id
			OR r.phase_id IS DISTINCT FROM NEW.phase_id THEN
			RAISE EXCEPTION 'continuation source result belongs to another project/unit' USING ERRCODE = 'check_violation';
		END IF;
	END IF;
	IF NEW.dispatch_id IS NOT NULL THEN
		SELECT * INTO d FROM worker_dispatches WHERE id = NEW.dispatch_id;
		IF NOT FOUND OR d.project_id <> NEW.project_id OR d.work_unit_id IS DISTINCT FROM NEW.work_unit_id THEN
			RAISE EXCEPTION 'continuation dispatch belongs to another project/unit' USING ERRCODE = 'check_violation';
		END IF;
	END IF;
	IF EXISTS (SELECT 1 FROM worker_sessions s WHERE s.project_id <> NEW.project_id
		AND s.id IN (NEW.session_id, NEW.old_session_id, NEW.new_session_id)) THEN
		RAISE EXCEPTION 'continuation session belongs to another project' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS continuations_binding ON continuations;
CREATE TRIGGER continuations_binding BEFORE INSERT OR UPDATE ON continuations
	FOR EACH ROW EXECUTE FUNCTION continuation_binding_guard();
