-- P4-07 (WBS 4.10 recommended model + 4.11 storage): WBS version history.
-- Historical versions are append-only; the single existing phase_documents
-- row remains the current-version pointer (content-hash algorithm lands in
-- P4-08, so content_hash stores an empty-hash placeholder for now).
CREATE TABLE IF NOT EXISTS wbs_versions (
	id UUID PRIMARY KEY,
	phase_id UUID NOT NULL REFERENCES phases (id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (phase_id, version)
);
ALTER TABLE phase_documents
	ADD COLUMN current_version INTEGER NOT NULL DEFAULT 1 CHECK (current_version > 0);
