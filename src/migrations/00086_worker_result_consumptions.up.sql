-- P14-29 (WBS 14.101, 14.102): a WorkerResult is consumed at most once per
-- consumer (continuation / later orchestration); immutable.
CREATE TABLE IF NOT EXISTS worker_result_consumptions (
	result_id UUID NOT NULL REFERENCES worker_results (id) ON DELETE CASCADE,
	consumer TEXT NOT NULL CHECK (consumer <> ''),
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	continuation_id UUID NULL REFERENCES continuations (id) ON DELETE CASCADE,
	consumed_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (result_id, consumer)
);
CREATE INDEX IF NOT EXISTS worker_result_consumptions_continuation_idx ON worker_result_consumptions (continuation_id);
DROP TRIGGER IF EXISTS worker_result_consumptions_immutable ON worker_result_consumptions;
CREATE TRIGGER worker_result_consumptions_immutable BEFORE UPDATE ON worker_result_consumptions
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
