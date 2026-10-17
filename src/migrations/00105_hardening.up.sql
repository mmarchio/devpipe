-- P16-62 (WBS 16.87–16.148): final hardening passes (one per execution and
-- finalization generation), duplicate groups per round with their
-- classification / destination / lineage, ordered consolidation operations,
-- the persisted report, and the project's accepted-duplication baseline.
CREATE TABLE IF NOT EXISTS hardening_passes (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	generation INTEGER NOT NULL CHECK (generation > 0),
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'ANALYZING', 'PLANNING', 'EXECUTING', 'VALIDATING', 'DONE',
		'FAILED', 'RECOVERY_REQUIRED')),
	resume_status TEXT NULL,
	round INTEGER NOT NULL DEFAULT 1 CHECK (round > 0),
	baseline_checkpoint_id UUID NULL REFERENCES repository_checkpoints (id) ON DELETE SET NULL,
	baseline_fingerprint TEXT NOT NULL DEFAULT '',
	repo_fingerprint TEXT NOT NULL DEFAULT '',
	recovery_reason TEXT NOT NULL DEFAULT '',
	version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
	started_at TIMESTAMPTZ NOT NULL,
	completed_at TIMESTAMPTZ NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	UNIQUE (execution_id, generation)
);

CREATE TABLE IF NOT EXISTS hardening_groups (
	id UUID PRIMARY KEY,
	pass_id UUID NOT NULL REFERENCES hardening_passes (id) ON DELETE CASCADE,
	round INTEGER NOT NULL CHECK (round > 0),
	group_key TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	members JSONB NOT NULL,
	classification TEXT NOT NULL CHECK (classification IN ('SAFE', 'UNSAFE_IMPORT_CYCLE', 'UNSAFE_ARCHITECTURE',
		'UNSAFE_TYPE_DEPENDENCY', 'UNSAFE_SIDE_EFFECT', 'LOW_VALUE', 'REQUIRES_REVIEW')),
	reason TEXT NOT NULL CHECK (reason <> ''),
	destination JSONB NULL,
	status TEXT NOT NULL CHECK (status IN ('ANALYZED', 'SKIPPED', 'PLANNED', 'IN_PROGRESS', 'CONSOLIDATED', 'FAILED', 'REMAINING')),
	matrix JSONB NOT NULL DEFAULT '[]',
	tests JSONB NOT NULL DEFAULT '[]',
	lineage JSONB NULL,
	checkpoint_id UUID NULL REFERENCES repository_checkpoints (id) ON DELETE SET NULL,
	tests_before INTEGER NOT NULL DEFAULT 0,
	tests_after INTEGER NOT NULL DEFAULT 0,
	refs_migrated INTEGER NOT NULL DEFAULT 0,
	functions_removed INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	UNIQUE (pass_id, round, group_key)
);

CREATE TABLE IF NOT EXISTS hardening_operations (
	id UUID PRIMARY KEY,
	group_id UUID NOT NULL REFERENCES hardening_groups (id) ON DELETE CASCADE,
	seq INTEGER NOT NULL CHECK (seq > 0),
	kind TEXT NOT NULL CHECK (kind IN ('COPY_TO_SHARED', 'TEST_SHARED', 'UPDATE_REFERENCES', 'TEST_CALLERS',
		'CONSOLIDATE_TESTS', 'REMOVE_DUPLICATES', 'VALIDATE', 'CHECKPOINT')),
	package TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'DONE', 'FAILED')),
	evidence JSONB NOT NULL DEFAULT '{}',
	repo_fingerprint TEXT NOT NULL DEFAULT '',
	validation_run_id UUID NULL REFERENCES validation_runs (id) ON DELETE SET NULL,
	started_at TIMESTAMPTZ NULL,
	finished_at TIMESTAMPTZ NULL,
	UNIQUE (group_id, seq)
);

CREATE TABLE IF NOT EXISTS hardening_reports (
	pass_id UUID PRIMARY KEY REFERENCES hardening_passes (id) ON DELETE CASCADE,
	report JSONB NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS hardening_retained_groups (
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	fingerprint TEXT NOT NULL,
	classification TEXT NOT NULL,
	reason TEXT NOT NULL CHECK (reason <> ''),
	recorded_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (project_id, fingerprint)
);
