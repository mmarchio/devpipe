-- P7-15 (WBS 7.23): materialized-file metadata. Each state file carries at
-- most ONE current record of what was last materialized (state_file_id,
-- version, content_hash, path, materialized_at) so drift detection (P7-16)
-- can compare the persisted version against what is on disk. The record is
-- a PROJECTION: it never overrides the persisted version — PostgreSQL stays
-- authoritative. Re-materialization updates the row in place (state_file_id
-- is the primary key), never appending duplicates.
CREATE TABLE IF NOT EXISTS materialized_state_files (
	state_file_id UUID PRIMARY KEY REFERENCES state_files (id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	content_hash TEXT NOT NULL,
	path TEXT NOT NULL,
	materialized_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
