-- P18-30 (WBS 18.15): the driver fencing epoch never decreases or
-- repeats for an execution — every acquisition by a new holder increments
-- it, and no write may lower it.
CREATE OR REPLACE FUNCTION executions_driver_epoch_guard() RETURNS trigger AS $$
BEGIN
	IF NEW.driver_epoch < OLD.driver_epoch THEN
		RAISE EXCEPTION 'driver epoch must not decrease (% -> %)', OLD.driver_epoch, NEW.driver_epoch USING ERRCODE = 'check_violation';
	END IF;
	IF NEW.driver_owner IS DISTINCT FROM OLD.driver_owner AND NEW.driver_owner IS NOT NULL AND NEW.driver_epoch = OLD.driver_epoch
		AND OLD.driver_owner IS NOT NULL THEN
		RAISE EXCEPTION 'a new driver must take a new epoch' USING ERRCODE = 'check_violation';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS executions_driver_epoch ON executions;
CREATE TRIGGER executions_driver_epoch BEFORE UPDATE OF driver_epoch, driver_owner ON executions
	FOR EACH ROW EXECUTE FUNCTION executions_driver_epoch_guard();
