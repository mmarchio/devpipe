-- P10-20 (WBS 10.37/10.66): append-only cache audit events — the source of
-- cache metrics (reconstructable after restart) and the activity read
-- model. Events carry identities, keys and short reasons; never prompt
-- content or secrets.
CREATE TABLE IF NOT EXISTS prompt_cache_events (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	entry_id UUID NULL,
	cache_key TEXT NOT NULL DEFAULT '',
	layer TEXT NOT NULL DEFAULT '',
	event_type TEXT NOT NULL,
	state_key TEXT NOT NULL DEFAULT '',
	work_unit_id UUID NULL,
	reason TEXT NOT NULL DEFAULT '',
	avoided_compression_calls INTEGER NOT NULL DEFAULT 0,
	avoided_tokens INTEGER NOT NULL DEFAULT 0,
	avoided_latency_ms BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS prompt_cache_events_project_idx ON prompt_cache_events (project_id, created_at);
