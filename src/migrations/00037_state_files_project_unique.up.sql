-- P7-09 (WBS 7.14 T7.14.5): the state-file uniqueness constraint must be
-- per-project. The original index (00035) keys only on (type_id, scope,
-- association), so a second project could never create a file for the same
-- global type (e.g. ARCHITECTURE) with the same scope — a conflict with a
-- file that belongs to a different project. State files are project-scoped
-- entities, so a project must be able to keep its own file of every type
-- independently of other projects. The index is rebuilt to include
-- project_id; the association COALESCE sentinels are unchanged so the
-- (type, scope, association) uniqueness within one project is preserved.
DROP INDEX IF EXISTS state_files_type_scope_association_unique;
CREATE UNIQUE INDEX IF NOT EXISTS state_files_type_scope_association_unique
	ON state_files (project_id, type_id, scope,
		COALESCE(phase_id, '00000000-0000-0000-0000-000000000000'::uuid),
		COALESCE(work_unit_id, '00000000-0000-0000-0000-000000000000'::uuid));
