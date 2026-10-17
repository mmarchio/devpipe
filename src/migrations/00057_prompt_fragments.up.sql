-- P10-20 (WBS 10.45-10.48): reusable static prompt fragments. A fragment is
-- keyed only by its own dependencies and is never a worker prompt.
CREATE TABLE IF NOT EXISTS prompt_fragments (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	fragment_key TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('SYSTEM_ROLE_PREFIX', 'STATE_PREFIX', 'STATIC_CONTEXT', 'CUSTOM')),
	content TEXT NOT NULL,
	hash TEXT NOT NULL,
	estimated_tokens INTEGER NOT NULL DEFAULT 0,
	sources JSONB NOT NULL DEFAULT '[]'::jsonb,
	hit_count BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (project_id, fragment_key)
);
CREATE OR REPLACE FUNCTION prompt_fragment_guard() RETURNS trigger AS $$
BEGIN
	IF NEW.content <> OLD.content OR NEW.hash <> OLD.hash OR NEW.fragment_key <> OLD.fragment_key
		OR NEW.project_id <> OLD.project_id OR NEW.kind <> OLD.kind THEN
		RAISE EXCEPTION 'prompt fragment content is immutable' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS prompt_fragments_guard ON prompt_fragments;
CREATE TRIGGER prompt_fragments_guard BEFORE UPDATE ON prompt_fragments
	FOR EACH ROW EXECUTE FUNCTION prompt_fragment_guard();
