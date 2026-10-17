-- P14-28 (WBS 14.53): append-only continuation transition history.
-- Sequence is unique and monotonic per continuation; rows are immutable.
CREATE TABLE IF NOT EXISTS continuation_transitions (
	id UUID PRIMARY KEY,
	continuation_id UUID NOT NULL REFERENCES continuations (id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	from_status TEXT NULL,
	to_status TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL,
	UNIQUE (continuation_id, sequence)
);
DROP TRIGGER IF EXISTS continuation_transitions_immutable ON continuation_transitions;
CREATE TRIGGER continuation_transitions_immutable BEFORE UPDATE ON continuation_transitions
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
