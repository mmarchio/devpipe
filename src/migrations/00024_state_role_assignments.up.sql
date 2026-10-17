-- P6-07 (WBS 6.4): ordered state-to-role assignments. One row per
-- (state, role) with a position within the state's ordered list of
-- participating roles.
CREATE TABLE IF NOT EXISTS state_role_assignments (
	id UUID PRIMARY KEY,
	state_id UUID NOT NULL REFERENCES workflow_states (id) ON DELETE CASCADE,
	role_id UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
	ordinal INTEGER NOT NULL DEFAULT 0 CHECK (ordinal >= 0),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (state_id, role_id)
);
-- T6.4.6: the assigned role must belong to the same project as the state's
-- machine (a GLOBAL role — project_id NULL — is valid for any project). A
-- missing role is rejected by the FK (T6.4.2); the trigger handles the
-- case where both rows exist but the projects differ.
CREATE FUNCTION check_assignment_same_project() RETURNS trigger AS $$
DECLARE
	v_role_project UUID;
BEGIN
	SELECT project_id INTO v_role_project FROM roles WHERE id = NEW.role_id;
	IF v_role_project IS NOT NULL AND
		(SELECT sm.project_id
		 FROM workflow_states ws
		 JOIN state_machines sm ON sm.id = ws.machine_id
		 WHERE ws.id = NEW.state_id) IS DISTINCT FROM v_role_project THEN
		RAISE EXCEPTION 'assignment role % does not belong to the state machine''s project', NEW.role_id
			USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER state_role_assignments_same_project
BEFORE INSERT OR UPDATE OF state_id, role_id
ON state_role_assignments
FOR EACH ROW EXECUTE FUNCTION check_assignment_same_project();
