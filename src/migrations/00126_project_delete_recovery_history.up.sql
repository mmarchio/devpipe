-- Project deletion (WBS 3.13): recovery plans and actions stay immutable
-- while their project exists, but must not block the projects ON DELETE
-- CASCADE. During that cascade the parent projects row is already gone, so
-- a DELETE is allowed exactly when the owning project no longer exists.
CREATE OR REPLACE FUNCTION recovery_plan_guard() RETURNS trigger AS $$
BEGIN
	IF TG_OP = 'DELETE' AND NOT EXISTS (SELECT 1 FROM projects WHERE id = OLD.project_id) THEN
		RETURN OLD;
	END IF;
	RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME USING ERRCODE = 'check_violation';
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS recovery_plans_immutable ON recovery_plans;
CREATE TRIGGER recovery_plans_immutable BEFORE UPDATE OR DELETE ON recovery_plans
	FOR EACH ROW EXECUTE FUNCTION recovery_plan_guard();

CREATE OR REPLACE FUNCTION recovery_action_guard() RETURNS trigger AS $$
BEGIN
	IF TG_OP = 'DELETE' THEN
		IF NOT EXISTS (SELECT 1 FROM projects WHERE id = OLD.project_id) THEN
			RETURN OLD;
		END IF;
		RAISE EXCEPTION 'recovery action % is final', OLD.status USING ERRCODE = 'check_violation';
	END IF;
	IF OLD.status <> 'RUNNING' THEN
		RAISE EXCEPTION 'recovery action % is final', OLD.status USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
