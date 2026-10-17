-- P18-15 (WBS 18.3 / 18.4 / 18.206): the redacted application
-- configuration recorded at every start (hash + per-section hashes; no
-- secret values), and the reproducibility metadata every execution
-- records at bootstrap.
CREATE TABLE IF NOT EXISTS app_config_snapshots (
	id UUID PRIMARY KEY,
	created_at TIMESTAMPTZ NOT NULL,
	instance_id TEXT NOT NULL CHECK (instance_id <> ''),
	profile TEXT NOT NULL CHECK (profile IN ('development', 'test', 'production')),
	app_version TEXT NOT NULL CHECK (app_version <> ''),
	schema_version INTEGER NOT NULL CHECK (schema_version > 0),
	config_hash TEXT NOT NULL CHECK (config_hash ~ '^[0-9a-f]{64}$'),
	sections JSONB NOT NULL,
	config JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS app_config_snapshots_created_idx ON app_config_snapshots (created_at DESC, id);
CREATE INDEX IF NOT EXISTS app_config_snapshots_hash_idx ON app_config_snapshots (config_hash);
DROP TRIGGER IF EXISTS app_config_snapshots_immutable ON app_config_snapshots;
CREATE TRIGGER app_config_snapshots_immutable BEFORE UPDATE OR DELETE ON app_config_snapshots
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();

ALTER TABLE executions
	ADD COLUMN IF NOT EXISTS app_version TEXT NULL,
	ADD COLUMN IF NOT EXISTS schema_version INTEGER NULL,
	ADD COLUMN IF NOT EXISTS app_config_hash TEXT NULL;
