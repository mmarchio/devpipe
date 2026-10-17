-- P18-55 (WBS 18.99–18.104, decision U2): raw validation stdout / stderr
-- expire after the retention period. Expiry is the ONLY sanctioned change
-- to a validation command result: stdout / stderr are emptied once,
-- their sha256 recorded and log_expired_at set; outcome, exit code,
-- timing and every other column stay as they were.
ALTER TABLE validation_command_results ADD COLUMN IF NOT EXISTS log_expired_at TIMESTAMPTZ NULL;
ALTER TABLE validation_command_results ADD COLUMN IF NOT EXISTS stdout_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE validation_command_results ADD COLUMN IF NOT EXISTS stderr_sha256 TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS validation_command_results_log_live_idx ON validation_command_results (created_at) WHERE log_expired_at IS NULL;

CREATE OR REPLACE FUNCTION overseer_validation_results_guard() RETURNS trigger AS $$
DECLARE
	probe validation_command_results;
BEGIN
	IF OLD.log_expired_at IS NULL AND NEW.log_expired_at IS NOT NULL AND NEW.stdout = '' AND NEW.stderr = ''
		AND NEW.stdout_sha256 = encode(sha256(convert_to(OLD.stdout, 'UTF8')), 'hex')
		AND NEW.stderr_sha256 = encode(sha256(convert_to(OLD.stderr, 'UTF8')), 'hex') THEN
		probe := NEW;
		probe.stdout := OLD.stdout;
		probe.stderr := OLD.stderr;
		probe.stdout_sha256 := OLD.stdout_sha256;
		probe.stderr_sha256 := OLD.stderr_sha256;
		probe.log_expired_at := NULL;
		IF ROW(probe.*) IS NOT DISTINCT FROM ROW(OLD.*) THEN
			RETURN NEW;
		END IF;
	END IF;
	RAISE EXCEPTION 'validation_command_results rows are immutable' USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS validation_command_results_immutable ON validation_command_results;
CREATE TRIGGER validation_command_results_immutable BEFORE UPDATE ON validation_command_results
	FOR EACH ROW EXECUTE FUNCTION overseer_validation_results_guard();
