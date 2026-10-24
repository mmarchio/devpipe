-- Phase 19: archive membership of a finished work unit (TASK_ARCHIVE.md
-- under FILES state authority). Set once, by the accepting dispatch.
ALTER TABLE work_units ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ NULL;
ALTER TABLE work_units ADD COLUMN IF NOT EXISTS archived_by_dispatch_id UUID NULL REFERENCES worker_dispatches (id) ON DELETE SET NULL;
