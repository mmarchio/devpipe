-- P15-28 (WBS 15.19, 15.80, 15.127): immutable repository baselines
-- captured at snapshot time and re-captured before finalization.
CREATE TABLE IF NOT EXISTS execution_repository_baselines (
	id UUID PRIMARY KEY,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('SNAPSHOT', 'FINAL')),
	working_directory TEXT NOT NULL,
	is_git BOOLEAN NOT NULL,
	head_commit TEXT NULL,
	branch TEXT NULL,
	dirty BOOLEAN NOT NULL,
	fingerprint TEXT NOT NULL DEFAULT '',
	hash TEXT NOT NULL CHECK (length(hash) = 64),
	captured_at TIMESTAMPTZ NOT NULL,
	UNIQUE (execution_id, kind)
);
DROP TRIGGER IF EXISTS execution_repository_baselines_immutable ON execution_repository_baselines;
CREATE TRIGGER execution_repository_baselines_immutable BEFORE UPDATE ON execution_repository_baselines
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
