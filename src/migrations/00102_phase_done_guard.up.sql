-- P16-59 (WBS 16.85): a phase cannot become DONE while its current plan
-- (latest non-SUPERSEDED version) has a non-DONE unit. Enforced for
-- projects under execution (any execution row); pre-execution data-level
-- bookkeeping (Phases 2-7) is unaffected.
CREATE OR REPLACE FUNCTION phase_done_guard() RETURNS trigger AS $$
DECLARE
	cur UUID;
BEGIN
	IF NEW.status = 'DONE' AND OLD.status IS DISTINCT FROM 'DONE'
		AND EXISTS (SELECT 1 FROM executions WHERE project_id = NEW.project_id) THEN
		SELECT id INTO cur FROM execution_plans
			WHERE phase_id = NEW.id AND status <> 'SUPERSEDED'
			ORDER BY version DESC LIMIT 1;
		IF cur IS NOT NULL AND EXISTS (SELECT 1 FROM work_units WHERE execution_plan_id = cur AND status <> 'DONE') THEN
			RAISE EXCEPTION 'phase % cannot be DONE: its current plan has non-DONE units', NEW.id
				USING ERRCODE = 'check_violation';
		END IF;
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS phases_done_guard ON phases;
CREATE TRIGGER phases_done_guard BEFORE UPDATE ON phases
	FOR EACH ROW EXECUTE FUNCTION phase_done_guard();
