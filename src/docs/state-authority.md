# Workflow state (Phase 19)

PostgreSQL is the only workflow-state store. The statefile workflow's team
reports through the overseer MCP server (`submit_result` and the other
tools), and the check states read what it submitted. No statefile under
`<working directory>/statefiles/` is read or written to decide a
transition.

Earlier releases let a project keep its workflow state in statefiles
(`FILES` authority) and move to the database with a cutover. That mode is
retired. Every project now runs under what used to be `DATABASE`
authority, and the cutover endpoints are gone.

## Ownership

All paths are in `internal/service/`.

| Value | Owner | Written by | Read by (decides transitions) |
| --- | --- | --- | --- |
| Task status reported by the team | `task_reports.status` | `submit_result` | `testResult`, `implementResult`, `acceptanceResult` |
| Acceptance criteria met | `task_reports.criteria_met` | `submit_result` (TEST) | `testResult` |
| Manual approvals | `acceptance_approvals` | `testResult` (met MANUAL criteria) | unit validation |
| Archive membership | `work_units.archived_at` | `acceptanceResult` | `get_context`, `get_task` |
| Next / active task | overseer (dependency order of `work_units`) | `acceptanceResult` | every command |
| Plan candidate | `plan_candidates` | `planCandidate` (from `submit_result` with a plan) | `handlePlanValidate` |
| Plan rejection | `plan_candidates.status = 'REJECTED'` | `rejectCandidate` | `commandText` (the next PLAN command carries the problems) |
| Replanned / fixed task | `task_reports` kind PLAN | `submit_result` (REPLAN, FIX_TASK) | `replannedTask` |
| Task analysis | `task_reports` kind ANALYSIS | `submit_result` (ANALYZE_TASK) | `runAnalysis` |
| Work-unit status, runtime position, transitions | database | engine | engine |

Project artifacts (source code, WBS documents, uploads) stay filesystem-owned.

These files are still written, as generated projections that never decide a
transition:

- `statefiles/HERMES.md` (the workflow rules and MCP tools) and
  `statefiles/<ROLE>.md` (`writeTeamFiles`).
- `.overseer/state/*` (Phase 7 materialization and drift).

`internal/archtest` (`TestDatabaseAuthorityReadsNoStatefile`) fails the
build when a workflow decision path calls a statefile reader or writer.

Bootstrap readiness (category `mcp_server`, "MCP server") refuses to start
a statefile-workflow execution when `mcp.enabled` is off: the team could
not submit any result.

## Recovery

- A command answered without `submit_result` is resent with the problem
  (`MCP_RESULT_MISSING`), up to the resend limit, then recovery. An
  implementation without a result resumes with CONTINUE.
- A malformed result (for example a TEST result whose `criteria_met` does
  not match the task's acceptance checks) is resent the same way.
- Each command carries its own capability token. Only its SHA-256 is
  stored, keyed to the command prompt. A tool call is bound to that prompt's
  in-flight dispatch, so a late call from a stopped session is rejected.
  A command prepared without a token (before the upgrade that retired the
  statefiles) is prepared again on resume.
- MCP follow-up actions are committed to `workflow_outbox` together with
  the request that caused them and survive a restart. A requested context
  reset is applied by the engine before its next command, never under a
  running dispatch.

## Upgrading from FILES authority

Migration `00144_statefiles_retired` archives every work unit the overseer
already marked DONE in projects that never cut over (the import the cutover
used to do). It is forward-only, like every migration.

- A project's in-flight command is prepared again with a token when the
  execution resumes, so the team gets the MCP instructions.
- Statefiles left in a project's working directory (`PLAN.md`,
  `BACKLOG.md`, `ACTIVE_TASK.md`, `TASK_ARCHIVE.md`, `TASK_ANALYSIS.md`,
  `PLAN_REJECTED.md`, `statefiles/wbs/`) are ignored; delete them when
  convenient.
- Work the statefiles record as done but the overseer never validated is
  no longer adopted from them. Finish it under the old release first, or
  let the team redo it.
- `project_state_authority` and `statefile_hashes` are kept as history;
  nothing reads or writes them any more.

Operations: `docs/runbook.md`, "MCP server".
