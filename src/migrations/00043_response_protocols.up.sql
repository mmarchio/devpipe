-- P8-13 (WBS 8.23 persistence): configurable response protocols with
-- versioned content. A protocol is the schema a worker must answer with
-- (PLAN_RESPONSE / IMPLEMENTATION_RESPONSE / VALIDATION_RESPONSE /
-- REVIEW_RESPONSE); the CHECK set mirrors the domain's Known() kinds. A
-- state may reference a protocol by (protocol, version), never "latest".
CREATE TABLE IF NOT EXISTS response_protocols (
	id UUID PRIMARY KEY,
	project_id UUID NULL REFERENCES projects (id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('PLAN_RESPONSE', 'IMPLEMENTATION_RESPONSE', 'VALIDATION_RESPONSE', 'REVIEW_RESPONSE')),
	enabled BOOLEAN NOT NULL DEFAULT true,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The same kind is legal globally and per project, but not twice within one
-- scope (scoped-key unique index, mirroring prompt_templates).
CREATE UNIQUE INDEX IF NOT EXISTS response_protocols_scope_kind_unique
	ON response_protocols (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), kind);
-- Append-only protocol content versions: each version stores the exact schema
-- bytes and a deterministic SHA-256 content hash. The (protocol_id, version)
-- unique key is the database-level duplicate guard for concurrent MAX+1
-- writers (T8.23.2 DB half); historical rows are never rewritten
-- (T8.23.4).
CREATE TABLE IF NOT EXISTS response_protocol_versions (
	id UUID PRIMARY KEY,
	protocol_id UUID NOT NULL REFERENCES response_protocols (id) ON DELETE CASCADE,
	version INTEGER NOT NULL CHECK (version > 0),
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS response_protocol_versions_protocol_version_unique
	ON response_protocol_versions (protocol_id, version);
-- A state's pin of a specific protocol version (T8.23.3): one reference per
-- (state, protocol), version-pinned so the pin stays resolvable after the
-- protocol advances.
CREATE TABLE IF NOT EXISTS state_protocol_references (
	state_id UUID NOT NULL REFERENCES workflow_states (id) ON DELETE CASCADE,
	protocol_id UUID NOT NULL REFERENCES response_protocols (id) ON DELETE CASCADE,
	protocol_version INTEGER NOT NULL CHECK (protocol_version > 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (state_id, protocol_id)
);
