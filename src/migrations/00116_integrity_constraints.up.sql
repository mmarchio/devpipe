-- P18-39 (WBS 18.38): database-level enforcement of critical workflow
-- invariants that were previously guaranteed only by application code:
-- every project-scoped execution record references an existing parent,
-- and a child row's project_id always equals its parent's (ownership —
-- no record can point into another project).
--
-- Foreign keys are added NOT VALID: they are enforced for every new or
-- changed row, but existing rows are not re-checked here, so upgrading a
-- database that already holds an inconsistency cannot fail mid-migration.
-- Such rows are reported by the read-only invariant scanner (WBS 18.39),
-- never repaired automatically (WBS 18.181). ON DELETE SET NULL with a
-- column list needs PostgreSQL ≥ 15; the supported minimum is PostgreSQL
-- 17, checked at startup (database.CheckServerVersion).

-- Parent keys for composite (id, project_id) ownership references.
ALTER TABLE executions DROP CONSTRAINT IF EXISTS executions_id_project_key;
ALTER TABLE executions ADD CONSTRAINT executions_id_project_key UNIQUE (id, project_id);
ALTER TABLE phases DROP CONSTRAINT IF EXISTS phases_id_project_key;
ALTER TABLE phases ADD CONSTRAINT phases_id_project_key UNIQUE (id, project_id);
ALTER TABLE work_units DROP CONSTRAINT IF EXISTS work_units_id_project_key;
ALTER TABLE work_units ADD CONSTRAINT work_units_id_project_key UNIQUE (id, project_id);
ALTER TABLE worker_dispatches DROP CONSTRAINT IF EXISTS worker_dispatches_id_project_key;
ALTER TABLE worker_dispatches ADD CONSTRAINT worker_dispatches_id_project_key UNIQUE (id, project_id);

-- work_units.project_id (00033) had no foreign key at all.
ALTER TABLE work_units DROP CONSTRAINT IF EXISTS work_units_project_fk;
ALTER TABLE work_units ADD CONSTRAINT work_units_project_fk FOREIGN KEY (project_id)
	REFERENCES projects (id) ON DELETE CASCADE NOT VALID;

-- A work unit belongs to the project of its plan's phase. A project_id
-- left at the 00033 column default (the zero UUID) is derived from the
-- plan, exactly as the repository does (P6-30); an explicit different
-- project is rejected.
CREATE OR REPLACE FUNCTION work_units_ownership_guard() RETURNS trigger AS $$
DECLARE
	owner UUID;
BEGIN
	SELECT p.project_id INTO owner FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE ep.id = NEW.execution_plan_id;
	IF NEW.project_id = '00000000-0000-0000-0000-000000000000' AND owner IS NOT NULL THEN
		NEW.project_id := owner;
	END IF;
	IF owner IS NOT NULL AND owner IS DISTINCT FROM NEW.project_id THEN
		RAISE EXCEPTION 'work unit project % does not own its plan (project %)', NEW.project_id, owner
			USING ERRCODE = 'foreign_key_violation';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS work_units_ownership ON work_units;
CREATE TRIGGER work_units_ownership BEFORE INSERT OR UPDATE OF project_id, execution_plan_id ON work_units
	FOR EACH ROW EXECUTE FUNCTION work_units_ownership_guard();

-- Worker dispatches: unit / phase in the same project.
ALTER TABLE worker_dispatches DROP CONSTRAINT IF EXISTS worker_dispatches_unit_owner_fk;
ALTER TABLE worker_dispatches ADD CONSTRAINT worker_dispatches_unit_owner_fk FOREIGN KEY (work_unit_id, project_id)
	REFERENCES work_units (id, project_id) ON DELETE SET NULL (work_unit_id) NOT VALID;
ALTER TABLE worker_dispatches DROP CONSTRAINT IF EXISTS worker_dispatches_phase_owner_fk;
ALTER TABLE worker_dispatches ADD CONSTRAINT worker_dispatches_phase_owner_fk FOREIGN KEY (phase_id, project_id)
	REFERENCES phases (id, project_id) ON DELETE SET NULL (phase_id) NOT VALID;

-- Worker results: dispatch / session / unit / phase in the same project.
ALTER TABLE worker_results DROP CONSTRAINT IF EXISTS worker_results_dispatch_owner_fk;
ALTER TABLE worker_results ADD CONSTRAINT worker_results_dispatch_owner_fk FOREIGN KEY (dispatch_id, project_id)
	REFERENCES worker_dispatches (id, project_id) ON DELETE CASCADE NOT VALID;
ALTER TABLE worker_results DROP CONSTRAINT IF EXISTS worker_results_session_owner_fk;
ALTER TABLE worker_results ADD CONSTRAINT worker_results_session_owner_fk FOREIGN KEY (session_id, project_id)
	REFERENCES worker_sessions (id, project_id) ON DELETE CASCADE NOT VALID;
