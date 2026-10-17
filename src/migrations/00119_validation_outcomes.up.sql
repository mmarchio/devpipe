-- P18-47 (WBS 18.63–18.66): distinct per-command validation outcomes —
-- TIMEOUT (deadline; process group killed), EXECUTION_FAILURE (could not
-- start / killed from outside) and CANCELLED (stopped by the overseer;
-- the run is ABANDONED, never PASS / FAIL) alongside PASS / FAIL /
-- INFRA_ERROR. Existing rows keep their values.
ALTER TABLE validation_command_results DROP CONSTRAINT IF EXISTS validation_command_results_outcome_check;
ALTER TABLE validation_command_results ADD CONSTRAINT validation_command_results_outcome_check
	CHECK (outcome IN ('PASS', 'FAIL', 'TIMEOUT', 'EXECUTION_FAILURE', 'INFRA_ERROR', 'CANCELLED'));
