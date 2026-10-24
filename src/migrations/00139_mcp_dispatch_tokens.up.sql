-- Phase 19: the capability token of a command prompt. The engine writes
-- the token into the command it sends; the MCP server binds every tool
-- call to the prompt's in-flight dispatch (only the SHA-256 is stored), so
-- a call after the dispatch finished — a late call from a stopped session
-- — is rejected. revoked_at cuts a token off early.
CREATE TABLE IF NOT EXISTS mcp_dispatch_tokens (
	token_hash TEXT PRIMARY KEY CHECK (token_hash ~ '^[0-9a-f]{64}$'),
	worker_prompt_id UUID NOT NULL UNIQUE REFERENCES worker_prompts (id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	work_unit_id UUID NULL REFERENCES work_units (id) ON DELETE CASCADE,
	role_key TEXT NOT NULL CHECK (role_key <> ''),
	state_key TEXT NOT NULL CHECK (state_key <> ''),
	created_at TIMESTAMPTZ NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	revoked_at TIMESTAMPTZ NULL,
	CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS mcp_dispatch_tokens_execution_idx ON mcp_dispatch_tokens (execution_id);
