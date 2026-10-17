-- WBS 2.23: updated_at on every mutable record (phases, work_units).
ALTER TABLE phases ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE work_units ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
