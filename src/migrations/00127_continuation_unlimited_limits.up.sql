-- Continuation / session-reset maximums accept -1 = no limit. The original
-- inline CHECKs (00083) have generated, possibly truncated names, so they
-- are dropped by column and re-added under explicit names.
DO $$
DECLARE c TEXT;
BEGIN
	FOR c IN
		SELECT con.conname FROM pg_constraint con
		JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = ANY (con.conkey)
		WHERE con.conrelid = 'project_continuation_settings'::regclass AND con.contype = 'c'
		  AND att.attname IN ('max_continuations_per_work_unit', 'max_session_resets_per_work_unit')
	LOOP
		EXECUTE format('ALTER TABLE project_continuation_settings DROP CONSTRAINT %I', c);
	END LOOP;
END $$;
ALTER TABLE project_continuation_settings DROP CONSTRAINT IF EXISTS project_continuation_settings_max_continuations_check;
ALTER TABLE project_continuation_settings ADD CONSTRAINT project_continuation_settings_max_continuations_check
	CHECK (max_continuations_per_work_unit >= 1 OR max_continuations_per_work_unit = -1);
ALTER TABLE project_continuation_settings DROP CONSTRAINT IF EXISTS project_continuation_settings_max_resets_check;
ALTER TABLE project_continuation_settings ADD CONSTRAINT project_continuation_settings_max_resets_check
	CHECK (max_session_resets_per_work_unit >= 0 OR max_session_resets_per_work_unit = -1);
