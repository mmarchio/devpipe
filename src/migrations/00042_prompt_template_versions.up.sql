-- P8-11 (WBS 8.4 persistence): append-only content versions for prompt
-- templates. Each version stores the exact source bytes and a deterministic
-- SHA-256 content hash (T8.4.6). The (template_id, version) unique key is the
-- database-level duplicate guard: concurrent writers that compute the same
-- MAX+1 cannot both commit (T8.4.5 DB half). Versions are never rewritten.
CREATE TABLE IF NOT EXISTS prompt_template_versions (
	id UUID PRIMARY KEY,
	template_id UUID NOT NULL REFERENCES prompt_templates (id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS prompt_template_versions_template_version_unique
	ON prompt_template_versions (template_id, version);