ALTER TABLE worker_results DROP CONSTRAINT IF EXISTS worker_results_unit_owner_fk;
ALTER TABLE worker_results ADD CONSTRAINT worker_results_unit_owner_fk FOREIGN KEY (work_unit_id, project_id)
	REFERENCES work_units (id, project_id) ON DELETE SET NULL (work_unit_id) NOT VALID;
ALTER TABLE worker_results DROP CONSTRAINT IF EXISTS worker_results_phase_owner_fk;
ALTER TABLE worker_results ADD CONSTRAINT worker_results_phase_owner_fk FOREIGN KEY (phase_id, project_id)
	REFERENCES phases (id, project_id) ON DELETE SET NULL (phase_id) NOT VALID;

-- Execution steps: execution / unit / phase in the same project; the
-- validation run and checkpoint they record must exist.
ALTER TABLE execution_steps DROP CONSTRAINT IF EXISTS execution_steps_execution_owner_fk;
ALTER TABLE execution_steps ADD CONSTRAINT execution_steps_execution_owner_fk FOREIGN KEY (execution_id, project_id)
	REFERENCES executions (id, project_id) ON DELETE CASCADE NOT VALID;
ALTER TABLE execution_steps DROP CONSTRAINT IF EXISTS execution_steps_unit_owner_fk;
ALTER TABLE execution_steps ADD CONSTRAINT execution_steps_unit_owner_fk FOREIGN KEY (work_unit_id, project_id)
	REFERENCES work_units (id, project_id) ON DELETE SET NULL (work_unit_id) NOT VALID;
ALTER TABLE execution_steps DROP CONSTRAINT IF EXISTS execution_steps_phase_owner_fk;
ALTER TABLE execution_steps ADD CONSTRAINT execution_steps_phase_owner_fk FOREIGN KEY (phase_id, project_id)
	REFERENCES phases (id, project_id) ON DELETE SET NULL (phase_id) NOT VALID;
ALTER TABLE execution_steps DROP CONSTRAINT IF EXISTS execution_steps_validation_run_fk;
ALTER TABLE execution_steps ADD CONSTRAINT execution_steps_validation_run_fk FOREIGN KEY (validation_run_id)
	REFERENCES validation_runs (id) ON DELETE SET NULL NOT VALID;
ALTER TABLE execution_steps DROP CONSTRAINT IF EXISTS execution_steps_checkpoint_fk;
ALTER TABLE execution_steps ADD CONSTRAINT execution_steps_checkpoint_fk FOREIGN KEY (checkpoint_id)
	REFERENCES repository_checkpoints (id) ON DELETE SET NULL NOT VALID;

-- Execution events, validation runs, checkpoints: execution (and unit)
-- in the same project.
ALTER TABLE execution_events DROP CONSTRAINT IF EXISTS execution_events_execution_owner_fk;
ALTER TABLE execution_events ADD CONSTRAINT execution_events_execution_owner_fk FOREIGN KEY (execution_id, project_id)
	REFERENCES executions (id, project_id) ON DELETE CASCADE NOT VALID;
ALTER TABLE validation_runs DROP CONSTRAINT IF EXISTS validation_runs_execution_owner_fk;
ALTER TABLE validation_runs ADD CONSTRAINT validation_runs_execution_owner_fk FOREIGN KEY (execution_id, project_id)
	REFERENCES executions (id, project_id) ON DELETE CASCADE NOT VALID;
ALTER TABLE validation_runs DROP CONSTRAINT IF EXISTS validation_runs_unit_owner_fk;
ALTER TABLE validation_runs ADD CONSTRAINT validation_runs_unit_owner_fk FOREIGN KEY (work_unit_id, project_id)
	REFERENCES work_units (id, project_id) ON DELETE SET NULL (work_unit_id) NOT VALID;
ALTER TABLE repository_checkpoints DROP CONSTRAINT IF EXISTS repository_checkpoints_execution_owner_fk;
ALTER TABLE repository_checkpoints ADD CONSTRAINT repository_checkpoints_execution_owner_fk FOREIGN KEY (execution_id, project_id)
	REFERENCES executions (id, project_id) ON DELETE CASCADE NOT VALID;

-- Continuations: unit in the same project.
ALTER TABLE continuations DROP CONSTRAINT IF EXISTS continuations_unit_owner_fk;
ALTER TABLE continuations ADD CONSTRAINT continuations_unit_owner_fk FOREIGN KEY (work_unit_id, project_id)
	REFERENCES work_units (id, project_id) ON DELETE CASCADE NOT VALID;
