CREATE TABLE IF NOT EXISTS work_unit_dependencies (
	work_unit_id UUID NOT NULL REFERENCES work_units (id) ON DELETE CASCADE,
	depends_on_work_unit_id UUID NOT NULL REFERENCES work_units (id) ON DELETE CASCADE,
	PRIMARY KEY (work_unit_id, depends_on_work_unit_id),
	CHECK (work_unit_id <> depends_on_work_unit_id)
);
