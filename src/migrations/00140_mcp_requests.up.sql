-- Phase 19: MCP mutation requests by idempotency key. A repeated request
-- returns the stored result; the same key with a different payload is a
-- conflict. Immutable (audit).
CREATE TABLE IF NOT EXISTS mcp_requests (
	id UUID PRIMARY KEY,
	dispatch_id UUID NOT NULL REFERENCES worker_dispatches (id) ON DELETE CASCADE,
	tool TEXT NOT NULL CHECK (tool <> ''),
	idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
	payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
	result JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (dispatch_id, tool, idempotency_key)
);
DROP TRIGGER IF EXISTS mcp_requests_immutable ON mcp_requests;
CREATE TRIGGER mcp_requests_immutable BEFORE UPDATE ON mcp_requests
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
