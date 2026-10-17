-- P4-17 (WBS 4.23): explicit phase failure recording.
-- Additive columns; existing rows keep their state (failure fields NULL).
ALTER TABLE phases
	ADD COLUMN failure_reason TEXT,
	ADD COLUMN failed_at TIMESTAMPTZ;
