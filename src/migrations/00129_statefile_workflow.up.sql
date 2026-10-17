-- Statefile workflow: command states send their command verbatim (a
-- COMMAND worker prompt has no canonical prompt or compression run), and
-- the check states read the statefiles the team wrote.

ALTER TABLE worker_prompts ALTER COLUMN canonical_prompt_id DROP NOT NULL;
ALTER TABLE worker_prompts ALTER COLUMN run_id DROP NOT NULL;
DO $$
DECLARE c TEXT;
BEGIN
	FOR c IN
		SELECT con.conname FROM pg_constraint con
		WHERE con.conrelid = 'worker_prompts'::regclass AND con.contype = 'c'
	LOOP
		EXECUTE format('ALTER TABLE worker_prompts DROP CONSTRAINT %I', c);
	END LOOP;
END $$;
ALTER TABLE worker_prompts ADD CONSTRAINT worker_prompts_source_check CHECK (source IN ('CANONICAL', 'COMPRESSED', 'COMMAND'));
ALTER TABLE worker_prompts ADD CONSTRAINT worker_prompts_compressed_check CHECK ((source = 'COMPRESSED') = (compressed_prompt_id IS NOT NULL));
ALTER TABLE worker_prompts ADD CONSTRAINT worker_prompts_command_check
	CHECK ((source = 'COMMAND') = (canonical_prompt_id IS NULL AND run_id IS NULL));

-- Step actions of the command and check states.
DO $$
DECLARE c TEXT;
BEGIN
	FOR c IN
		SELECT con.conname FROM pg_constraint con
		JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = ANY (con.conkey)
		WHERE con.conrelid = 'execution_steps'::regclass AND con.contype = 'c' AND att.attname = 'action'
	LOOP
		EXECUTE format('ALTER TABLE execution_steps DROP CONSTRAINT %I', c);
	END LOOP;
END $$;
ALTER TABLE execution_steps ADD CONSTRAINT execution_steps_action_check CHECK (action IN (
	'COMMAND', 'IMPLEMENT_CHECK', 'TEST_CHECK', 'ACCEPTANCE_CHECK', 'ARCHITECT_DISPATCH', 'WBS_VALIDATE',
	'PLAN_DISPATCH', 'PLAN_VALIDATE', 'SELECT_UNIT', 'DISPATCH', 'EVALUATE_RESULT',
	'CONTINUATION', 'RUN_UNIT_VALIDATION', 'UNIT_GATES', 'MORE_WORK', 'PHASE_VALIDATION', 'MORE_PHASES',
	'FINAL_HARDENING', 'HARDENING_FIX', 'FINAL_VALIDATION', 'FINAL_CHECKPOINT', 'CONTROL', 'COMPLETE'));
