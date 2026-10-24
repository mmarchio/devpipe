-- The statefile workflow's state lives in the database only: the team
-- reports through the overseer MCP server and no statefile decides a
-- transition. Projects that never cut over get the cutover's import:
-- every work unit the overseer marked DONE is archived (TASK_ARCHIVE.md
-- is no longer read). project_state_authority and statefile_hashes are
-- kept as history; nothing reads or writes them any more.

UPDATE work_units u SET archived_at = COALESCE(u.completed_at, now())
WHERE u.status = 'DONE'
  AND u.archived_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM project_state_authority a WHERE a.project_id = u.project_id);
