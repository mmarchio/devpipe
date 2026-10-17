-- Statefile verification: the MD5 of each statefile the overseer last
-- verified after the team wrote it, so a later verification of an
-- overwritten file can tell whether its content changed.

CREATE TABLE IF NOT EXISTS statefile_hashes (
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	path TEXT NOT NULL CHECK (path <> ''),
	md5 TEXT NOT NULL CHECK (md5 ~ '^[0-9a-f]{32}$'),
	verified_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (execution_id, path)
);
