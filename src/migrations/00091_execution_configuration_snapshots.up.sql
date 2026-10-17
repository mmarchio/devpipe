-- P15-25 (WBS 15.40–15.42): immutable execution configuration snapshots.
-- canonical is the canonical JSON content; hash = SHA-256(canonical).
CREATE TABLE IF NOT EXISTS execution_configuration_snapshots (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL UNIQUE REFERENCES executions (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	project_version INTEGER NOT NULL CHECK (project_version > 0),
	canonical TEXT NOT NULL,
	hash TEXT NOT NULL CHECK (length(hash) = 64),
	created_at TIMESTAMPTZ NOT NULL
);
DROP TRIGGER IF EXISTS execution_configuration_snapshots_immutable ON execution_configuration_snapshots;
CREATE TRIGGER execution_configuration_snapshots_immutable BEFORE UPDATE ON execution_configuration_snapshots
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();

ALTER TABLE executions
	ADD COLUMN IF NOT EXISTS configuration_snapshot_id UUID NULL
		REFERENCES execution_configuration_snapshots (id) ON DELETE SET NULL;

-- The snapshot reference is set once and never changed.
CREATE OR REPLACE FUNCTION execution_snapshot_ref_guard() RETURNS trigger AS $$
BEGIN
	IF OLD.configuration_snapshot_id IS NOT NULL
		AND NEW.configuration_snapshot_id IS DISTINCT FROM OLD.configuration_snapshot_id THEN
		RAISE EXCEPTION 'execution configuration snapshot is immutable' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS executions_snapshot_ref ON executions;
CREATE TRIGGER executions_snapshot_ref BEFORE UPDATE ON executions
	FOR EACH ROW EXECUTE FUNCTION execution_snapshot_ref_guard();
