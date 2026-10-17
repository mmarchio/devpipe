-- P4-31 (Phase 4 DoD gate): phase numbers are never reused after deletion.
-- The phases table alone cannot track this once the highest-order phase is
-- deleted (MAX over live rows drops back to a historical number), so the
-- project carries a monotonically increasing high-water mark of the largest
-- phase_order ever issued. Backfill it from the current phase rows.
ALTER TABLE projects ADD COLUMN max_phase_order INTEGER NOT NULL DEFAULT 0;
UPDATE projects AS p SET max_phase_order = m.max_order
  FROM (SELECT project_id, MAX(phase_order) AS max_order FROM phases GROUP BY project_id) AS m
  WHERE p.id = m.project_id;
