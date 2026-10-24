# Overseer runbook

Operational procedures for an overseer instance and the recovery conditions an execution can stop in. The decision tables behind the recovery entries (safe and unsafe actions, automatic handling, crash points) are generated in [`recovery-decision-table.md`](recovery-decision-table.md); the metrics referred to below are listed in [`metrics.md`](metrics.md).

Endpoints are under `/api/v1`. Recovery actions are taken from the execution's recovery page in the console, or with
`POST /api/v1/projects/{project}/executions/{execution}/recovery/actions` and a body such as `{"action": "MANUAL_RESUME", "reason": "…"}`.
`GET …/recovery/plan` shows the planner's view (condition, safe actions, evidence) before you act. Every action is audited.

## Startup

1. Set the configuration: `OVERSEER_CONFIG_PATH` (YAML) and secrets in the environment (`OVERSEER_DATABASE_PASSWORD`, provider keys). In the `production` profile `OVERSEER_BACKUP_DIR` must be set.
2. Start `./overseer`. It connects to PostgreSQL, takes a pre-migration backup when migrations are pending (production), applies `migrations/`, then starts the web console, the scheduler and the monitors.
3. Check `GET /health` (process up) and `GET /ready` (database reachable, migrations applied, no blocking gate). `GET /api/v1/system/readiness` explains a NOT_READY result.
4. On startup every non-terminal execution is scanned; executions that need a human show `RECOVERY_REQUIRED` with their condition.

## Shutdown

Send `SIGTERM` (or `SIGINT`). The overseer stops admitting new work, lets in-flight worker requests and validation runs finish within the shutdown timeout (`shutdown=30s` by default), releases its session leases and closes the database pool. A request still in flight when the timeout expires is reconciled at the next start (`INFLIGHT_DISPATCH` / `DELIVERY_UNKNOWN`); it is never resent blindly.

## Upgrade

1. Take a backup: `./overseer backup --dir DIR`.
2. Stop the running instance (see Shutdown).
3. Replace the binary and the `migrations/` directory with the new release.
4. Start it. Pending migrations are applied at startup (in production after an automatic `pre_migration` backup). A failed migration stops startup; restore the backup and start the previous release.
5. Executions keep the configuration snapshot they started with; new settings apply to new executions.

## Backup

`./overseer backup --dir DIR [--reason manual|pre_migration]` writes a logical backup (every table plus a manifest with the schema version and row counts) to `DIR`. It reads in one consistent snapshot and can run while the overseer is up. Keep backups outside the database host. Project working directories and their git repositories are not part of the backup; back them up with the repositories themselves.

## Restore

1. Create an empty database and point the configuration at it.
2. `./overseer restore --dir DIR [--migrations DIR]` loads the backup and validates row counts and the schema version.
3. Start the overseer. A restored database starts read-only: the restore checks run (`schema_compatibility`, `integrity_scan`, `audit_chain`, `recovery_scan`), mutation routes are refused and no worker request is sent, even when every check passes. `GET /api/v1/system/restore` shows the checks.
4. When the checks pass and you have reviewed them, enable execution: `POST /api/v1/system/restore/enable` with `{"reason": "…"}`. A failing check keeps the instance NOT_READY; fix the cause and restart to re-check.

## Point-in-time recovery

The overseer's writes are transactional and every side effect outside the database (worker dispatches, checkpoints, materialized files) is reconciled from database rows, so a PostgreSQL point-in-time recovery (base backup plus WAL replay) is a valid restore source:

1. Recover the cluster to the chosen time with PostgreSQL's own tooling.
2. Start the overseer against it and treat it exactly like a restore (step 3 of Restore): review the restore checks before enabling execution.
3. Work done after the recovery point is unknown to the database. Dispatches recorded as in flight come back as `INFLIGHT_DISPATCH` or `DELIVERY_UNKNOWN`, repository changes as `REPOSITORY_HEAD_CHANGED`; resolve them through the normal recovery entries below.

## Worker outage

When the Hermes gateway is unreachable, the affected executions wait (`PROVIDER_UNAVAILABLE`) and resume automatically once the gateway answers; a request that never left is resent (`DISPATCH_NOT_SENT`). A request whose delivery is uncertain is never resent automatically (`DELIVERY_UNKNOWN`). Check the gateway, its token (`auth_secret_ref` / file) and `GET /api/v1/system/health`; nothing needs to be done in the overseer once the gateway is back.

## Database outage

