-- P18-24 (WBS 18.44–18.46, decision U4): after a restore the instance
-- starts read-only with the scheduler off, even when every startup check
-- passes; an operator reviews the check report and explicitly enables
-- execution (audited RESTORE_ENABLE action). ENABLED is the terminal state.
ALTER TABLE restore_state DROP CONSTRAINT IF EXISTS restore_state_status_check;
ALTER TABLE restore_state ADD CONSTRAINT restore_state_status_check
	CHECK (status IN ('VALIDATION_PENDING', 'VALIDATED', 'FAILED', 'ENABLED'));
ALTER TABLE restore_state ADD COLUMN IF NOT EXISTS enabled_at TIMESTAMPTZ NULL;
ALTER TABLE restore_state ADD COLUMN IF NOT EXISTS enabled_by TEXT NOT NULL DEFAULT '';
ALTER TABLE restore_state ADD COLUMN IF NOT EXISTS enable_reason TEXT NOT NULL DEFAULT '' CHECK (length(enable_reason) <= 2000);
ALTER TABLE restore_state DROP CONSTRAINT IF EXISTS restore_state_enabled_check;
ALTER TABLE restore_state ADD CONSTRAINT restore_state_enabled_check
	CHECK ((status = 'ENABLED') = (enabled_at IS NOT NULL AND enabled_by <> '' AND enable_reason <> ''));

ALTER TABLE audit_events DROP CONSTRAINT IF EXISTS audit_events_action_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_action_check CHECK (action IN ('PROJECT_CREATE', 'PROJECT_UPDATE', 'PROJECT_DELETE',
	'LIFECYCLE_START', 'LIFECYCLE_PAUSE', 'LIFECYCLE_RESUME', 'LIFECYCLE_CANCEL', 'ENGINE_STEP',
	'PHASE_APPEND', 'PHASE_UPDATE', 'WBS_EDIT', 'CONFIG_EDIT', 'ROLE_EDIT', 'WORKFLOW_EDIT',
	'STATE_FILE_EDIT', 'TEMPLATE_EDIT', 'DRIFT_RESOLVE', 'RECOVERY_ACTION', 'WORKER_OPERATE', 'MAINTENANCE',
	'ACKNOWLEDGE_AUDIT_FAILURE', 'RESTORE_ENABLE'));
