-- P13-25 (WBS 13.53/13.54): worker acceptance-criterion claims. A claim
-- never verifies a criterion; recognized = the id belongs to the
-- dispatched work unit.
CREATE TABLE IF NOT EXISTS worker_result_acceptance_claims (
	result_id UUID NOT NULL REFERENCES worker_results (id) ON DELETE CASCADE,
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
	criterion_id TEXT NOT NULL CHECK (criterion_id <> ''),
	status TEXT NOT NULL CHECK (status IN ('satisfied', 'unsatisfied', 'not_checked')),
	evidence TEXT NOT NULL DEFAULT '',
	recognized BOOLEAN NOT NULL,
	PRIMARY KEY (result_id, ordinal)
);
DROP TRIGGER IF EXISTS worker_result_claims_immutable ON worker_result_acceptance_claims;
CREATE TRIGGER worker_result_claims_immutable BEFORE UPDATE ON worker_result_acceptance_claims
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
