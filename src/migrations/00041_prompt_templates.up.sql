-- P8-10 (WBS 8.1): configurable prompt templates, project-scoped (nil project
-- = global scope). The scoped key is unique per (project scope, key) so the
-- same key is legal across projects and globally but not twice within one
-- scope (T8.1.3 DB half). The CHECK set mirrors the domain's Known() purposes
-- (WBS 8.1).
CREATE TABLE IF NOT EXISTS prompt_templates (
	id UUID PRIMARY KEY,
	project_id UUID NULL REFERENCES projects (id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	purpose TEXT NOT NULL CHECK (purpose IN ('PHASE_PLANNING', 'IMPLEMENTATION', 'CONTINUATION', 'VALIDATION', 'REVIEW', 'CONTROL', 'CUSTOM')),
	enabled BOOLEAN NOT NULL DEFAULT true,
	version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS prompt_templates_scope_key_unique
	ON prompt_templates (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), key);
