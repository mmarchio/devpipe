-- P9-18 (WBS 9.10): versioned compression-model system instructions. Append-
-- only: a change appends a new version; historical versions are retained
-- (T9.10.3) because a change affects output and cache behaviour.
CREATE TABLE IF NOT EXISTS compression_instructions (
	version INTEGER PRIMARY KEY CHECK (version > 0),
	content TEXT NOT NULL,
	hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
