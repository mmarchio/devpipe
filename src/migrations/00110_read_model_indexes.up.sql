-- P17-26 (WBS 17.158–17.160): indexes behind the bounded web read models
-- (dashboard, project list, overview, paginated work units, timelines).
-- Only indexes not already present are added.
CREATE INDEX IF NOT EXISTS work_units_plan_status_idx ON work_units (execution_plan_id, status);
CREATE INDEX IF NOT EXISTS work_unit_dependencies_depends_on_idx ON work_unit_dependencies (depends_on_work_unit_id);
CREATE INDEX IF NOT EXISTS executions_project_status_idx ON executions (project_id, status);
CREATE INDEX IF NOT EXISTS execution_plans_phase_idx ON execution_plans (phase_id, version DESC);
CREATE INDEX IF NOT EXISTS projects_status_idx ON projects (status);
CREATE INDEX IF NOT EXISTS fix_attempts_execution_idx ON fix_attempts (execution_id);
CREATE INDEX IF NOT EXISTS worker_dispatches_unit_idx ON worker_dispatches (work_unit_id, created_at) WHERE work_unit_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS repository_checkpoints_target_idx ON repository_checkpoints (target_id, created_at) WHERE target_id IS NOT NULL;
