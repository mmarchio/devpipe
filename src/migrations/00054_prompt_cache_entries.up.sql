-- P10-18 (WBS 10.1/10.13/10.27/10.30-10.34/10.43/10.68): persistent,
-- content-addressed prompt cache entries. An entry references immutable
-- Phase 8/9 prompt artifacts (never prompt text). Exactly one authoritative
-- (VALID or BUILDING) entry may exist per (project, key, layer): the partial
-- unique index lets INVALIDATED / FAILED history coexist with a later valid
-- entry for the same key (WBS 10.33.3).
CREATE TABLE IF NOT EXISTS prompt_cache_entries (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	cache_key TEXT NOT NULL,
	layer TEXT NOT NULL CHECK (layer IN ('WORKER', 'CANONICAL', 'COMPRESSION')),
	canonical_prompt_id UUID NULL REFERENCES canonical_prompts (id) ON DELETE CASCADE,
	compressed_prompt_id UUID NULL REFERENCES compressed_prompts (id) ON DELETE CASCADE,
	worker_prompt_id UUID NULL REFERENCES worker_prompts (id) ON DELETE CASCADE,
	status TEXT NOT NULL CHECK (status IN ('VALID', 'BUILDING', 'FAILED', 'INVALIDATED')),
	source TEXT NOT NULL DEFAULT 'ON_DEMAND' CHECK (source IN ('ON_DEMAND', 'PRECOMPUTED')),
	state_key TEXT NOT NULL DEFAULT '',
	phase_id UUID NULL,
	work_unit_id UUID NULL,
	metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
	build_owner TEXT NOT NULL DEFAULT '',
	lease_expires_at TIMESTAMPTZ NULL,
	error_kind TEXT NOT NULL DEFAULT '',
	error_message TEXT NOT NULL DEFAULT '',
	retry_after TIMESTAMPTZ NULL,
	invalidation_reason TEXT NOT NULL DEFAULT '',
	compression_calls INTEGER NOT NULL DEFAULT 0 CHECK (compression_calls >= 0),
	compression_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (compression_input_tokens >= 0),
	build_duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (build_duration_ms >= 0),
	hit_count BIGINT NOT NULL DEFAULT 0 CHECK (hit_count >= 0),
	created_at TIMESTAMPTZ NOT NULL,
	last_used_at TIMESTAMPTZ NOT NULL,
	CHECK (status <> 'VALID' OR worker_prompt_id IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS prompt_cache_entries_authoritative_key
	ON prompt_cache_entries (project_id, cache_key, layer) WHERE status IN ('VALID', 'BUILDING');
CREATE INDEX IF NOT EXISTS prompt_cache_entries_project_key_idx ON prompt_cache_entries (project_id, cache_key);
CREATE INDEX IF NOT EXISTS prompt_cache_entries_project_status_idx ON prompt_cache_entries (project_id, status);
CREATE INDEX IF NOT EXISTS prompt_cache_entries_project_last_used_idx ON prompt_cache_entries (project_id, last_used_at);
CREATE INDEX IF NOT EXISTS prompt_cache_entries_canonical_idx ON prompt_cache_entries (canonical_prompt_id);
CREATE INDEX IF NOT EXISTS prompt_cache_entries_worker_idx ON prompt_cache_entries (worker_prompt_id);
CREATE INDEX IF NOT EXISTS prompt_cache_entries_slot_idx ON prompt_cache_entries (project_id, state_key, work_unit_id, created_at);

-- WBS 10.27: an entry never silently changes what it points at. Identity
-- columns are immutable; artifact references may only be set once (from
-- NULL, when a BUILDING entry completes); status follows the lifecycle.
CREATE OR REPLACE FUNCTION prompt_cache_entry_guard() RETURNS trigger AS $$
BEGIN
	IF NEW.project_id <> OLD.project_id OR NEW.cache_key <> OLD.cache_key OR NEW.layer <> OLD.layer
		OR NEW.created_at <> OLD.created_at THEN
		RAISE EXCEPTION 'prompt cache entry identity is immutable' USING ERRCODE = 'check_violation';
	END IF;
	IF (OLD.worker_prompt_id IS NOT NULL AND NEW.worker_prompt_id IS DISTINCT FROM OLD.worker_prompt_id)
		OR (OLD.canonical_prompt_id IS NOT NULL AND NEW.canonical_prompt_id IS DISTINCT FROM OLD.canonical_prompt_id)
		OR (OLD.compressed_prompt_id IS NOT NULL AND NEW.compressed_prompt_id IS DISTINCT FROM OLD.compressed_prompt_id) THEN
		RAISE EXCEPTION 'prompt cache entry artifact references are immutable' USING ERRCODE = 'check_violation';
	END IF;
	IF NEW.status <> OLD.status AND NOT (
		(OLD.status = 'BUILDING' AND NEW.status IN ('VALID', 'FAILED')) OR
		(OLD.status = 'VALID' AND NEW.status = 'INVALIDATED')) THEN
		RAISE EXCEPTION 'illegal prompt cache status transition % -> %', OLD.status, NEW.status USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS prompt_cache_entries_guard ON prompt_cache_entries;
CREATE TRIGGER prompt_cache_entries_guard BEFORE UPDATE ON prompt_cache_entries
	FOR EACH ROW EXECUTE FUNCTION prompt_cache_entry_guard();
