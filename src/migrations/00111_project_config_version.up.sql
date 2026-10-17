-- P17-46 (WBS 17.140): optimistic-concurrency version of a project's
-- sub-configuration (compression, continuation, engine, execution,
-- precompute, prompt cache, result protocol, planning). Every write bumps
-- it; a write carrying an expected version that is no longer current is a
-- 409 conflict.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS config_version INTEGER NOT NULL DEFAULT 1 CHECK (config_version > 0);
