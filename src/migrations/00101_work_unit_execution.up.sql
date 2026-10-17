-- P16-58 (WBS 16.16–16.23, 16.58, 16.66–16.68): work units gain their
-- plan-local key, deterministic validation, typed acceptance checks,
-- declared paths, amendment lineage, the execution claim and the PASS
-- validation that justified DONE; plans gain kind / amendment lineage;
-- planner candidates are persisted with their validation report.
ALTER TABLE work_units
	ADD COLUMN IF NOT EXISTS unit_key TEXT NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS validation_commands JSONB NOT NULL DEFAULT '[]',
	ADD COLUMN IF NOT EXISTS acceptance_checks JSONB NOT NULL DEFAULT '[]',
	ADD COLUMN IF NOT EXISTS paths JSONB NOT NULL DEFAULT '[]',
	ADD COLUMN IF NOT EXISTS carried_from_unit_id UUID NULL REFERENCES work_units (id) ON DELETE SET NULL,
	ADD COLUMN IF NOT EXISTS claimed_execution_id UUID NULL REFERENCES executions (id) ON DELETE SET NULL,
	ADD COLUMN IF NOT EXISTS done_validation_run_id UUID NULL;
ALTER TABLE work_units DROP CONSTRAINT IF EXISTS work_units_done_requires_validation;
ALTER TABLE work_units ADD CONSTRAINT work_units_done_requires_validation
	CHECK (status <> 'DONE' OR claimed_execution_id IS NULL OR done_validation_run_id IS NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS work_units_one_active_claim ON work_units (claimed_execution_id)
	WHERE claimed_execution_id IS NOT NULL AND status IN ('EXECUTING', 'INCOMPLETE', 'VALIDATING');

ALTER TABLE execution_plans
	ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'INITIAL' CHECK (kind IN ('INITIAL', 'AMENDMENT')),
	ADD COLUMN IF NOT EXISTS amends_plan_id UUID NULL REFERENCES execution_plans (id) ON DELETE SET NULL,
	ADD COLUMN IF NOT EXISTS source_wbs_hash TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS plan_candidates (
	id UUID PRIMARY KEY,
	project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
	execution_id UUID NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
	phase_id UUID NOT NULL REFERENCES phases (id) ON DELETE CASCADE,
	dispatch_id UUID NULL UNIQUE REFERENCES worker_dispatches (id) ON DELETE SET NULL,
	wbs_version INTEGER NOT NULL CHECK (wbs_version >= 0),
	wbs_hash TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL CHECK (mode IN ('INITIAL', 'AMENDMENT')),
	payload JSONB NULL,
	status TEXT NOT NULL CHECK (status IN ('PENDING', 'ACCEPTED', 'REJECTED')),
	report JSONB NOT NULL DEFAULT '[]',
	plan_id UUID NULL REFERENCES execution_plans (id) ON DELETE SET NULL,
	created_at TIMESTAMPTZ NOT NULL,
	decided_at TIMESTAMPTZ NULL,
	CHECK ((status = 'PENDING') = (decided_at IS NULL)),
	CHECK (status <> 'ACCEPTED' OR plan_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS plan_candidates_phase_idx ON plan_candidates (execution_id, phase_id, created_at);
CREATE OR REPLACE FUNCTION plan_candidate_guard() RETURNS trigger AS $$
BEGIN
	IF OLD.status <> 'PENDING' THEN
		RAISE EXCEPTION 'plan candidate is decided' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS plan_candidates_guard ON plan_candidates;
CREATE TRIGGER plan_candidates_guard BEFORE UPDATE ON plan_candidates
	FOR EACH ROW EXECUTE FUNCTION plan_candidate_guard();

-- P16-80: a planner's plan candidate travels in the Phase 13 result as an
-- opaque `plan` object, stored verbatim (TEXT keeps the bytes the
-- normalized hash covers).
ALTER TABLE worker_results ADD COLUMN IF NOT EXISTS plan TEXT NULL;
