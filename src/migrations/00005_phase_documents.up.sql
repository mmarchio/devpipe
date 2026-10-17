CREATE TABLE IF NOT EXISTS phase_documents (
	id UUID PRIMARY KEY,
	phase_id UUID NOT NULL REFERENCES phases (id) ON DELETE CASCADE,
	doc_type TEXT NOT NULL DEFAULT 'WBS'
		CHECK (doc_type IN ('WBS','NOTES','SPECIFICATION')),
	content TEXT NOT NULL,
	version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (phase_id, doc_type)
);
