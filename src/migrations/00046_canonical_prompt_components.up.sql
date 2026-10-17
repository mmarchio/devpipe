-- P9-12 (WBS 9.4 persistence): the composed components of a canonical prompt
-- with their compression policies and source provenance. Phase 8 persisted
-- only the rendered content; compression needs the policy boundaries. Rows
-- are written in the canonical prompt's creation transaction, in canonical
-- order (ordinal), and never rewritten.
CREATE TABLE IF NOT EXISTS canonical_prompt_components (
	canonical_prompt_id UUID NOT NULL REFERENCES canonical_prompts (id) ON DELETE CASCADE,
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
	component_type TEXT NOT NULL,
	source TEXT NOT NULL,
	policy TEXT NOT NULL CHECK (policy IN ('PRESERVE_EXACT', 'PRESERVE_SEMANTICS', 'COMPRESSIBLE', 'OPTIONAL')),
	content TEXT NOT NULL,
	source_id UUID NULL,
	source_version INTEGER NOT NULL DEFAULT 0,
	source_hash TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (canonical_prompt_id, ordinal)
);
