CREATE TABLE IF NOT EXISTS project_status_events (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	from_status TEXT NOT NULL,
	to_status TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_project_status_events_project ON project_status_events (project_id, created_at);
