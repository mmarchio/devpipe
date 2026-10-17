-- P16-60 (WBS 16.44–16.52): deterministic validation runs and their
-- command results (immutable once finished), operator approvals of manual
-- acceptance criteria, and bounded fix attempts linked to the failed run.
CREATE TABLE IF NOT EXISTS validation_runs (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	scope TEXT NOT NULL CHECK (scope IN ('UNIT', 'PHASE', 'FINAL', 'HARDENING')),
	work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE SET NULL,
	phase_id UUID NULL REFERENCES phases (id) ON DELETE SET NULL,
	target_id UUID NULL,
	label TEXT NOT NULL DEFAULT '',
	attempt INTEGER NOT NULL DEFAULT 1 CHECK (attempt > 0),
	status TEXT NOT NULL CHECK (status IN ('RUNNING', 'PASS', 'FAIL', 'INFRA_ERROR', 'NOT_CONFIGURED', 'ABANDONED')),
	cwd TEXT NOT NULL,
	acceptance JSONB NULL,
	started_at TIMESTAMPTZ NOT NULL,
	finished_at TIMESTAMPTZ NULL,
	CHECK ((status = 'RUNNING') = (finished_at IS NULL))
);
CREATE INDEX IF NOT EXISTS validation_runs_execution_idx ON validation_runs (execution_id, started_at);
CREATE INDEX IF NOT EXISTS validation_runs_unit_idx ON validation_runs (work_unit_id, started_at);
CREATE INDEX IF NOT EXISTS validation_runs_running_idx ON validation_runs (execution_id) WHERE status = 'RUNNING';
CREATE OR REPLACE FUNCTION validation_run_guard() RETURNS trigger AS $$
BEGIN
	IF OLD.status <> 'RUNNING' THEN
		RAISE EXCEPTION 'validation run % is final', OLD.status USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS validation_runs_guard ON validation_runs;
CREATE TRIGGER validation_runs_guard BEFORE UPDATE ON validation_runs
	FOR EACH ROW EXECUTE FUNCTION validation_run_guard();

ALTER TABLE work_units DROP CONSTRAINT IF EXISTS work_units_done_validation_fk;
ALTER TABLE work_units ADD CONSTRAINT work_units_done_validation_fk
	FOREIGN KEY (done_validation_run_id) REFERENCES validation_runs (id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS validation_command_results (
	id UUID PRIMARY KEY,
	run_id UUID NOT NULL REFERENCES validation_runs (id) ON DELETE CASCADE,
	ord INTEGER NOT NULL CHECK (ord > 0),
	name TEXT NOT NULL DEFAULT '',
	program TEXT NOT NULL,
	args JSONB NOT NULL DEFAULT '[]',
	cwd TEXT NOT NULL,
	exit_code INTEGER NULL,
	stdout TEXT NOT NULL DEFAULT '',
	stderr TEXT NOT NULL DEFAULT '',
	duration_ms BIGINT NOT NULL CHECK (duration_ms >= 0),
	timeout_ms BIGINT NOT NULL CHECK (timeout_ms >= 0),
	timed_out BOOLEAN NOT NULL DEFAULT false,
	outcome TEXT NOT NULL CHECK (outcome IN ('PASS', 'FAIL', 'INFRA_ERROR')),
	infra_reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (run_id, ord)
);
DROP TRIGGER IF EXISTS validation_command_results_immutable ON validation_command_results;
CREATE TRIGGER validation_command_results_immutable BEFORE UPDATE ON validation_command_results
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();

CREATE TABLE IF NOT EXISTS acceptance_approvals (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	work_unit_id UUID NOT NULL REFERENCES work_units (id) ON DELETE CASCADE,
	check_index INTEGER NOT NULL CHECK (check_index >= 0),
	approved_by TEXT NOT NULL CHECK (approved_by <> ''),
	note TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (work_unit_id, check_index)
);

CREATE TABLE IF NOT EXISTS fix_attempts (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	target_kind TEXT NOT NULL CHECK (target_kind IN ('UNIT', 'HARDENING')),
	target_id UUID NOT NULL,
	failed_run_id UUID NULL REFERENCES validation_runs (id) ON DELETE SET NULL,
	attempt INTEGER NOT NULL CHECK (attempt > 0),
	dispatch_id UUID NULL REFERENCES worker_dispatches (id) ON DELETE SET NULL,
	evidence JSONB NOT NULL DEFAULT '{}',
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (target_id, attempt)
);
CREATE INDEX IF NOT EXISTS fix_attempts_execution_idx ON fix_attempts (execution_id, created_at);
