-- P10-19 (WBS 10.54): the Layer 1 (canonical) cache key — a lookup index
-- over the existing immutable canonical prompt artifacts, so a compression
-- configuration change reuses the canonical prompt instead of recomposing.
ALTER TABLE canonical_prompts ADD COLUMN IF NOT EXISTS cache_key TEXT NULL;
CREATE INDEX IF NOT EXISTS canonical_prompts_cache_key_idx ON canonical_prompts (project_id, cache_key);
