-- P11-18 (WBS 11.19-11.22): projected execution contexts. A projection
-- describes what the prompt context WOULD be after a valid future
-- transition; it references the real base snapshot and is never itself a
-- snapshot. Immutable.
CREATE TABLE IF NOT EXISTS precompute_projections (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	base_snapshot_id UUID NOT NULL REFERENCES context_snapshots (id) ON DELETE CASCADE,
	base_semantic_hash TEXT NOT NULL,
	phase_id UUID NULL,
	projected_work_unit_id UUID NULL,
	projected_state_key TEXT NOT NULL,
	projected_role_key TEXT NOT NULL DEFAULT '',
	transition_path JSONB NOT NULL DEFAULT '[]'::jsonb,
	pins JSONB NOT NULL DEFAULT '[]'::jsonb,
	projection_hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS precompute_projections_project_idx ON precompute_projections (project_id, projection_hash);
DROP TRIGGER IF EXISTS precompute_projections_immutable ON precompute_projections;
CREATE TRIGGER precompute_projections_immutable BEFORE UPDATE ON precompute_projections
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
