-- P11-19 (WBS 11.53): measured transition latency — worker result received
-- → next worker prompt ready — by preparation source.
CREATE TABLE IF NOT EXISTS transition_latencies (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	state_key TEXT NOT NULL DEFAULT '',
	work_unit_id UUID NULL,
	worker_prompt_id UUID NULL,
	cache_entry_id UUID NULL,
	source TEXT NOT NULL,
	result_received_at TIMESTAMPTZ NOT NULL,
	prompt_ready_at TIMESTAMPTZ NOT NULL,
	duration_ms BIGINT NOT NULL CHECK (duration_ms >= 0),
	created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS transition_latencies_project_idx ON transition_latencies (project_id, created_at);
