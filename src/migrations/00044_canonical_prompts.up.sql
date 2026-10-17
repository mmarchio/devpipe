-- P8-30 (WBS 8.26 persistence): canonical prompt persistence. A canonical
-- prompt is the exact, persisted record of one composed prompt: the
-- project/phase/work-unit associations plus the full provenance (the
-- context snapshot, the pinned template version, the pinned role version,
-- the pinned state-machine version + state key) and the exact content +
-- hash + estimated-token count. One row per creation — append-only; the
-- content is stored byte-for-byte so a historical prompt stays
-- reconstructable after every source advances (WBS 8.48).
CREATE TABLE IF NOT EXISTS canonical_prompts (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	phase_id UUID NULL REFERENCES phases (id) ON DELETE CASCADE,
	work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE CASCADE,
	context_snapshot_id UUID NOT NULL REFERENCES context_snapshots (id) ON DELETE CASCADE,
	template_id UUID NOT NULL REFERENCES prompt_templates (id) ON DELETE CASCADE,
	template_version INTEGER NOT NULL CHECK (template_version > 0),
	role_id UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
	role_version INTEGER NOT NULL CHECK (role_version > 0),
	state_machine_id UUID NOT NULL REFERENCES state_machines (id) ON DELETE CASCADE,
	state_machine_version INTEGER NOT NULL CHECK (state_machine_version > 0),
	state_key TEXT NOT NULL,
	content TEXT NOT NULL,
	hash TEXT NOT NULL,
	estimated_tokens INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS canonical_prompts_project_idx ON canonical_prompts (project_id);
