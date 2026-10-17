-- P17-24 (WBS 17.76 / 17.78): a per-project, commit-ordered event sequence
-- (assigned under a project-scoped advisory transaction lock) used as the
-- SSE event id, so a client can resume with Last-Event-ID and detect gaps.
ALTER TABLE execution_events ADD COLUMN IF NOT EXISTS project_seq BIGINT NULL CHECK (project_seq > 0);

-- Backfill existing rows in creation order per project (created_at, then
-- the per-execution sequence). The immutability
-- trigger is disabled for the backfill only.
ALTER TABLE execution_events DISABLE TRIGGER execution_events_immutable;
UPDATE execution_events e SET project_seq = s.rn
	FROM (SELECT id, row_number() OVER (PARTITION BY project_id ORDER BY created_at, exec_seq NULLS FIRST, id) AS rn FROM execution_events) s
	WHERE e.id = s.id AND e.project_seq IS NULL;
ALTER TABLE execution_events ENABLE TRIGGER execution_events_immutable;

ALTER TABLE execution_events ALTER COLUMN project_seq SET NOT NULL;
ALTER TABLE execution_events DROP CONSTRAINT IF EXISTS execution_events_project_seq_key;
ALTER TABLE execution_events ADD CONSTRAINT execution_events_project_seq_key UNIQUE (project_id, project_seq);
