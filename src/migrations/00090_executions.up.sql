-- P15-24 (WBS 15.1–15.5, 15.9, 15.86, 15.145–15.148, 15.156): executions —
-- durable runs of a project's configuration. Project-local numbers are
-- allocated from projects.execution_seq and never reused; at most one
-- active execution per project; the binding is immutable after insert.
ALTER TABLE projects
	ADD COLUMN IF NOT EXISTS execution_seq INTEGER NOT NULL DEFAULT 0 CHECK (execution_seq >= 0);

CREATE TABLE IF NOT EXISTS executions (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	number INTEGER NOT NULL CHECK (number > 0),
	status TEXT NOT NULL CHECK (status IN ('INITIALIZING', 'READY', 'RUNNING', 'PAUSED', 'COMPLETED', 'FAILED',
		'CANCELLED', 'RECOVERY_REQUIRED')),
	generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0),
	current_session_id UUID NULL REFERENCES worker_sessions (id) ON DELETE SET NULL,
	state_machine_id UUID NOT NULL REFERENCES state_machines (id),
	state_machine_version INTEGER NOT NULL CHECK (state_machine_version > 0),
	state_machine_hash TEXT NOT NULL CHECK (state_machine_hash <> ''),
	initial_phase_id UUID NOT NULL REFERENCES phases (id) ON DELETE CASCADE,
	wbs_version INTEGER NOT NULL CHECK (wbs_version >= 0),
	wbs_hash TEXT NOT NULL DEFAULT '',
	plan_id UUID NULL REFERENCES execution_plans (id) ON DELETE SET NULL,
	plan_state TEXT NOT NULL CHECK (plan_state IN ('MISSING', 'STALE', 'INVALID', 'CURRENT')),
	initial_state_key TEXT NOT NULL,
	first_actionable_state_id UUID NULL REFERENCES workflow_states (id),
	first_actionable_state_key TEXT NULL,
	initial_role_key TEXT NOT NULL DEFAULT '',
	initial_work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE SET NULL,
	initial_template_key TEXT NOT NULL DEFAULT '',
	initial_context_snapshot_id UUID NULL REFERENCES context_snapshots (id) ON DELETE SET NULL,
	initial_worker_prompt_id UUID NULL REFERENCES worker_prompts (id) ON DELETE SET NULL,
	initial_cache_key TEXT NULL,
	initial_canonical_key TEXT NULL,
	plan_hash TEXT NOT NULL CHECK (length(plan_hash) = 64),
	bootstrap_stage TEXT NOT NULL CHECK (bootstrap_stage IN ('VALIDATING', 'SNAPSHOTTING', 'INITIALIZING_FILES',
		'INITIALIZING_RUNTIME', 'PREPARING_CONTEXT', 'FINALIZING', 'COMPLETE', 'FAILED')),
	failed_stage TEXT NULL CHECK (failed_stage IN ('VALIDATING', 'SNAPSHOTTING', 'INITIALIZING_FILES',
		'INITIALIZING_RUNTIME', 'PREPARING_CONTEXT', 'FINALIZING')),
	failure_code TEXT NULL,
	failure_class TEXT NULL CHECK (failure_class IN ('CONFIGURATION', 'RECOVERABLE', 'INFRASTRUCTURE')),
	failure_reason TEXT NOT NULL DEFAULT '',
	claim_owner TEXT NULL,
	claim_expires_at TIMESTAMPTZ NULL,
	started_at TIMESTAMPTZ NOT NULL,
	ready_at TIMESTAMPTZ NULL,
	running_at TIMESTAMPTZ NULL,
	completed_at TIMESTAMPTZ NULL,
	failed_at TIMESTAMPTZ NULL,
	cancelled_at TIMESTAMPTZ NULL,
	version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	UNIQUE (project_id, number),
	CHECK (bootstrap_stage <> 'FAILED' OR failed_stage IS NOT NULL),
	CHECK ((status = 'FAILED') = (failed_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS executions_one_active_per_project ON executions (project_id)
	WHERE status IN ('INITIALIZING', 'READY', 'RUNNING', 'PAUSED', 'RECOVERY_REQUIRED');
CREATE INDEX IF NOT EXISTS executions_project_idx ON executions (project_id, number DESC);
CREATE INDEX IF NOT EXISTS executions_recoverable_idx ON executions (status, claim_expires_at)
	WHERE status IN ('INITIALIZING', 'READY');

-- Binding and numbering are immutable; a final execution never reopens;
-- the initial phase belongs to the project (WBS 15.3).
CREATE OR REPLACE FUNCTION execution_binding_guard() RETURNS trigger AS $$
BEGIN
	IF TG_OP = 'UPDATE' THEN
		IF NEW.id <> OLD.id OR NEW.project_id <> OLD.project_id OR NEW.number <> OLD.number
			OR NEW.state_machine_id <> OLD.state_machine_id OR NEW.state_machine_version <> OLD.state_machine_version
			OR NEW.state_machine_hash <> OLD.state_machine_hash OR NEW.initial_phase_id <> OLD.initial_phase_id
			OR NEW.wbs_version <> OLD.wbs_version OR NEW.wbs_hash <> OLD.wbs_hash
			OR NEW.plan_id IS DISTINCT FROM OLD.plan_id AND OLD.plan_id IS NOT NULL AND NEW.plan_id IS NOT NULL
			OR NEW.plan_hash <> OLD.plan_hash OR NEW.started_at <> OLD.started_at OR NEW.created_at <> OLD.created_at THEN
			RAISE EXCEPTION 'execution binding is immutable' USING ERRCODE = 'check_violation';
		END IF;
		IF OLD.status IN ('COMPLETED', 'FAILED', 'CANCELLED') AND NEW.status <> OLD.status THEN
			RAISE EXCEPTION 'execution % is final', OLD.status USING ERRCODE = 'check_violation';
		END IF;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM phases WHERE id = NEW.initial_phase_id AND project_id = NEW.project_id) THEN
		RAISE EXCEPTION 'execution initial phase belongs to another project' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS executions_binding ON executions;
CREATE TRIGGER executions_binding BEFORE INSERT OR UPDATE ON executions
	FOR EACH ROW EXECUTE FUNCTION execution_binding_guard();
