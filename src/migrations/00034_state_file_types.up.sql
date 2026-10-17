-- P7-05 (WBS 7.1): logical state-file types, project-scoped (nil project =
-- global scope). The scoped key is unique per (project scope, key) so the
-- same key is legal across projects but not twice in one scope (T7.1.3/T7.1.4
-- DB half). CHECK sets mirror the domain's Known() sets (WBS 7.3/7.16).
CREATE TABLE IF NOT EXISTS state_file_types (
	id UUID PRIMARY KEY,
	project_id UUID NULL REFERENCES projects (id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	default_filename TEXT NOT NULL DEFAULT '',
	scope TEXT NOT NULL CHECK (scope IN ('PROJECT', 'PHASE', 'WORK_UNIT', 'EXECUTION')),
	source TEXT NOT NULL DEFAULT 'MANAGED' CHECK (source IN ('DERIVED', 'MANAGED', 'USER', 'EXTERNAL')),
	required BOOLEAN NOT NULL DEFAULT false,
	enabled BOOLEAN NOT NULL DEFAULT true,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS state_file_types_scope_key_unique
	ON state_file_types (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), key);
