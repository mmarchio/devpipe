-- P5-06 (WBS 5.7): reusable instruction documents. Scoped like roles:
-- project_id NULL = GLOBAL, set = PROJECT scope. Content versioning
-- (instruction_versions) lands in P5-07; the instructions row carries the
-- current-version pointer.
CREATE TABLE IF NOT EXISTS instructions (
	id UUID PRIMARY KEY,
	project_id UUID NULL REFERENCES projects (id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
	enabled BOOLEAN NOT NULL DEFAULT true,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS instructions_scope_key_unique
	ON instructions (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), key);
