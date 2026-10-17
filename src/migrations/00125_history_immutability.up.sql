-- P18-71 (WBS 18.204): historical immutability coverage. Snapshots and
-- prompt history that execution lineage depends on reject every UPDATE.
-- (Executions, steps, dispatches, results, validation, checkpoints, audit
-- and the configuration snapshots were already guarded.) A canonical
-- prompt's projection link may only be cleared by its projection's
-- deletion (ON DELETE SET NULL); nothing else changes.
CREATE OR REPLACE FUNCTION overseer_canonical_prompt_guard() RETURNS trigger AS $$
DECLARE
	probe canonical_prompts;
BEGIN
	IF OLD.projection_id IS NOT NULL AND NEW.projection_id IS NULL THEN
		probe := NEW;
		probe.projection_id := OLD.projection_id;
		IF ROW(probe.*) IS NOT DISTINCT FROM ROW(OLD.*) THEN
			RETURN NEW;
		END IF;
	END IF;
	RAISE EXCEPTION 'canonical_prompts rows are immutable' USING ERRCODE = 'check_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS canonical_prompts_immutable ON canonical_prompts;
CREATE TRIGGER canonical_prompts_immutable BEFORE UPDATE ON canonical_prompts
	FOR EACH ROW EXECUTE FUNCTION overseer_canonical_prompt_guard();

DROP TRIGGER IF EXISTS canonical_prompt_components_immutable ON canonical_prompt_components;
CREATE TRIGGER canonical_prompt_components_immutable BEFORE UPDATE ON canonical_prompt_components
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
DROP TRIGGER IF EXISTS canonical_prompt_preservation_items_immutable ON canonical_prompt_preservation_items;
CREATE TRIGGER canonical_prompt_preservation_items_immutable BEFORE UPDATE ON canonical_prompt_preservation_items
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
DROP TRIGGER IF EXISTS context_snapshots_immutable ON context_snapshots;
CREATE TRIGGER context_snapshots_immutable BEFORE UPDATE ON context_snapshots
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
DROP TRIGGER IF EXISTS context_snapshot_files_immutable ON context_snapshot_files;
CREATE TRIGGER context_snapshot_files_immutable BEFORE UPDATE ON context_snapshot_files
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
DROP TRIGGER IF EXISTS wbs_versions_immutable ON wbs_versions;
CREATE TRIGGER wbs_versions_immutable BEFORE UPDATE ON wbs_versions
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
DROP TRIGGER IF EXISTS state_file_versions_immutable ON state_file_versions;
CREATE TRIGGER state_file_versions_immutable BEFORE UPDATE ON state_file_versions
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
DROP TRIGGER IF EXISTS prompt_template_versions_immutable ON prompt_template_versions;
CREATE TRIGGER prompt_template_versions_immutable BEFORE UPDATE ON prompt_template_versions
	FOR EACH ROW EXECUTE FUNCTION overseer_reject_update();
