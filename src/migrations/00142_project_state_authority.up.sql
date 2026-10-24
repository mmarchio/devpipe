-- Phase 19: a project under DATABASE state authority (no row: FILES).
-- Insert-only: the cutover cannot be undone silently once the database
-- holds newer workflow state.
CREATE TABLE IF NOT EXISTS project_state_authority (
	project_id UUID PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
	authority TEXT NOT NULL CHECK (authority = 'DATABASE'),
	cutover_by TEXT NOT NULL CHECK (cutover_by <> ''),
	imported JSONB NOT NULL DEFAULT '{}'::jsonb,
	cutover_at TIMESTAMPTZ NOT NULL
);
DROP TRIGGER IF EXISTS project_state_authority_immutable ON project_state_authority;
CREATE TRIGGER project_state_authority_immutable BEFORE UPDATE ON project_state_authority
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
