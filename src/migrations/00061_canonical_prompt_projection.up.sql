-- P11-18 (WBS 11.20): a canonical prompt composed from a projection keeps
-- context_snapshot_id = the real base snapshot and records the projection.
ALTER TABLE canonical_prompts ADD COLUMN IF NOT EXISTS projection_id UUID NULL REFERENCES precompute_projections (id) ON DELETE SET NULL;
