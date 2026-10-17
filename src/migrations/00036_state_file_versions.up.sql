-- P7-06 (WBS 7.5/7.6): append-only content versions for state files. Each
-- version stores the exact content bytes, a deterministic SHA-256 content
-- hash (WBS 7.6), and an approximate token estimate stored as metadata
-- (WBS 7.28). The (state_file_id, version) unique key is the database-level
-- duplicate guard: concurrent writers that compute the same MAX+1 cannot
-- both commit (T7.5.5 DB half). Versions are never rewritten: updates
-- append a new row and advance state_files.current_version in the same
-- transaction.
CREATE TABLE IF NOT EXISTS state_file_versions (
	id UUID PRIMARY KEY,
	state_file_id UUID NOT NULL REFERENCES state_files (id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	token_estimate INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS state_file_versions_file_version_unique
	ON state_file_versions (state_file_id, version);
