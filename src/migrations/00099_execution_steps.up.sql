-- P16-56 (WBS 16.8, 16.83): durable step records. An INTENT row is
-- committed before any external action; its result is recorded on the same
-- row. The idempotency key (execution, runtime position, action, attempt)
-- makes a repeated step detectable. Finished rows are immutable.
CREATE TABLE IF NOT EXISTS execution_steps (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	seq BIGINT NOT NULL CHECK (seq > 0),
	action TEXT NOT NULL CHECK (action IN ('PLAN_DISPATCH', 'PLAN_VALIDATE', 'SELECT_UNIT', 'DISPATCH', 'EVALUATE_RESULT',
		'CONTINUATION', 'RUN_UNIT_VALIDATION', 'UNIT_GATES', 'MORE_WORK', 'PHASE_VALIDATION', 'MORE_PHASES',
		'FINAL_HARDENING', 'HARDENING_FIX', 'FINAL_VALIDATION', 'FINAL_CHECKPOINT', 'CONTROL', 'COMPLETE')),
	state_key TEXT NOT NULL,
	runtime_version INTEGER NOT NULL CHECK (runtime_version >= 0),
	attempt INTEGER NOT NULL DEFAULT 1 CHECK (attempt > 0),
	idempotency_key TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL CHECK (status IN ('INTENT', 'APPLIED', 'FAILED', 'ABANDONED')),
	phase_id UUID NULL REFERENCES phases (id) ON DELETE SET NULL,
	work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE SET NULL,
	dispatch_id UUID NULL REFERENCES worker_dispatches (id) ON DELETE SET NULL,
	context_snapshot_id UUID NULL REFERENCES context_snapshots (id) ON DELETE SET NULL,
	worker_prompt_id UUID NULL REFERENCES worker_prompts (id) ON DELETE SET NULL,
	validation_run_id UUID NULL,
	checkpoint_id UUID NULL,
	plan_id UUID NULL REFERENCES execution_plans (id) ON DELETE SET NULL,
	continuation_id UUID NULL REFERENCES continuations (id) ON DELETE SET NULL,
	outcome TEXT NULL,
	detail JSONB NOT NULL DEFAULT '{}',
	error TEXT NOT NULL DEFAULT '',
	intent_at TIMESTAMPTZ NOT NULL,
	finished_at TIMESTAMPTZ NULL,
	UNIQUE (execution_id, seq),
	CHECK ((status = 'INTENT') = (finished_at IS NULL))
);
CREATE INDEX IF NOT EXISTS execution_steps_intent_idx ON execution_steps (execution_id) WHERE status = 'INTENT';

CREATE OR REPLACE FUNCTION execution_step_guard() RETURNS trigger AS $$
BEGIN
	IF OLD.status <> 'INTENT' THEN
		RAISE EXCEPTION 'execution step % is final', OLD.status USING ERRCODE = 'check_violation';
	END IF;
	IF NEW.id <> OLD.id OR NEW.execution_id <> OLD.execution_id OR NEW.seq <> OLD.seq OR NEW.action <> OLD.action
		OR NEW.idempotency_key <> OLD.idempotency_key OR NEW.intent_at <> OLD.intent_at THEN
		RAISE EXCEPTION 'execution step identity is immutable' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS execution_steps_guard ON execution_steps;
CREATE TRIGGER execution_steps_guard BEFORE UPDATE ON execution_steps
	FOR EACH ROW EXECUTE FUNCTION execution_step_guard();
