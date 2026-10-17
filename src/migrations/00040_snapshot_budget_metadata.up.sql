-- P7-20 (WBS 7.35/7.36): snapshot token budget + source metadata.
--
-- The snapshot row carries the aggregate token budget (state-file
-- estimates summed, role estimate, their sum) - the WBS 7.35 estimate is
-- STORED WITH THE SNAPSHOT, so the budget survives reconstruction without
-- re-derivation. Each state-file pin carries its own token estimate and
-- its WBS 7.36 source classification, persisted at snapshot time so the
-- classification is read back from the snapshot, not re-derived from
-- live state (T7.36.5). The estimate columns carry a neutral DEFAULT 0
-- and classification a DEFAULT '' so pre-existing snapshot rows (from
-- P7-18 runs on the same database) backfill to the neutral values and
-- stay readable.
ALTER TABLE context_snapshots
	ADD COLUMN IF NOT EXISTS state_file_tokens INTEGER NOT NULL DEFAULT 0 CHECK (state_file_tokens >= 0),
	ADD COLUMN IF NOT EXISTS role_tokens INTEGER NOT NULL DEFAULT 0 CHECK (role_tokens >= 0),
	ADD COLUMN IF NOT EXISTS estimated_total INTEGER NOT NULL DEFAULT 0 CHECK (estimated_total >= 0);

ALTER TABLE context_snapshot_files
	ADD COLUMN IF NOT EXISTS state_file_token_estimate INTEGER NOT NULL DEFAULT 0 CHECK (state_file_token_estimate >= 0),
	ADD COLUMN IF NOT EXISTS classification TEXT NOT NULL DEFAULT '';
