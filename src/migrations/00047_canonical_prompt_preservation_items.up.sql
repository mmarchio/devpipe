-- P9-13 (WBS 9.18-9.22): the preservation manifest of a canonical prompt —
-- individually identifiable items (AC-n, TEST-n, CONSTRAINT-n, identifiers,
-- schema fields, status values) with overseer-assigned stable IDs. The
-- canonical content and hash are unchanged; IDs live here.
CREATE TABLE IF NOT EXISTS canonical_prompt_preservation_items (
	canonical_prompt_id UUID NOT NULL REFERENCES canonical_prompts (id) ON DELETE CASCADE,
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
	kind TEXT NOT NULL,
	item_id TEXT NOT NULL,
	text TEXT NOT NULL,
	PRIMARY KEY (canonical_prompt_id, ordinal),
	UNIQUE (canonical_prompt_id, item_id)
);