With the database unavailable no side effect is started: the engine needs a transaction before every worker request, validation run or checkpoint. `GET /ready` reports NOT_READY. Restore the database (or connectivity); the overseer reconnects and executions continue from their persisted state. If the database had to be restored from a backup, follow Restore.

## Disk full

At the warning threshold (`disk.warn_percent`) a `DISK_THRESHOLD` alert is raised. At the critical threshold (`disk.critical_percent`, 95 % by default) the work gate blocks new worker requests, validation runs and checkpoints; executions wait. A write that fails with "no space left on device" stops the execution in `DISK_FULL`. Free space on the volume holding the working directories, `/tmp` and the backup directory, then resume (see `DISK_FULL` below).

## DELIVERY_UNKNOWN

A worker request may or may not have reached the worker: the connection broke after the request was written and no reply arrived. Never resend it. See the `DELIVERY_UNKNOWN` entry below.

## Repository mismatch

The working directory no longer matches what the execution recorded: the repository is missing, has a different identity, or HEAD moved outside the overseer. See `REPOSITORY_MISSING`, `REPOSITORY_IDENTITY_MISMATCH`, `REPOSITORY_HEAD_CHANGED` and `REPOSITORY_PERMISSION` below. Inspect the repository before resuming; resending work into the wrong repository is unsafe.

## Checkpoint recovery

A required git checkpoint failed, was rejected (e.g. by a hook) or its outcome is uncertain. A task is never marked DONE without its checkpoint. See `PENDING_CHECKPOINT`, `CHECKPOINT_REJECTED` and `CHECKPOINT_AMBIGUOUS` below.

## Audit verification failure

An INVALID or INCOMPLETE audit-chain verification is treated as a security incident: `AUDIT_VERIFICATION_FAILED` is logged and alerted, readiness reports NOT_READY and the work gate blocks new work.

1. Run `GET /api/v1/audit/verify` and keep the report.
2. Investigate how the audit rows changed (database access logs, restores, manual edits).
3. When the cause is understood, acknowledge that exact failure: `POST /api/v1/audit/verify/acknowledge` with `{"reason": "…", "fingerprint": "<from the report>"}`. The acknowledgement is itself audited; a different later failure needs its own acknowledgement.

## MCP server

The overseer MCP server (Phase 19) is how Hermes reads its working context and submits every result of the statefile workflow; no statefile is read or written (docs/state-authority.md). It is off by default; bootstrap readiness refuses to start a statefile-workflow execution without it ("MCP server" check, `CONFIG_INVALID`).

1. Choose a bearer token and export it to the overseer (for example `OVERSEER_MCP_KEY`). Set `mcp.enabled: true`, `mcp.addr` (for example `0.0.0.0:9142` on the devpipe network; never expose it beyond the Hermes container) and `mcp.auth_secret_env: OVERSEER_MCP_KEY`, then restart. `GET /healthz` on the MCP port answers without the token.
2. In the Hermes `config.yaml`, add the server under `mcp_servers`: `url: http://<overseer container>:9142/mcp` with `headers: {Authorization: "Bearer <token>"}`. Restart Hermes; its banner lists the overseer tools.
3. Each command carries a capability token that only works while that command's dispatch is in flight. A result that is missing when the reply arrives (`MCP_RESULT_MISSING`) gets the command resent with the problem, up to the resend limit, then recovery.

Follow-up actions (blocker notifications, context resets) are committed to `workflow_outbox` with the request that caused them. The dispatcher retries them with backoff and marks them `DEAD` after the last attempt. A `DEAD` row is logged at error level; inspect its `last_error`.

## Recovery conditions

### `ARTIFACT_CORRUPT`

A stored artifact (prompt, response, snapshot) fails its integrity hash. Find out which artifact and why (`overseer check --execution ID`), restore it from a backup if needed, then `MANUAL_RESUME`. Do not resend.

### `CHECKPOINT_AMBIGUOUS`

The checkpoint commit may or may not exist. Inspect the repository: if HEAD is the checkpoint (look for the overseer trailer), `ACCEPT_REPOSITORY_HEAD`; otherwise fix the repository and `MANUAL_RESUME`. Do not retry the checkpoint blindly; it may create a duplicate commit.

### `CHECKPOINT_REJECTED`

Git refused the checkpoint (a hook, a protected branch, a signing requirement). Fix the cause in the repository, then `RETRY_CHECKPOINT`, or `MANUAL_RESUME` if you committed it yourself.

### `CONFIG_DRIFT`

