-- P18-40 (WBS 18.48 / 18.49): the repository identity an execution works
-- in, recorded once (at bootstrap, or at the first guard check for an
-- execution bootstrapped before this migration) and immutable: work-tree
-- top level, root commit and branch (PLAN Decision 16).
CREATE TABLE IF NOT EXISTS execution_repository_identities (
	execution_id UUID PRIMARY KEY,
	project_id UUID NOT NULL,
	toplevel TEXT NOT NULL,
	root_commit TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL DEFAULT '',
	is_git BOOLEAN NOT NULL,
	source TEXT NOT NULL CHECK (source IN ('BOOTSTRAP', 'FIRST_CHECK')),
	recorded_at TIMESTAMPTZ NOT NULL,
	FOREIGN KEY (execution_id, project_id) REFERENCES executions (id, project_id) ON DELETE CASCADE
);
DROP TRIGGER IF EXISTS execution_repository_identities_immutable ON execution_repository_identities;
CREATE TRIGGER execution_repository_identities_immutable BEFORE UPDATE ON execution_repository_identities
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
