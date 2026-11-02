-- MVP / FULL task scope. The planner categorizes every task (work_units.
-- scope; units planned before this count as MVP). A project selects the
-- tasks the orchestrator works on (project_task_scope; no row = FULL,
-- i.e. MVP and FULL tasks).
ALTER TABLE work_units ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'MVP';
ALTER TABLE work_units DROP CONSTRAINT IF EXISTS work_units_scope_check;
ALTER TABLE work_units ADD CONSTRAINT work_units_scope_check CHECK (scope IN ('MVP', 'FULL'));

CREATE TABLE IF NOT EXISTS project_task_scope (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	scope TEXT NOT NULL CHECK (scope IN ('MVP', 'FULL')),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- P16-59 guard, scope-aware: with MVP selected, a phase may become DONE
-- while FULL units that never started remain (they are deferred, not
-- abandoned); every other unit of the current plan must be DONE.
CREATE OR REPLACE FUNCTION phase_done_guard() RETURNS trigger AS $$
DECLARE
	cur UUID;
	mvp_only BOOLEAN;
BEGIN
	IF NEW.status = 'DONE' AND OLD.status IS DISTINCT FROM 'DONE'
		AND EXISTS (SELECT 1 FROM executions WHERE project_id = NEW.project_id) THEN
		SELECT id INTO cur FROM execution_plans
			WHERE phase_id = NEW.id AND status <> 'SUPERSEDED'
			ORDER BY version DESC LIMIT 1;
		mvp_only := EXISTS (SELECT 1 FROM project_task_scope WHERE project_id = NEW.project_id AND scope = 'MVP');
		IF cur IS NOT NULL AND EXISTS (SELECT 1 FROM work_units WHERE execution_plan_id = cur AND status <> 'DONE'
			AND NOT (mvp_only AND scope = 'FULL' AND status IN ('PENDING', 'READY', 'FAILED', 'BLOCKED'))) THEN
			RAISE EXCEPTION 'phase % cannot be DONE: its current plan has non-DONE units', NEW.id
				USING ERRCODE = 'check_violation';
		END IF;
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
