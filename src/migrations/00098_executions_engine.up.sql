-- P16-55 (WBS 16.2–16.6, 16.29): executions gain the WAITING status, the
-- engine's durable position (current phase / unit / runtime), the driver
-- lease with a fencing epoch, the scheduling timestamp and recovery
-- evidence. WAITING is active: still one active execution per project.
ALTER TABLE executions DROP CONSTRAINT IF EXISTS executions_status_check;
ALTER TABLE executions ADD CONSTRAINT executions_status_check CHECK (status IN ('INITIALIZING', 'READY', 'RUNNING',
	'WAITING', 'PAUSED', 'COMPLETED', 'FAILED', 'CANCELLED', 'RECOVERY_REQUIRED'));
DROP INDEX IF EXISTS executions_one_active_per_project;
CREATE UNIQUE INDEX IF NOT EXISTS executions_one_active_per_project ON executions (project_id)
	WHERE status IN ('INITIALIZING', 'READY', 'RUNNING', 'WAITING', 'PAUSED', 'RECOVERY_REQUIRED');

ALTER TABLE executions
	ADD COLUMN IF NOT EXISTS current_phase_id UUID NULL REFERENCES phases (id) ON DELETE SET NULL,
	ADD COLUMN IF NOT EXISTS current_work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE SET NULL,
	ADD COLUMN IF NOT EXISTS runtime_state_id UUID NULL REFERENCES runtime_state (id) ON DELETE SET NULL,
	ADD COLUMN IF NOT EXISTS driver_owner TEXT NULL,
	ADD COLUMN IF NOT EXISTS driver_expires_at TIMESTAMPTZ NULL,
	ADD COLUMN IF NOT EXISTS driver_epoch BIGINT NOT NULL DEFAULT 0 CHECK (driver_epoch >= 0),
	ADD COLUMN IF NOT EXISTS last_scheduled_at TIMESTAMPTZ NULL,
	ADD COLUMN IF NOT EXISTS wait_reason TEXT NULL,
	ADD COLUMN IF NOT EXISTS recovery_reason TEXT NULL,
	ADD COLUMN IF NOT EXISTS recovery_evidence JSONB NULL,
	ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ NULL,
	ADD COLUMN IF NOT EXISTS finalization_generation INTEGER NOT NULL DEFAULT 1 CHECK (finalization_generation > 0);
CREATE INDEX IF NOT EXISTS executions_schedulable_idx ON executions (last_scheduled_at NULLS FIRST)
	WHERE status IN ('RUNNING', 'WAITING');
