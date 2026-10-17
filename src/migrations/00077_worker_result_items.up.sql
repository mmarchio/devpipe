-- P13-23 (WBS 13.44): ordered completed / remaining / blocking items.
CREATE TABLE IF NOT EXISTS worker_result_items (
	result_id UUID NOT NULL REFERENCES worker_results (id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('COMPLETED', 'REMAINING', 'BLOCKING')),
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
	text TEXT NOT NULL CHECK (text <> ''),
	PRIMARY KEY (result_id, kind, ordinal)
);
CREATE INDEX IF NOT EXISTS worker_result_items_blocking_idx ON worker_result_items (kind) WHERE kind = 'BLOCKING';
DROP TRIGGER IF EXISTS worker_result_items_immutable ON worker_result_items;
CREATE TRIGGER worker_result_items_immutable BEFORE UPDATE ON worker_result_items
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
