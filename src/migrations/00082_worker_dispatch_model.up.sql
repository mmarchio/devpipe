-- P13-28 (WBS 13.63): the worker model in force when a dispatch was sent —
-- authoritative grouping key for protocol compliance metrics.
ALTER TABLE worker_dispatches ADD COLUMN IF NOT EXISTS worker_model TEXT NOT NULL DEFAULT '';
