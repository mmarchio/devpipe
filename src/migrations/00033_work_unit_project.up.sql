-- P6-30 (WBS 6.40): the runtime-state read resolves the work-unit
-- association project-scoped. Persist the owning project on work_units
-- (backfilled from the unit's plan's phase) so a unit's project is
-- answerable without a join.
ALTER TABLE work_units ADD COLUMN project_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';
UPDATE work_units SET project_id = p.project_id
  FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id
  WHERE ep.id = work_units.execution_plan_id;
CREATE INDEX idx_work_units_project_id ON work_units (project_id);
