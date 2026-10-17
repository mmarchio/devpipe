-- P6-17 (WBS 6.20): state-machine versioning. An edit appends a NEW
-- machine row (the machine family is identified by a stable family_id;
-- version numbers the rows), fresh state/transition rows for the new
-- version, so every historical version remains retrievable and an active
-- execution's (machine, version) binding is never mutated.
-- 1. States and transitions are version-scoped: a state/transition row
--    belongs to one machine-version. Existing rows are v1.
ALTER TABLE workflow_states
	ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE state_transitions
	ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
-- A key is unique within one machine-version (WBS 6.2: unique within a
-- machine; WBS 6.20: the same key reappears in every new version with a
-- fresh state id).
ALTER TABLE workflow_states
	DROP CONSTRAINT workflow_states_machine_id_key_key;
ALTER TABLE workflow_states
	ADD CONSTRAINT workflow_states_machine_key_version_uq
	UNIQUE (machine_id, version, key);
-- 2. Machine families: versions of one machine share a family identity
--    (the first version's id, which is stable). An edit appends a row
--    carrying the same family_id, so the family's versions are addressable
--    from any of them. (DEFAULT cannot reference a column, so the backfill
--    is a separate UPDATE.)
ALTER TABLE state_machines
	ADD COLUMN family_id UUID;
UPDATE state_machines SET family_id = id;
ALTER TABLE state_machines
	ALTER COLUMN family_id SET NOT NULL;
-- 3. The family's version numbers are unique: an edit must land on a
--    distinct version (belt-and-braces behind the advisory-locked
--    MAX(version)+1 allocation).
ALTER TABLE state_machines
	ADD CONSTRAINT state_machines_family_version_uq
	UNIQUE (family_id, version);
