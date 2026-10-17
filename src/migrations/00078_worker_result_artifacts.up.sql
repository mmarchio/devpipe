-- P13-24 (WBS 13.27/13.44): worker-reported artifact claims (never
-- filesystem observations).
CREATE TABLE IF NOT EXISTS worker_result_artifacts (
	result_id UUID NOT NULL REFERENCES worker_results (id) ON DELETE CASCADE,
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
	path TEXT NOT NULL CHECK (path <> ''),
	action TEXT NOT NULL CHECK (action IN ('created', 'modified', 'deleted', 'renamed')),
	previous_path TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (result_id, ordinal),
	CHECK ((action = 'renamed') = (previous_path <> ''))
);
DROP TRIGGER IF EXISTS worker_result_artifacts_immutable ON worker_result_artifacts;
CREATE TRIGGER worker_result_artifacts_immutable BEFORE UPDATE ON worker_result_artifacts
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
