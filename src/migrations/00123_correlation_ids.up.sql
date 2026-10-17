-- P18-58 (WBS 18.111 / 18.112): the correlation id of the operation that
-- created a worker dispatch or a validation run (the HTTP request id, or
-- the id generated for an engine step), so one operation is traceable
-- across steps, events, dispatches, validation, recovery actions and audit.
ALTER TABLE worker_dispatches ADD COLUMN IF NOT EXISTS correlation_id TEXT NOT NULL DEFAULT '' CHECK (length(correlation_id) <= 128);
ALTER TABLE validation_runs ADD COLUMN IF NOT EXISTS correlation_id TEXT NOT NULL DEFAULT '' CHECK (length(correlation_id) <= 128);
CREATE INDEX IF NOT EXISTS worker_dispatches_correlation_idx ON worker_dispatches (correlation_id) WHERE correlation_id <> '';
CREATE INDEX IF NOT EXISTS validation_runs_correlation_idx ON validation_runs (correlation_id) WHERE correlation_id <> '';
