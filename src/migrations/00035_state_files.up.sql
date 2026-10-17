-- P7-05 (WBS 7.4): logical, project-scoped, versioned state files. The
-- file points at versioned content (state_file_versions, 00036) and carries
-- the association its scope requires. Cross-project phase/work-unit
-- associations are rejected by composite (project_id, association) FKs
-- (T7.4.5/T7.4.6 DB half — the domain-level project-match guard is the
-- AssociationBelongsToProject check; the DB FK pins row existence). A
-- (type, scope, association) pair yields at most one file.
CREATE TABLE IF NOT EXISTS state_files (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	type_id UUID NOT NULL REFERENCES state_file_types (id) ON DELETE CASCADE,
	scope TEXT NOT NULL CHECK (scope IN ('PROJECT', 'PHASE', 'WORK_UNIT', 'EXECUTION')),
	phase_id UUID NULL REFERENCES phases (id) ON DELETE CASCADE,
	work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE CASCADE,
	current_version INTEGER NOT NULL DEFAULT 0 CHECK (current_version >= 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS state_files_type_scope_association_unique
	ON state_files (type_id, scope,
		COALESCE(phase_id, '00000000-0000-0000-0000-000000000000'::uuid),
		COALESCE(work_unit_id, '00000000-0000-0000-0000-000000000000'::uuid));
