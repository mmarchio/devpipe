-- P7-18 (WBS 7.31/7.32): execution-context snapshot persistence. The
-- snapshot (context_snapshots) is the immutable record of one exact
-- runtime position: the runtime row id + version, the bound machine
-- version, the current state key, the role snapshot (serialized), and the
-- created-at instant. One row per creation — append-only; nothing here is
-- updated after creation (WBS 7.31 "persist snapshot").
--
-- context_snapshot_files pins each captured state file at its exact
-- immutable version: the version row's hash + the exact content bytes are
-- duplicated (WBS 7.30 — reconstruction is trivially guaranteed because
-- the pinned version's bytes are carried verbatim), and the scope is
-- retained. One row per snapshot per file; ordered by type_key at read
-- time.
CREATE TABLE IF NOT EXISTS context_snapshots (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	phase_id UUID NULL,
	work_unit_id UUID NULL,
	runtime_state_id UUID NOT NULL,
	runtime_state_version INTEGER NOT NULL CHECK (runtime_state_version > 0),
	state_machine_id UUID NOT NULL,
	state_machine_version INTEGER NOT NULL CHECK (state_machine_version > 0),
	state_key TEXT NOT NULL,
	role_snapshot JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS context_snapshots_project_idx ON context_snapshots (project_id);

CREATE TABLE IF NOT EXISTS context_snapshot_files (
	id UUID PRIMARY KEY,
	snapshot_id UUID NOT NULL REFERENCES context_snapshots (id) ON DELETE CASCADE,
	state_file_id UUID NOT NULL,
	type_key TEXT NOT NULL,
	scope TEXT NOT NULL,
	version INTEGER NOT NULL CHECK (version > 0),
	content_hash TEXT NOT NULL,
	content TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS context_snapshot_files_snapshot_idx ON context_snapshot_files (snapshot_id);
