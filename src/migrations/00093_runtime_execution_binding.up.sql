-- P15-27 (WBS 15.55, 15.56): runtime state and transition history reference
-- the execution; the bootstrap entry (outcome BOOTSTRAP) records the
-- initial state with no source state.
ALTER TABLE runtime_state
	ADD COLUMN IF NOT EXISTS execution_id UUID NULL REFERENCES executions (id) ON DELETE SET NULL;
ALTER TABLE state_transition_history
	ADD COLUMN IF NOT EXISTS execution_id UUID NULL REFERENCES executions (id) ON DELETE SET NULL;
ALTER TABLE state_transition_history
	ALTER COLUMN from_state_id DROP NOT NULL;
ALTER TABLE state_transition_history
	ADD CONSTRAINT state_transition_history_bootstrap_from
	CHECK (from_state_id IS NOT NULL OR outcome = 'BOOTSTRAP');
CREATE INDEX IF NOT EXISTS state_transition_history_execution_idx ON state_transition_history (execution_id, seq);
