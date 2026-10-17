-- P13-22 (WBS 13.43): accepted worker results — worker CLAIMS, never
-- authoritative state. Binding columns are copied from the Phase 12
-- dispatch (composite FK keeps the project consistent); one accepted
-- result per dispatch; immutable (a reparse never overwrites, WBS 13.47).
CREATE TABLE IF NOT EXISTS worker_results (
	id UUID PRIMARY KEY,
	dispatch_id UUID NOT NULL UNIQUE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	session_id UUID NOT NULL,
	request_id UUID NOT NULL,
	phase_id UUID NULL,
	work_unit_id UUID NULL,
	state_key TEXT NOT NULL,
	role_key TEXT NOT NULL,
	protocol_major INTEGER NOT NULL CHECK (protocol_major > 0),
	protocol_minor INTEGER NOT NULL CHECK (protocol_minor >= 0),
	status TEXT NOT NULL CHECK (status IN ('complete', 'incomplete', 'blocked', 'failed')),
	summary TEXT NOT NULL CHECK (summary <> ''),
	next_action TEXT NOT NULL,
	test_reported_run BOOLEAN NULL,
	test_reported_passed BOOLEAN NULL,
	test_summary TEXT NULL,
	raw_response_hash TEXT NOT NULL CHECK (length(raw_response_hash) = 64),
	normalized_hash TEXT NOT NULL CHECK (length(normalized_hash) = 64),
	parsed_hash TEXT NOT NULL,
	parse_mode TEXT NOT NULL CHECK (parse_mode IN ('STRICT', 'TOLERANT')),
	repair_mode TEXT NOT NULL,
	extraction TEXT NOT NULL CHECK (extraction IN ('RAW_JSON', 'FENCED_JSON')),
	repaired BOOLEAN NOT NULL,
	repair_method TEXT NULL,
	parser_version TEXT NOT NULL,
	parsed_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	FOREIGN KEY (dispatch_id) REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	CHECK (repaired = (repair_method IS NOT NULL)),
	CHECK (test_reported_passed IS NULL OR test_reported_run)
);
CREATE INDEX IF NOT EXISTS worker_results_project_idx ON worker_results (project_id, created_at);
CREATE INDEX IF NOT EXISTS worker_results_status_idx ON worker_results (project_id, status);
CREATE INDEX IF NOT EXISTS worker_results_unit_idx ON worker_results (work_unit_id);
DROP TRIGGER IF EXISTS worker_results_immutable ON worker_results;
CREATE TRIGGER worker_results_immutable BEFORE UPDATE ON worker_results
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
-- The binding must equal the dispatch's (the worker cannot rebind).
CREATE OR REPLACE FUNCTION worker_result_binding_guard() RETURNS trigger AS $$
DECLARE d worker_dispatches%ROWTYPE;
BEGIN
	SELECT * INTO d FROM worker_dispatches WHERE id = NEW.dispatch_id;
	IF NOT FOUND OR d.project_id <> NEW.project_id OR d.session_id <> NEW.session_id OR d.request_id <> NEW.request_id
		OR d.phase_id IS DISTINCT FROM NEW.phase_id OR d.work_unit_id IS DISTINCT FROM NEW.work_unit_id
		OR d.state_key <> NEW.state_key OR d.role_key <> NEW.role_key THEN
		RAISE EXCEPTION 'worker result binding does not match its dispatch' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS worker_results_binding ON worker_results;
CREATE TRIGGER worker_results_binding BEFORE INSERT ON worker_results
	FOR EACH ROW EXECUTE FUNCTION worker_result_binding_guard();
