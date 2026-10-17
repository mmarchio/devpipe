-- P15-26 (WBS 15.10): append-only bootstrap stage history.
CREATE TABLE IF NOT EXISTS execution_bootstrap_transitions (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	from_stage TEXT NULL,
	to_stage TEXT NOT NULL CHECK (to_stage IN ('VALIDATING', 'SNAPSHOTTING', 'INITIALIZING_FILES',
		'INITIALIZING_RUNTIME', 'PREPARING_CONTEXT', 'FINALIZING', 'COMPLETE', 'FAILED')),
	status TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (execution_id, sequence)
);
DROP TRIGGER IF EXISTS execution_bootstrap_transitions_immutable ON execution_bootstrap_transitions;
CREATE TRIGGER execution_bootstrap_transitions_immutable BEFORE UPDATE ON execution_bootstrap_transitions
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
