-- P6-06 (WBS 6.8): directed transitions between workflow states of ONE
-- machine. The same-machine rule (T6.8.4) is enforced by a trigger, and the
-- duplicate-ambiguity rule (T6.8.5, under the T6.9.4 outcome case policy:
-- trim + case-insensitive) by a unique index.
CREATE TABLE IF NOT EXISTS state_transitions (
	id UUID PRIMARY KEY,
	machine_id UUID NOT NULL REFERENCES state_machines (id) ON DELETE CASCADE,
	from_state_id UUID NOT NULL REFERENCES workflow_states (id) ON DELETE CASCADE,
	to_state_id UUID NOT NULL REFERENCES workflow_states (id) ON DELETE CASCADE,
	outcome TEXT NOT NULL,
	priority INTEGER NOT NULL DEFAULT 0,
	enabled BOOLEAN NOT NULL DEFAULT true,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- T6.8.5: at most one transition per (machine, from_state, outcome).
CREATE UNIQUE INDEX state_transitions_ambiguous_uq
	ON state_transitions (machine_id, from_state_id, lower(btrim(outcome)));
-- T6.8.4: both endpoints must belong to the transition's machine.
CREATE FUNCTION check_transition_same_machine() RETURNS trigger AS $$
BEGIN
	IF (SELECT machine_id FROM workflow_states WHERE id = NEW.from_state_id)
		IS DISTINCT FROM NEW.machine_id
		OR (SELECT machine_id FROM workflow_states WHERE id = NEW.to_state_id)
		IS DISTINCT FROM NEW.machine_id THEN
		RAISE EXCEPTION 'transition endpoints must belong to machine %', NEW.machine_id
			USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER state_transitions_same_machine
BEFORE INSERT OR UPDATE OF machine_id, from_state_id, to_state_id
ON state_transitions
FOR EACH ROW EXECUTE FUNCTION check_transition_same_machine();
