-- P16-61 (WBS 16.54–16.57, 16.80, 16.87, 16.133, 16.143, 16.154):
-- overseer-owned git checkpoints. The key is unique, so a crash after the
-- commit is reconciled (git trailer lookup) instead of committing twice.
CREATE TABLE IF NOT EXISTS repository_checkpoints (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('UNIT', 'HARDENING_BASELINE', 'HARDENING_GROUP', 'FINAL')),
	target_id UUID NULL,
	checkpoint_key TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'CREATED', 'FAILED', 'SKIPPED')),
	commit_sha TEXT NULL,
	parent_sha TEXT NULL,
	tree_sha TEXT NULL,
	validation_run_id UUID NULL REFERENCES validation_runs (id) ON DELETE SET NULL,
	error TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	completed_at TIMESTAMPTZ NULL,
	CHECK (status <> 'CREATED' OR (commit_sha IS NOT NULL AND tree_sha IS NOT NULL)),
	CHECK ((status = 'PENDING') = (completed_at IS NULL))
);
CREATE INDEX IF NOT EXISTS repository_checkpoints_execution_idx ON repository_checkpoints (execution_id, created_at);
CREATE OR REPLACE FUNCTION repository_checkpoint_guard() RETURNS trigger AS $$
BEGIN
	IF OLD.status <> 'PENDING' THEN
		RAISE EXCEPTION 'repository checkpoint % is final', OLD.status USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS repository_checkpoints_guard ON repository_checkpoints;
CREATE TRIGGER repository_checkpoints_guard BEFORE UPDATE ON repository_checkpoints
	FOR EACH ROW EXECUTE FUNCTION repository_checkpoint_guard();
