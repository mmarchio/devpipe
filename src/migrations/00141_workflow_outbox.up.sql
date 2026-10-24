-- Phase 19: follow-up actions committed in the same transaction as the
-- workflow change that requires them, then performed by the outbox
-- dispatcher (claimed with a lease, retried with backoff, DEAD past the
-- retry limit).
CREATE TABLE IF NOT EXISTS workflow_outbox (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	execution_id UUID NULL REFERENCES executions (id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('ENGINE_WAKE', 'CONTEXT_RESET', 'BLOCKER_NOTIFY')),
	payload JSONB NOT NULL DEFAULT '{}'::jsonb,
	dedupe_key TEXT NOT NULL UNIQUE CHECK (dedupe_key <> ''),
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'DONE', 'DEAD')),
	attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	next_attempt_at TIMESTAMPTZ NOT NULL,
	claimed_by TEXT NULL,
	claimed_until TIMESTAMPTZ NULL,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	finished_at TIMESTAMPTZ NULL,
	CHECK ((status = 'PENDING') = (finished_at IS NULL))
);
CREATE INDEX IF NOT EXISTS workflow_outbox_due_idx ON workflow_outbox (next_attempt_at) WHERE status = 'PENDING';
