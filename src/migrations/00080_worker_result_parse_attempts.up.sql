-- P13-26 (WBS 13.45/13.47/13.48): every parse attempt, accepted or not.
-- The raw content is not duplicated (Phase 12 stores it); the attempt
-- keeps hashes, classification, field errors and the binding snapshot used
-- for compliance metrics (WBS 13.63). Append-only.
CREATE TABLE IF NOT EXISTS worker_result_parse_attempts (
	id UUID PRIMARY KEY,
	dispatch_id UUID NOT NULL REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	raw_response_hash TEXT NOT NULL,
	outcome TEXT NOT NULL CHECK (outcome IN ('ACCEPTED', 'REJECTED', 'DUPLICATE')),
	error_code TEXT NOT NULL DEFAULT '',
	validation_errors JSONB NOT NULL DEFAULT '[]'::jsonb,
	result_status TEXT NOT NULL DEFAULT '',
	normalized_hash TEXT NOT NULL DEFAULT '',
	result_id UUID NULL REFERENCES worker_results (id) ON DELETE CASCADE,
	differs_from_accepted BOOLEAN NOT NULL DEFAULT false,
	parse_mode TEXT NOT NULL,
	repair_mode TEXT NOT NULL,
	repaired BOOLEAN NOT NULL,
	repair_method TEXT NULL,
	parser_version TEXT NOT NULL,
	reparse BOOLEAN NOT NULL DEFAULT false,
	role_key TEXT NOT NULL DEFAULT '',
	state_key TEXT NOT NULL DEFAULT '',
	worker_provider TEXT NOT NULL DEFAULT '',
	worker_model TEXT NOT NULL DEFAULT '',
	started_at TIMESTAMPTZ NOT NULL,
	completed_at TIMESTAMPTZ NOT NULL,
	CHECK ((outcome = 'REJECTED') = (error_code <> ''))
);
CREATE INDEX IF NOT EXISTS worker_result_attempts_project_idx ON worker_result_parse_attempts (project_id, started_at);
CREATE INDEX IF NOT EXISTS worker_result_attempts_dispatch_idx ON worker_result_parse_attempts (dispatch_id, started_at);
DROP TRIGGER IF EXISTS worker_result_attempts_immutable ON worker_result_parse_attempts;
CREATE TRIGGER worker_result_attempts_immutable BEFORE UPDATE ON worker_result_parse_attempts
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
