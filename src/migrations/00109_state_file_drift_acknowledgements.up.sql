-- P17-25 (WBS 17.47): "Ignore" for a drifted state file is an explicit
-- acknowledgement of one filesystem content hash. A later filesystem change
-- (new hash) raises drift again. Acknowledgements are immutable.
CREATE TABLE IF NOT EXISTS state_file_drift_acknowledgements (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	state_file_id UUID NOT NULL REFERENCES state_files (id) ON DELETE CASCADE,
	fs_hash TEXT NOT NULL CHECK (fs_hash <> ''),
	acknowledged_by TEXT NOT NULL CHECK (acknowledged_by <> ''),
	reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (state_file_id, fs_hash)
);
DROP TRIGGER IF EXISTS state_file_drift_ack_immutable ON state_file_drift_acknowledgements;
CREATE TRIGGER state_file_drift_ack_immutable BEFORE UPDATE ON state_file_drift_acknowledgements
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
