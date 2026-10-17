-- 00045: project planning settings (WBS 8.44). The planning composition
-- reads the project's planning policy and planning granularity from
-- project configuration (WBS 8.16: planning granularity remains project
-- configuration, not an implementation assumption).
ALTER TABLE project_settings
	ADD COLUMN IF NOT EXISTS planning_policy TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS planning_granularity TEXT NOT NULL DEFAULT '';
