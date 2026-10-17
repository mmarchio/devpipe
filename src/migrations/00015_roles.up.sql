-- P5-03 (WBS 5.1): role definitions. project_id NULL = GLOBAL scope
-- (defaults/templates); set = PROJECT scope. Scoped key uniqueness: a key
-- is unique per (scope, key); the global scope is folded to a sentinel UUID
-- so (NULL, k) and (project_a, k) can coexist.
CREATE TABLE IF NOT EXISTS roles (
	id UUID PRIMARY KEY,
	project_id UUID NULL REFERENCES projects (id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	enabled BOOLEAN NOT NULL DEFAULT true,
	version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS roles_scope_key_unique
	ON roles (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), key);

-- P5-03 (WBS 5.5): system-prompt version history (append-only). The roles row carries the current-version pointer.
ALTER TABLE roles
	ADD COLUMN IF NOT EXISTS current_system_prompt_version INTEGER NOT NULL DEFAULT 1 CHECK (current_system_prompt_version > 0);
CREATE TABLE IF NOT EXISTS role_system_prompts (
	id UUID PRIMARY KEY,
	role_id UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (role_id, version)
);