The project configuration changed after the execution's snapshot. The execution keeps its snapshot; it resumes automatically (`RESUME`).

### `DELIVERY_UNKNOWN`

The request may have been delivered. Run `RECONCILE_DISPATCH` to ask the worker; inspect the repository (`VALIDATE_REPOSITORY_STATE`) for the work the request asked for, then record what happened with `RESOLVE_DELIVERY`. Never `RESEND` or `MANUAL_RESUME` past it: the work could be done twice.

### `DISK_FULL`

A write failed because the disk is full. Free space (see Disk full), then `MANUAL_RESUME`.

### `DISPATCH_NOT_SENT`

The request provably never left the overseer. It is resent automatically (`RESEND`); no action is needed.

### `EXECUTION_TERMINAL`

The execution is COMPLETED, FAILED or CANCELLED. Nothing can be resumed; start a new execution.

### `EXPIRED_LEASE`

The session lease of a stopped instance expired. Resumed automatically (`RESUME`).

### `INFLIGHT_DISPATCH`

A request was delivered and the reply has not arrived yet. The overseer reconciles it automatically (`RECONCILE_DISPATCH`) or waits (`WAIT`). Do not resend.

### `INVARIANT_VIOLATION`

A database invariant does not hold (e.g. two active tasks). Run `overseer check --execution ID` for the report, repair the data from a backup or by hand with care, then `MANUAL_RESUME`. Do not `RESEND` or `RESUME` before the report is clean.

### `MANUAL_CRITERIA`

A task has `[MANUAL]` acceptance criteria that need a human. Verify them and `APPROVE_CRITERIA` from the recovery page.

### `MANUAL_REVIEW`

A limit was exhausted (fix attempts, planning attempts, resends of a missing or malformed result) or the engine needs a decision. Read the reason, the last command and the reply on the recovery page, fix the cause (often the task or the team's MCP connection), then `MANUAL_RESUME`.

### `ORPHAN_PRECOMPUTE`

A speculative precomputation outlived its execution step. Discarded automatically (`DISCARD_SPECULATIVE`).

### `PANIC`

The engine panicked during a step. Look at the logged stack and the step; `RECONCILE_DISPATCH` if a request was in flight, then `MANUAL_RESUME`. Do not resend.

### `PENDING_CHECKPOINT`

A required checkpoint did not finish (e.g. the git lock was held). Retried automatically (`RETRY_CHECKPOINT`). If it keeps failing, check `.git/index.lock` and repository permissions.

### `PROMPT_TOO_LARGE`

The prompt does not fit the worker's context window, even after compression. Reduce the inputs (WBS, role instructions) or raise the context window, then `MANUAL_RESUME`.

### `PROTOCOL_VIOLATION`

The worker's reply broke the response protocol repeatedly. Check the model and the gateway, then `MANUAL_RESUME`.

### `PROVIDER_UNAVAILABLE`

The worker or compression provider is unreachable. The execution waits (`WAIT`) and resumes when the provider answers (see Worker outage).

### `REPOSITORY_HEAD_CHANGED`

HEAD moved outside the overseer since the last checkpoint. Review the new commits; if they are intended, `ACCEPT_REPOSITORY_HEAD`. Do not resend before reviewing them.

### `REPOSITORY_IDENTITY_MISMATCH`

The working directory holds a different repository than the one recorded. Put the right repository back (or correct the working directory), then `MANUAL_RESUME`.

### `REPOSITORY_MISSING`

The working directory or its repository is gone. Restore it, then `MANUAL_RESUME`.

### `REPOSITORY_PERMISSION`

The overseer cannot read or write the repository. Fix ownership and permissions for the overseer's user, then `MANUAL_RESUME`.

### `RESPONSE_UNPERSISTED`

A reply arrived but was not stored before a crash. `RECONCILE_DISPATCH`, check the repository (`VALIDATE_REPOSITORY_STATE`), then `RESOLVE_DELIVERY`. Do not resend.

### `RESUMABLE`

The execution stopped cleanly and can continue. Resumed automatically (`RESUME`).

### `RUNNING_VALIDATION`

A validation run was interrupted. It is abandoned (never PASS or FAIL) and rerun automatically (`RUN_VALIDATION`).

### `VALIDATION_INFRA`

The validation run itself failed (a missing program, a timeout, the environment), not the tests. Retried automatically within its retry policy (`RUN_VALIDATION`); when retries run out, fix the environment and `MANUAL_RESUME`.
