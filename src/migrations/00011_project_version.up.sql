-- P3-24 (WBS 3.24): optimistic-concurrency version for projects.
-- Clients send the version they read back with mutations; a stale version
-- is rejected (409) and never overwrites newer data.
ALTER TABLE projects ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
