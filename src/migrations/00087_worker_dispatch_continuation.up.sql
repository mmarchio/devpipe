-- P14-30 (WBS 14.35): a continuation dispatch references its continuation.
ALTER TABLE worker_dispatches ADD COLUMN IF NOT EXISTS continuation_id UUID NULL REFERENCES continuations (id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS worker_dispatches_continuation_idx ON worker_dispatches (continuation_id) WHERE continuation_id IS NOT NULL;
