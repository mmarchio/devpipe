-- P18-23 / P18-24 (WBS 18.41–18.46, 18.140): backup records (metadata of
-- logical backups taken from this database) and the restore gate of a
-- database populated by a restore (validation must pass before execution
-- is enabled).
CREATE TABLE IF NOT EXISTS backup_records (
	id UUID PRIMARY KEY,
	created_at TIMESTAMPTZ NOT NULL,
	schema_version INTEGER NOT NULL CHECK (schema_version > 0),
	app_version TEXT NOT NULL CHECK (app_version <> ''),
	location TEXT NOT NULL CHECK (location <> ''),
	reason TEXT NOT NULL DEFAULT '' CHECK (reason IN ('', 'manual', 'pre_migration')),
	tables INTEGER NOT NULL CHECK (tables > 0),
	total_rows BIGINT NOT NULL CHECK (total_rows >= 0),
	manifest_hash TEXT NOT NULL CHECK (manifest_hash ~ '^[0-9a-f]{64}$'),
	restore_verified_at TIMESTAMPTZ NULL
);
CREATE INDEX IF NOT EXISTS backup_records_created_idx ON backup_records (created_at DESC);

CREATE TABLE IF NOT EXISTS restore_state (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	backup_id UUID NOT NULL,
	backup_created_at TIMESTAMPTZ NOT NULL,
	schema_version INTEGER NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('VALIDATION_PENDING', 'VALIDATED', 'FAILED')),
	restored_at TIMESTAMPTZ NOT NULL,
	validated_at TIMESTAMPTZ NULL,
	detail JSONB NOT NULL DEFAULT '{}'
);
