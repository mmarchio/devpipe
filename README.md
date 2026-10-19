# Overseer

Overseer runs software projects autonomously with a team of AI agents. It sends short commands to a [Hermes Agent](https://github.com/NousResearch/hermes-agent) worker, reads the results back from files the team writes in the project, checks them deterministically, runs the project's validation itself, and decides every next step. The worker's replies are stored for operators but never parsed: what the overseer acts on is the files it can verify.

PostgreSQL holds all state (projects, plans, work units, executions, steps, validation runs, events), so an execution survives restarts and every decision is auditable. A web console lets operators configure projects, start executions and resolve recoveries.

## How a project runs

A project has a working directory, a sequence of phases, and a WBS (work breakdown) per phase. Bootstrapping a project snapshots its configuration and writes the team files into `<working directory>/statefiles/`:

| File | Written by | Purpose |
|---|---|---|
| `HERMES.md` | overseer | Workflow rules, commands, task file format |
| `ARCHITECT.md`, `PLANNER.md`, `PROJECT_MANAGER.md`, `ENGINEER.md`, `TESTER.md` | overseer | One file per role, from the project's role definitions |
| `wbs/phase-NN.md` | overseer | The approved WBS of each phase |
| `PLAN.md` | ARCHITECT | The phase plan |
| `BACKLOG.md` | PLANNER | Every task of the phase, in execution order |
| `ACTIVE_TASK.md` | PROJECT_MANAGER | The one task being worked on |
| `TASK_ARCHIVE.md` | PROJECT_MANAGER | Finished tasks |
| `PLAN_REJECTED.md` | overseer | Problems of the last rejected plan (removed once a plan is accepted) |

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

Every command state sends one command (for example `implement task 1.1.1`). The `*_CHECK` states read the statefiles back and decide the transition. The guarantees that matter in practice:

- **Nothing is taken on trust.** Plans are parsed and validated (unique keys, known kinds, dependency rules, acceptance criteria, Go test names, commands the overseer can run). A task is DONE only after the overseer has run its validation commands itself and they passed. Statuses written in the files are never adopted without that validation.
- **Formatting errors are resent, not fatal.** When the team writes a file the overseer cannot accept, the same command is sent again with the exact problem and the full expected format appended, at most 3 times, before the project stops for operator recovery. This includes a test command whose result was never written to `ACTIVE_TASK.md`.
- **Every execution starts by giving Hermes the format.** The `SKILL` step asks Hermes to save a `<project>-output-format` skill holding the required response and file format.
- **Verified files are hashed.** The MD5 of each statefile the overseer verifies is stored per execution, so later checks know whether a file was created, changed or left untouched.
- **Attempt limits are bounded and resettable.** Fix attempts, resends, continuations and planning attempts each have a limit. Hitting one stops the project for recovery, and resolving that recovery resets the counter.
- **Validation runs in the overseer's environment.** Validation and `[COMMAND]` criteria run on the overseer's host, not in the Hermes container. A plan that names a program the overseer cannot find is rejected at planning time.
- **Git is opt-in.** With "Use git" on, a checkpoint commit is made after each accepted task, and bootstrap refuses a working directory that is not a git repository. With it off (the default), no checkpoints are made.

When something needs a human (an exhausted limit, a failed checkpoint, an unexpected repository change), the execution moves to `RECOVERY_REQUIRED` and the console shows the reason, the evidence, the last command and the model's reply.

## Requirements

- Go 1.25 or newer
- PostgreSQL (the overseer applies its migrations at startup)
- A Hermes Agent gateway reachable over HTTP (OpenAI-compatible `/v1/chat/completions`), with the project working directories visible to it at the same paths
- An OpenAI-compatible endpoint for prompt compression (used by the composed-prompt features)
- `git`, only for projects that use git checkpoints

## Build and run

```sh
go build -o overseer ./cmd/overseer
OVERSEER_CONFIG_PATH=./overseer.yaml OVERSEER_DATABASE_PASSWORD=... ./overseer
```

The web console listens on `0.0.0.0:8080` by default. Pending migrations in `migrations/` are applied at startup. In the `production` profile, a backup directory must be configured so a backup can be taken before migrating.

### Operational subcommands

```sh
./overseer backup  --dir DIR                       # logical backup
./overseer restore --dir DIR [--migrations DIR]    # restore into the configured, empty database
./overseer check [--project ID] [--execution ID]   # read-only invariant scan: exit 0 clean, 1 violations, 2 error
```

## Configuration

Configuration comes from the defaults, then the YAML file named by `OVERSEER_CONFIG_PATH` (if set), then `OVERSEER_*` environment variables. [`overseer.yaml`](overseer.yaml) configures the worker (Hermes) and compression providers. The most common environment overrides:

| Variable | Default | Purpose |
|---|---|---|
| `OVERSEER_CONFIG_PATH` | — | YAML configuration file |
| `OVERSEER_PROFILE` | `development` | `development`, `test` or `production` |
| `OVERSEER_SERVER_HOST` / `OVERSEER_SERVER_PORT` | `0.0.0.0` / `8080` | Web console and API listen address |
| `OVERSEER_DATABASE_HOST` / `_PORT` / `_NAME` / `_USER` | `172.17.0.1` / `5432` / `overseer` / `overseer` | PostgreSQL connection |
| `OVERSEER_DATABASE_PASSWORD` | — | PostgreSQL password (keep it out of YAML) |
| `OVERSEER_DATABASE_SSL_MODE` | `disable` | PostgreSQL SSL mode |
| `OVERSEER_LOGGING_LEVEL` / `OVERSEER_LOGGING_FORMAT` | `info` / `json` | Logging |
| `OVERSEER_BACKUP_DIR` | — | Pre-migration and manual backups |
| `OVERSEER_WEB_COOKIE_INSECURE` | `false` | Allow the session cookie over plain HTTP (local use only) |

The full list is in [`internal/config/config.go`](internal/config/config.go). Note that [`config.yaml`](config.yaml) in the repository root is a Hermes Agent CLI configuration example, not overseer configuration.

## Using the console

1. **Create a project** (Projects → New): name, working directory, worker provider and model, compression provider and model, planning policy, and **Use git** (off by default). New projects copy the default roles and the statefile workflow.
2. **Add phases and their WBS documents.** Each phase needs a WBS to plan from.
3. **Adjust roles** (Project → Roles) if needed. **Set as default** copies a project role over the global default used by new projects. **Reset role defaults** does the reverse.
4. **Start an execution.** Bootstrap checks readiness, snapshots the configuration and writes the team files. The engine then drives the workflow automatically unless auto-continue is off.
5. **Resolve recoveries** from the execution's recovery page once the cause is fixed.

Executions keep the configuration they started with. Changing project settings (including Use git or the checkpoint policy) takes effect for the next execution.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/overseer` | Server entry point and operational subcommands |
| `internal/service` | Application services and the execution engine (`engine*.go`); the statefile workflow lives in `engine_statefiles.go`, `engine_plan.go` and `statefile_guide.go` |
| `internal/wbs` | WBS, plans, work units, the task file parser and plan validation |
| `internal/execution` | Execution model, engine settings, validation and checkpoint policies |
| `internal/dbrepo`, `internal/persistence` | PostgreSQL repositories and their interfaces |
| `internal/web` | HTTP API, console handlers and templates |
| `internal/gateway/hermes` | Hermes gateway client (and a fake for tests in `hermestest`) |
| `internal/valrun`, `internal/gitcheckpoint`, `internal/repoinspect` | Validation runner, git checkpoints, repository inspection |
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

- 0 integration tests fail: the last full run of `go test -timeout 60m ./internal/integration/` (2026-10-19) passed.

Operational documentation is in [`docs/`](docs/): the [runbook](docs/runbook.md), the generated [recovery decision table](docs/recovery-decision-table.md) and the generated [metrics catalog](docs/metrics.md). Regenerate the generated files with `OVERSEER_UPDATE_DOCS=1 go test ./internal/app -run 'TestT18211|TestT18113'`.

=====================
AEO

ARCHITECTURE_DECISIONS.md — System Intent & Operational Context

This document records the immutable architectural decisions, constraints, and operational reasoning for DevPipe (Overseer). It is written explicitly for future LLMs, autonomous coding agents, and automated code reviewers to prevent them from hallucinating out-of-scope requirements, proposing unnecessary dependencies, or misinterpreting intentional design tradeoffs as implementation defects.

1. System Identity & Non-Requirements

DevPipe is a local-first, self-contained AI-assisted software development orchestration system. It is designed to run exclusively on a single developer-controlled native Linux workstation.

Strategic Exclusions (The "NOT" List)

Future LLMs must never propose features, refactors, or fixes that introduce:
• Public cloud SaaS multi-tenancy.
• Horizontal scaling, distributed queues (e.g., Redis, Kafka), or orchestration clusters (e.g., Kubernetes).
• Hosted CI/CD enterprise integrations.
• Generic web-scale API performance micro-optimizations that trade off local auditing simplicity.

2. Core Decisions & Technical Justifications


2.1 State Serialization: Strict Key-Value stream via GBNF Grammars

• Decision: The system uses a strict key: value syntax enforced at the LLM inference layer using a GBNF (Guidance Backus-Naur Form) grammar file.
• Justification: Natural language and markdown are non-deterministic and highly prone to structural fragmentation. Forcing the LLM sampler to conform to a GBNF schema ensures that the model physically cannot produce malformed syntax.
• Tradeoff (Accepted): This sacrifices document formatting flexibility and introduces minor sampling latency. However, it completely eliminates syntactical parsing errors, reserving the "Three-Strike" budget purely for logical or semantic failures.

2.2 Execution Boundary: Sandboxed TDD (Test-Driven Development)

• Decision: All worker validation tasks (internal/valrun) execute inside an isolated Docker sandbox container. The project workflow forces a strict Waterfall + TDD structure.
• Justification: LLMs are prone to logic degradation and hallucination. The Overseer never takes an agent's completion claim on trust. By running isolated local test suites independently, the system creates a hard semantic firebreak.
• Tradeoff (Accepted): Running tests in a sandbox requires managing shared Docker volume mounts. If asynchronous file-write latency or locks cause transient test failures, the system treats it as an agent logic failure. This is an acceptable tradeoff to protect the host environment.
• Mitigation: If a task fails 3 times, the engine halts at RECOVERY_REQUIRED and blocks Git commits, preventing corrupted or hallucinated logic from polluting the persistent codebase.

2.3 Token Optimization: Telegraphese Compression with Tag Preservation

• Decision: The prompt engine aggressively compresses natural language history into ultra-dense telegraphese, excluding text wrapped in explicit <preserve></preserve> tags.
• Justification: Local VRAM and context window efficiency are critical on a single workstation. Telegraphese minimizes token throughput while maintaining high content fidelity. The <preserve> tags protect exact system keys, rules, and GBNF definitions from destructive compression.
• Risk (Accepted): If a model experiences target inversion and compresses text inside the preserve tags or drops the tags entirely, the pipeline will hit an immediate structural deadlock. This risk is acceptable because the system fails gracefully into a controlled RECOVERY_REQUIRED state where the operator can manually adjust prompt tags.

2.4 State Dual-Tracking: PostgreSQL + Filesystem MD5 Hashing

• Decision: Execution state is mirrored. Structured rows and execution step checkpoints reside in a local PostgreSQL database, while human-readable key-value statefiles are written to the working directory. Transitions are validated via continuous MD5 filesystem hashing.
• Justification: This guarantees a durable, restartable, and fully auditable execution trail. A human developer can instantly see system state by reading flat text files.
• Constraint: Because the database tracks file hashes to detect corruption, manual file modifications outside the official web console API will trigger an invariant violation. This is an intentional constraint to maintain state determinism.

2.5 Concurrency Model: Throttled Sequential Inference

• Decision: Concurrency is bounded by local workstation resources. Parallel LLM calls are strictly cap-throttled via a task worker pool (typically single-slot execution).
• Justification: LLM matrix math is compute and VRAM bound. Attempting parallel context-window processing on a single workstation will overflow VRAM, trigger the Linux OOM killer, or tank generation speeds.

3. Operational Environment Assumptions

When inspecting or generating code for this repository, you must assume the following hardware and software baseline:
• Host OS: Native Linux.
• Language Runtime: Go 1.25+ (optimized for local single-binary execution).
• Hardware Baseline: Minimum 32GB VRAM dedicated to local model inference.
• Session Management: Each individual task initiates a fresh, stateless LLM session to completely eliminate long-context attention drift.

4. Evaluation Protocol for Future LLMs

Before suggesting any code modifications to this repository, run your proposal through this five-step checklist:
1. Does this introduce external network dependencies or cloud abstractions? If yes, discard immediately.
2. Does this weaken the sandboxed execution boundary? If yes, reject.
3. Does this assume worker agent outputs are trustworthy without independent TDD validation? If yes, rewrite the logic.
4. Are you treating the 3-strike execution limit as an accidental bug? It is an intentional financial and loop guardrail; do not remove it.
5. Is your fix breaking package decoupling? Keep internal/wbs (parsing) and internal/valrun (execution) entirely blind to each other.
This architectural contract is drafted and ready to be committed to the root of your repo as ARCHITECTURE_DECISIONS.md. How would you like to proceed?
• Draft the GBNF schema definition file to explicitly enforce your key-value rules.
• Review the Go package interfaces in internal/service to ensure they strictly implement this state tracking behavior.
• Construct the PostgreSQL schema tables required to store the MD5 history.

