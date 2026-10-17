-- P16-63 (WBS 16.156–16.160): the immutable completion record (written
-- once, in the completion transaction) and immutability of a COMPLETED
-- execution row: no column may change, except a reference cleared to NULL
-- by an ON DELETE SET NULL cascade.
CREATE TABLE IF NOT EXISTS execution_completions (
	execution_id UUID PRIMARY KEY REFERENCES executions (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	final_phase_id UUID NULL REFERENCES phases (id) ON DELETE SET NULL,
	hardening_pass_id UUID NULL REFERENCES hardening_passes (id) ON DELETE SET NULL,
	final_validation_run_id UUID NULL REFERENCES validation_runs (id) ON DELETE SET NULL,
	final_checkpoint_id UUID NULL REFERENCES repository_checkpoints (id) ON DELETE SET NULL,
	completed_at TIMESTAMPTZ NOT NULL
);
DROP TRIGGER IF EXISTS execution_completions_immutable ON execution_completions;
CREATE TRIGGER execution_completions_immutable BEFORE UPDATE ON execution_completions
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();

CREATE OR REPLACE FUNCTION execution_completed_guard() RETURNS trigger AS $$
BEGIN
	IF OLD.status = 'COMPLETED' AND EXISTS (
		SELECT 1 FROM jsonb_each(to_jsonb(NEW)) n JOIN jsonb_each(to_jsonb(OLD)) o USING (key)
		WHERE n.value IS DISTINCT FROM o.value AND n.value <> 'null'::jsonb) THEN
		RAISE EXCEPTION 'execution is COMPLETED and immutable' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS executions_completed_guard ON executions;
CREATE TRIGGER executions_completed_guard BEFORE UPDATE ON executions
	FOR EACH ROW EXECUTE FUNCTION execution_completed_guard();
