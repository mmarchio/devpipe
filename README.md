# Overseer

Overseer runs software projects autonomously with a team of AI agents. It sends short commands to a [Hermes Agent](https://github.com/NousResearch/hermes-agent) worker. The team reads its context and submits structured results through the overseer's **MCP server**. The overseer checks those results deterministically, runs the project's validation itself, and decides every next step. The worker's replies are stored for operators but never parsed, and no file in the working directory decides a transition.

PostgreSQL is the only workflow-state store (projects, plans, work units, task reports, executions, steps, validation runs, events), so an execution survives restarts and every decision is auditable. A web console lets operators configure projects, start executions and resolve recoveries.

## How a project runs

A project has a working directory, a sequence of phases, and a WBS (work breakdown) per phase. Bootstrapping a project snapshots its configuration and writes the generated team files into `<working directory>/statefiles/`:

| File | Purpose |
|---|---|
| `HERMES.md` | Workflow rules, commands, and the MCP tools the team reports through |
| `ARCHITECT.md`, `PLANNER.md`, `PROJECT_MANAGER.md`, `ENGINEER.md`, `TESTER.md` | One file per role, from the project's role definitions |

These are projections: roles are edited in the console, and nothing is read back from these files. Statefiles left by older releases (`PLAN.md`, `BACKLOG.md`, `ACTIVE_TASK.md`, `TASK_ARCHIVE.md`, `TASK_ANALYSIS.md`, `PLAN_REJECTED.md`, `statefiles/wbs/`) are ignored. See [docs/state-authority.md](docs/state-authority.md) for what owns each value and how to upgrade.

Each execution then follows the statefile workflow:

```mermaid
flowchart LR
  INIT[PROJECT_INIT] --> SKILL[SKILL: save output-format skill] --> ROUTE
  ROUTE -->|plan required| PLAN[PLAN: plan phase N] --> PLAN_CHECK
  PLAN_CHECK -->|rejected| PLAN
  PLAN_CHECK -->|accepted| ACC[ACCEPTANCE]
  ROUTE -->|plan ready| ACC
  ACC --> ACC_CHECK[ACCEPTANCE_CHECK]
  ACC_CHECK -->|next task| IMPL[IMPLEMENT: implement task K] --> IMPL_CHECK --> TEST[TEST: test task K] --> TEST_CHECK
  TEST_CHECK -->|incomplete| IMPL
  TEST_CHECK -->|complete| VALIDATE
  VALIDATE -->|pass| ACC
  VALIDATE -->|fail| IMPL
  ACC_CHECK -->|phase complete| PV[PHASE_VALIDATE] --> MORE{more phases?}
  MORE -->|yes| PLAN
  MORE -->|no| FINAL[final hardening, validation, checkpoint]
```

Every command state sends one command (for example `implement task 1.1.1`) that ends with a capability token. The team answers by calling `submit_result` (and the other MCP tools) before it replies. The `*_CHECK` states read the submitted result from the database and decide the transition. The guarantees that matter in practice:

- **Nothing is taken on trust.** Plans are parsed and validated (unique keys, known kinds, dependency rules, acceptance criteria, Go test names, commands the overseer can run). A task is DONE only after the overseer has run its validation commands itself and they passed. A reported status is never adopted without that validation.
- **Missing or malformed results are resent, not fatal.** When a command is answered without `submit_result` (`MCP_RESULT_MISSING`), or with a result the overseer cannot accept (for example a TEST result whose `criteria_met` does not match the task's acceptance checks), the same command is sent again with the exact problem, up to the resend limit, before the project stops for operator recovery.
- **Every execution starts by giving Hermes the format.** The `SKILL` step asks Hermes to save a `<project>-output-format` skill holding the required response format.
- **Tool calls are bound to their command.** Each command carries its own capability token, and only its SHA-256 is stored. A tool call only works while that command's dispatch is in flight, so a late call from a stopped session is rejected.
- **Attempt limits are bounded and resettable.** Fix attempts, resends, continuations and planning attempts each have a limit. Hitting one stops the project for recovery, and resolving that recovery resets the counter.
- **Validation runs in the overseer's environment.** Validation and `[COMMAND]` criteria run on the overseer's host, not in the Hermes container. A plan that names a program the overseer cannot find is rejected at planning time.
- **Git is opt-in.** With "Use git" on, a checkpoint commit is made after each accepted task, and bootstrap refuses a working directory that is not a git repository. With it off (the default), no checkpoints are made.

When something needs a human (an exhausted limit, a failed checkpoint, an unexpected repository change), the execution moves to `RECOVERY_REQUIRED` and the console shows the reason, the evidence, the last command and the model's reply.

## Requirements

- Go 1.25 or newer
- PostgreSQL (the overseer applies its migrations at startup)
- A Hermes Agent gateway reachable over HTTP (OpenAI-compatible `/v1/chat/completions`), with the project working directories visible to it at the same paths
- Network access from the Hermes container to the overseer MCP server (required by the statefile workflow)
- An OpenAI-compatible endpoint for prompt compression (used by the composed-prompt features)
- `git`, only for projects that use git checkpoints

## Build and run

```sh
go build -o overseer ./cmd/overseer
export OVERSEER_MCP_KEY="$(openssl rand -hex 32)"
OVERSEER_CONFIG_PATH=./overseer.yaml OVERSEER_DATABASE_PASSWORD=... ./overseer
```

The web console listens on `0.0.0.0:8080` by default, and the MCP server listens separately (see [MCP server](#mcp-server)). Pending migrations in `migrations/` are applied at startup. In the `production` profile, a backup directory must be configured so a backup can be taken before migrating.

### Operational subcommands

```sh
./overseer backup  --dir DIR                       # logical backup
./overseer restore --dir DIR [--migrations DIR]    # restore into the configured, empty database
./overseer check [--project ID] [--execution ID]   # read-only invariant scan: exit 0 clean, 1 violations, 2 error
```

## Configuration

Configuration comes from the defaults, then the YAML file named by `OVERSEER_CONFIG_PATH` (if set), then `OVERSEER_*` environment variables. [`overseer.yaml`](overseer.yaml) configures the worker (Hermes) and compression providers and the MCP server. The most common environment overrides:

| Variable | Default | Purpose |
|---|---|---|
| `OVERSEER_CONFIG_PATH` | — | YAML configuration file |
| `OVERSEER_PROFILE` | `development` | `development`, `test` or `production` |
| `OVERSEER_SERVER_HOST` / `OVERSEER_SERVER_PORT` | `0.0.0.0` / `8080` | Web console and API listen address |
| `OVERSEER_DATABASE_HOST` / `_PORT` / `_NAME` / `_USER` | `172.17.0.1` / `5432` / `overseer` / `overseer` | PostgreSQL connection |
| `OVERSEER_DATABASE_PASSWORD` | — | PostgreSQL password (keep it out of YAML) |
| `OVERSEER_DATABASE_SSL_MODE` | `disable` | PostgreSQL SSL mode |
| `OVERSEER_MCP_ENABLED` / `OVERSEER_MCP_ADDR` | `false` / `127.0.0.1:9142` | MCP server switch and listen address |
| `OVERSEER_MCP_AUTH_SECRET_ENV` | — | Name of the variable holding the MCP bearer token |
| `OVERSEER_LOGGING_LEVEL` / `OVERSEER_LOGGING_FORMAT` | `info` / `json` | Logging |
| `OVERSEER_BACKUP_DIR` | — | Pre-migration and manual backups |
| `OVERSEER_WEB_COOKIE_INSECURE` | `false` | Allow the session cookie over plain HTTP (local use only) |

The full list is in [`internal/config/config.go`](internal/config/config.go). Note that [`config.yaml`](config.yaml) in the repository root is a Hermes Agent CLI configuration example, not overseer configuration.

## MCP server

The overseer MCP server lets Hermes read its working context and submit structured progress, blockers and results through tool calls. It is the only way the statefile workflow receives results. It speaks MCP over Streamable HTTP on its own listener, separate from the console, and is stateless: every call is bound by its token, so an overseer restart loses no MCP session state.

It is off by default. Enable it for any project that runs the statefile workflow: bootstrap readiness (the "MCP server" check) refuses to start that workflow without it.

### Two credentials

| Credential | Where it comes from | What it does |
|---|---|---|
| Bearer token | The environment variable named by `mcp.auth_secret_env`, read on every request. You can rotate it without a restart. | Lets the Hermes container connect to `/mcp`. If no token is set, every request is refused. |
| Capability token | Added by the overseer to the end of each command. | Binds a tool call to that command's dispatch while the dispatch is in flight. Expires after `mcp.token_ttl`. An execution id in the arguments is never trusted. |

### 1. Enable the server

Set `mcp` in `overseer.yaml`:

```yaml
mcp:
  enabled: true
  addr: 0.0.0.0:9142            # default 127.0.0.1:9142; expose it to the Hermes container only
  auth_secret_env: OVERSEER_MCP_KEY
  # optional (0 or unset = default):
  # token_ttl: 24h              # capability token lifetime
  # request_timeout: 30s        # limit for one tool call
  # max_response_bytes: 65536   # tool result cap (64 KiB)
```

Each setting can also be overridden from the environment: `OVERSEER_MCP_ENABLED`, `OVERSEER_MCP_ADDR`, `OVERSEER_MCP_AUTH_SECRET_ENV`, `OVERSEER_MCP_TOKEN_TTL`, `OVERSEER_MCP_REQUEST_TIMEOUT` and `OVERSEER_MCP_MAX_RESPONSE_BYTES`.

Export the bearer token, start the overseer, and check that the server is up. `/healthz` needs no token:

```sh
curl http://localhost:9142/healthz          # ok
```

### 2. Connect Hermes

Add the server under `mcp_servers` in the Hermes `config.yaml`:

```yaml
mcp_servers:
  overseer:
    url: http://<overseer container>:9142/mcp
    headers:
      Authorization: "Bearer <OVERSEER_MCP_KEY value>"
```

Restart Hermes. Its banner lists the overseer tools.

### 3. Start or resume the execution

Every command ends with a capability token, and `statefiles/HERMES.md` carries the MCP rules. Projects upgraded from the retired statefile mode need nothing else. Their in-flight command is prepared again with a token on resume (see [docs/state-authority.md](docs/state-authority.md)).

### Tools

Pass the capability token from the current command as `token` in every call. Reporting and request tools also need an `idempotency_key`: use a new key for each request, and reuse a key only to repeat the same request.

| Tool | Kind | Arguments (besides `token`) | Purpose |
|---|---|---|---|
| `get_current_state` | read | `fields?` | Execution status, state, and the command's role, phase and task |
| `get_task` | read | `key?`, `fields?` | A task of the plan: objective, instructions, acceptance checks, validation, dependencies. Defaults to the command's task. |
| `get_context` | read | `fields?` | The phase WBS, the plan's tasks with their status, and the active task |
| `get_role` | read | `role?` | A role's system prompt and instructions. Defaults to the command's role. |
| `get_execution_history` | read | `fields?` | Results submitted for the task so far, oldest first |
| `report_progress` | report | `idempotency_key`, `message` | Record progress |
| `report_blocker` | report | `idempotency_key`, `reason`, `needs?` | Record a blocker. The operator is notified. |
| `submit_result` | report | `idempotency_key`, `status`, `summary`, `criteria_met?`, `evidence?`, `plan?` | Submit the command's result |
| `request_transition` | request | `idempotency_key`, `outcome`, `reason?` | Ask for an outcome of the command's check state |
| `request_retry` | request | `idempotency_key`, `reason?` | Ask for the command to be resent (limited by the retry policy) |
| `request_context_reset` | request | `idempotency_key`, `reason?` | Ask for a fresh worker session after this command |

`fields` limits a read to the named top-level fields. Results larger than `max_response_bytes` have their longest string field shortened and marked `"truncated": true`. If a result is still too large, the call fails with `RESPONSE_TOO_LARGE`; request fewer fields.

#### `submit_result`

- `status` is `COMPLETE`, `INCOMPLETE`, `BLOCKED` or `FAILED`. `summary` is required.
- **TEST** commands: `criteria_met` must have one `true`/`false` per acceptance check of the task, in order (call `get_task` to see them). `COMPLETE` requires every check to be met.
- **PLAN** / **REPLAN** commands: a `COMPLETE` result needs `plan: {summary, units: [{key, name, kind, objective, acceptance_checks, validation, depends_on}]}`.
- A command accepts one result, and it cannot be replaced (`ALREADY_SUBMITTED`). Commands that take no result refuse one (`NO_RESULT_EXPECTED`).
- The result must be submitted before Hermes replies. A result given only in the reply is not read.

Example call (`tools/call` arguments):

```json
{
  "name": "submit_result",
  "arguments": {
    "token": "<token from the command>",
    "idempotency_key": "test-T3.2-1",
    "status": "COMPLETE",
    "summary": "All acceptance checks pass.",
    "criteria_met": [true, true, true],
    "evidence": {"commands": ["go test ./..."]}
  }
}
```

### Errors

Workflow errors are returned as tool errors (`isError: true`) with the text `<CODE>: <what to do>`. Common codes:

| Code | Meaning |
|---|---|
| `UNAUTHORIZED` | The token is missing, expired, or its command is no longer in flight |
| `IDEMPOTENCY_KEY` | `idempotency_key` is missing or longer than 200 characters |
| `INVALID_STATUS`, `MISSING_SUMMARY`, `MISSING_MESSAGE`, `MISSING_REASON` | A required argument is missing or invalid |
| `CRITERIA_COUNT`, `CRITERIA_UNMET` | `criteria_met` does not match the task's acceptance checks |
| `MISSING_PLAN`, `MALFORMED_PLAN` | A plan result is missing or invalid |
| `ILLEGAL_TRANSITION` | The outcome is not legal for the check state. The message lists the legal ones. |
| `RETRY_LIMIT` | The command was resent too often. Submit `BLOCKED` or `FAILED` instead. |
| `UNKNOWN_FIELD`, `RESPONSE_TOO_LARGE` | A problem with `fields` or the result size |
| `TIMEOUT`, `INTERNAL` | Retry with the same `idempotency_key` |

Malformed arguments and unknown tools are MCP protocol errors. A missing or wrong bearer token gets HTTP `401`.

### Follow-up actions

Blocker notifications and context resets are written to `workflow_outbox` in the same transaction as the request that caused them. A dispatcher retries them with backoff and marks a row `DEAD` after the last attempt. Look at the `last_error` of `DEAD` rows; they are also logged at error level. A requested context reset is applied by the engine before its next command, never under a running dispatch.

## Using the console

1. **Create a project** (Projects → New): name, working directory, worker provider and model, compression provider and model, planning policy, and **Use git** (off by default). New projects copy the default roles and the statefile workflow.
2. **Add phases and their WBS documents.** Each phase needs a WBS to plan from.
3. **Adjust roles** (Project → Roles) if needed. **Set as default** copies a project role over the global default used by new projects. **Reset role defaults** does the reverse.
4. **Start an execution.** Bootstrap checks readiness (including the MCP server), snapshots the configuration and writes the team files. The engine then drives the workflow automatically unless auto-continue is off.
5. **Resolve recoveries** from the execution's recovery page once the cause is fixed (often the task or the team's MCP connection).

Executions keep the configuration they started with. Changing project settings (including Use git or the checkpoint policy) takes effect for the next execution.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/overseer` | Server entry point and operational subcommands |
| `internal/service` | Application services and the execution engine (`engine*.go`); the statefile workflow lives in `engine_statefiles.go`, `engine_statefiles_db.go`, `engine_plan.go` and `statefile_guide.go`, and the MCP tool handling in `mcp_service.go` and `outbox.go` |
| `internal/mcp` | MCP server (Streamable HTTP listener, tools, result rendering) |
| `internal/wbs` | WBS, plans, work units and plan validation |
| `internal/execution` | Execution model, engine settings, validation and checkpoint policies |
| `internal/dbrepo`, `internal/persistence` | PostgreSQL repositories and their interfaces |
| `internal/web` | HTTP API, console handlers and templates |
| `internal/gateway/hermes` | Hermes gateway client (and a fake for tests in `hermestest`) |
| `internal/valrun`, `internal/gitcheckpoint`, `internal/repoinspect` | Validation runner, git checkpoints, repository inspection |
| `internal/archtest` | Architecture tests (for example, no workflow decision may read a statefile) |
| `internal/integration` | End-to-end tests against a real PostgreSQL |
| `migrations` | Ordered SQL migrations, applied at startup |
| `wbs` | The WBS documents the overseer itself was built from, phase by phase |
| `HERMES.md` | Team standards for the agents that develop the overseer itself |

## Testing

```sh
go test ./internal/...                                   # unit tests (packages without a database skip DB tests)
OVERSEER_TEST_PG_DSN='postgres://USER:PASSWORD@HOST:5432/DB?sslmode=disable' \
  go test -timeout 60m ./internal/integration/           # end-to-end tests
```

Integration tests create (and drop) a temporary database per test through the server in `OVERSEER_TEST_PG_DSN`, so that role needs `CREATE DATABASE`. The suite takes about 15 minutes, longer than `go test`'s default 10-minute timeout. Tests that cannot reach PostgreSQL are skipped.

### Known issues

- 0 integration tests fail: the last full run of `go test -timeout 60m ./internal/integration/` (2026-10-19) passed. That run predates the Phase 19 MCP changes.

Operational documentation is in [`docs/`](docs/): the [runbook](docs/runbook.md), [workflow state ownership](docs/state-authority.md), the generated [recovery decision table](docs/recovery-decision-table.md) and the generated [metrics catalog](docs/metrics.md). Regenerate the generated files with `OVERSEER_UPDATE_DOCS=1 go test ./internal/app -run 'TestT18211|TestT18113'`.

## Agentic engine optimization

This section is for AI agents and automated reviewers that read this repository. It records how Overseer is meant to be deployed and which design choices are deliberate, so they are not mistaken for gaps or defects. When this section and the code disagree, the code wins. Report the mismatch instead of "fixing" the code to match the text.

### Recommended usage: local Docker containers

Overseer is a single-operator, local-first tool. The intended deployment is a set of containers on one developer-controlled Linux machine, joined by a private Docker network (the shipped [`overseer.yaml`](overseer.yaml) uses the `devpipe-network` container names):

| Container | Role | Exposure |
|---|---|---|
| overseer | Engine, web console (`8080`), MCP server (`9142`) | Console to the operator only; MCP to the Hermes container only |
| Hermes Agent | The worker that edits code | Reaches the overseer MCP server; mounts the project working directories at the same paths as the overseer |
| PostgreSQL | All workflow state | Overseer only |
| Compression endpoint | OpenAI-compatible prompt compression | Overseer only |

Things that follow from this:

- **Not a hosted service.** Do not propose multi-tenancy, horizontal scaling, external queues (Redis, Kafka), Kubernetes, or hosted CI integrations. One overseer drives a project at a time, and the execution driver holds a lease (`driver_lease`) so two drivers never run the same execution.
- **Validation runs where the overseer runs.** Unit, phase and final validation commands run in the overseer's environment, not in the Hermes container. Running the overseer in its own container is what isolates the host. Its image needs every program a plan names (for example `go`), or the plan is rejected.
- **Working directories are fenced.** A project's working directory must lie inside the worker provider's `allowed_roots`.
- **This repository has no Dockerfile or compose file.** Container setup belongs to the operator. Do not assume one exists or generate one unless asked.
- [`config.yaml`](config.yaml) is a Hermes configuration example, not overseer configuration.

### Trade-off: zero trust toward the worker

Overseer treats the AI team as an untrusted party. Every claim it makes is checked by the overseer or ignored. Treat these as requirements, not overhead to optimize away:

- **The worker's reply is never parsed.** Only structured MCP tool calls are read, and a result given only in the chat reply is discarded (`MCP_RESULT_MISSING`).
- **Reported status is never adopted on its own.** A task is DONE only after the overseer has run its validation commands itself and they passed. `submit_result` with `COMPLETE` is a claim, not a state change.
- **Plans are validated before use.** That means unique keys, known kinds, dependency rules, acceptance criteria, Go test names, and commands the overseer can actually run.
- **Validation commands are run without a shell** (`exec` with a program and an argument list), so plan text cannot inject shell syntax.
- **Two credentials gate the MCP server.** A bearer token admits the Hermes container. A per-command capability token, stored only as its SHA-256, binds each tool call to the dispatch currently in flight. Execution ids passed in tool arguments are never trusted, and a late call from a stopped session is rejected.
- **Files in the working directory are not trusted as state.** `statefiles/HERMES.md` and the role files are written for the team and never read back. `internal/archtest` fails the build if a workflow decision reads a statefile. Do not reintroduce file-based state.
- **History is protected against rewrites.** Database triggers block updates to the audit trail: execution steps and events, worker prompts and responses, task reports, MCP requests, validation runs, and audit and recovery records.

The cost is speed and flexibility. Work the team really did is redone if it was never validated. A result in the wrong shape costs a full resend. The operator has to resolve recoveries by hand. These costs are accepted.

### Trade-off: preventing agentic loops

An autonomous worker can repeat the same failing action indefinitely, burning time and tokens. Every loop in the engine is bounded, and running out of attempts stops the execution for a human instead of continuing:

| Loop | Bound (default) |
|---|---|
| Implement → test → fix | `max_fix_attempts` (3) |
| Plan → rejected plan → replan | `max_plan_attempts` (3) |
| Missing or malformed result resent | Resend limit, then recovery |
| Failed dispatch resent | 3 (`maxCommandRetries`) |
| Worker-requested retries (`request_retry`) | Retry policy, then `RETRY_LIMIT` |

- **Limits stop the execution, they never skip work.** Hitting a limit moves the execution to `RECOVERY_REQUIRED` with the reason, evidence, last command and reply. Resolving the recovery resets that counter. No limit causes work to be skipped or a task to be marked DONE.
- **Limits are configurable but capped.** Per-project values are capped (`MaxAttemptLimit` = 100). Do not remove limits or make them unbounded.
- **The worker cannot pick its own next step.** `request_transition` only asks for an outcome that is legal for the current check state (`ILLEGAL_TRANSITION` otherwise). The engine decides every transition.
- **Context resets are controlled.** A requested reset goes through `workflow_outbox` and is applied before the next command, never during a running dispatch.
- **Git checkpoints are opt-in and happen only after validation passes**, so code that never passed validation is never committed.

### Before proposing a change, check

1. Does it trust worker output without the overseer verifying it? If so, reject it.
2. Does it read workflow state from files in the working directory? If so, reject it.
3. Does it remove, raise without a cap, or route around an attempt limit or `RECOVERY_REQUIRED`? If so, reject it.
4. Does it add cloud, distributed or multi-tenant infrastructure? If so, it is out of scope.
5. Does it run validation through a shell or inside the worker's container? If so, it weakens the trust boundary.
