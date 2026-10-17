-- P11-19 (WBS 11.13): which dynamic inputs a precomputed static fragment
-- still needs. Immutable.
CREATE TABLE IF NOT EXISTS fragment_manifests (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	fragment_id UUID NOT NULL REFERENCES prompt_fragments (id) ON DELETE CASCADE,
	fragment_key TEXT NOT NULL,
	required_inputs JSONB NOT NULL,
	source_snapshot_hash TEXT NOT NULL,
	projection_hash TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (fragment_id, projection_hash)
);
DROP TRIGGER IF EXISTS fragment_manifests_immutable ON fragment_manifests;
CREATE TRIGGER fragment_manifests_immutable BEFORE UPDATE ON fragment_manifests
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
