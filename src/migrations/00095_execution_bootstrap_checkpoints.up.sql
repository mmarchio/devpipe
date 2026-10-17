-- P15-29 (WBS 15.149): immutable BOOTSTRAP_COMPLETE checkpoints.
CREATE TABLE IF NOT EXISTS execution_bootstrap_checkpoints (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL UNIQUE REFERENCES executions (id) ON DELETE CASCADE,
	stage TEXT NOT NULL CHECK (stage = 'BOOTSTRAP_COMPLETE'),
	artifacts JSONB NOT NULL,
	checkpoint_hash TEXT NOT NULL CHECK (length(checkpoint_hash) = 64),
	created_at TIMESTAMPTZ NOT NULL
);
DROP TRIGGER IF EXISTS execution_bootstrap_checkpoints_immutable ON execution_bootstrap_checkpoints;
CREATE TRIGGER execution_bootstrap_checkpoints_immutable BEFORE UPDATE ON execution_bootstrap_checkpoints
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
