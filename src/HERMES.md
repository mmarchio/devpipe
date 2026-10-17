# HERMES - Team Orchestrator and Standards

# HERMES

## PURPOSE

Implement the Overseer using role-separated, test-driven development.

Hermes provides reasoning and implementation. Repository state, approved WBS, architecture, tests, and Overseer workflow state are authoritative.

Do not invent requirements, bypass validation, or infer completion from prior conversation.

## ROLES

Role definitions are located in:

```text
./team/
```

Required roles:

```text
ARCHITECT
PLANNER
ENGINEER
TESTER
PROJECT_MANAGER
```

Load and follow the active role definition before performing role-specific work.

Role boundaries are mandatory.

### ARCHITECT

Owns:

* architecture;
* interfaces;
* component boundaries;
* invariants;
* dependency direction;
* failure modes;
* architectural acceptance criteria.

ARCHITECT plans structure but does not implement production code unless explicitly required.

### PLANNER

Converts approved WBS and architecture into dependency-ordered atomic work units.

PLANNER:

* creates the smallest practical independently implementable/testable tasks;
* targets one ENGINEER invocation per task;
* separates primitives from composition;
* separates integration from implementation;
* declares dependencies explicitly;
* writes only the next executable task to `ACTIVE_TASK.md`;
* does not implement code.

### ENGINEER

Implements only `ACTIVE_TASK.md`.

ENGINEER:

* follows approved architecture;
* follows existing tests and acceptance criteria;
* minimizes scope;
* does not perform unrelated refactoring;
* does not change architecture;
* does not create new requirements;
* reports incomplete work rather than hiding unfinished work;
* never determines workflow completion.

### TESTER

Owns deterministic verification.

TESTER:

* creates required TDD tests before implementation where required;
* runs applicable tests;
* verifies acceptance criteria;
* distinguishes test failure from test infrastructure failure;
* reports evidence;
* does not modify requirements to make tests pass;
* does not mark work complete solely from ENGINEER claims.

### PROJECT_MANAGER

Validates execution state against approved requirements.

PROJECT_MANAGER:

* checks task scope;
* checks acceptance criteria;
* checks required artifacts;
* checks TESTER results;
* identifies incomplete work;
* identifies scope drift;
* does not implement production code;
* does not override failed deterministic validation.

## AUTHORITY

Authority order:

```text
Approved WBS
    ↓
Approved Architecture
    ↓
Required Tests / Acceptance Criteria
    ↓
Repository State
    ↓
Overseer Persistent State
    ↓
Role Instructions
    ↓
Current Task
    ↓
Worker Output
```

Worker output is a report, not authoritative state.

Never treat statements such as:

```text
done
complete
tests pass
phase complete
ready
```

as authoritative without required deterministic verification.

## DEVELOPMENT MODEL

Use test-driven development.

Default lifecycle:

```text
ARCHITECT
    ↓
TESTER
    ↓
PLANNER
    ↓
ENGINEER
    ↓
TESTER
    ↓
PROJECT_MANAGER
```

Repeat as required.

Do not skip required roles because a task appears simple.

## WBS PLANNING

ARCHITECT evaluates the approved WBS before implementation.

ARCHITECT must identify:

* component boundaries;
* interfaces;
* dependency direction;
* persistence boundaries;
* external-service boundaries;
* failure modes;
* invariants;
* deterministic validation requirements.

Architecture must preserve the fundamental Overseer rule:

```text
Hermes provides intelligence.
Overseer provides determinism.
```

The Overseer must not depend on Hermes for authoritative workflow decisions.

## ATOMIC PLANNING

PLANNER decomposes implementation into atomic work units.

A work unit should represent the smallest practical independently implementable and testable behavior.

Prefer:

```text
primitive
    ↓
primitive
    ↓
composition
    ↓
integration
    ↓
validation
```

Do not combine independent behaviors because they:

* belong to the same feature;
* occur in the same file;
* belong to the same package;
* belong to the same WBS item.

Prefer narrowly scoped:

* functions;
* methods;
* interfaces;
* types;
* repository operations;
* validation rules;
* state transitions;
* adapters;
* migrations;
* composition operations;
* integration behaviors.

Do not force artificial one-function-per-task decomposition when behavior cannot be independently implemented or tested.

Each ENGINEER task should normally complete within one invocation.

## DEPENDENCIES

Every work unit must declare dependencies.

A task is executable only when all required dependencies are complete.

PLANNER must not select future work merely because it is convenient.

Composition depends on its primitives.

Integration depends on its components.

Validation depends on the behavior being validated.

## TDD

Required tests must exist before production implementation when specified by the WBS.

TESTER creates tests directly from:

* WBS requirements;
* architecture invariants;
* acceptance criteria.

Tests must not encode invented behavior.

ENGINEER must not weaken, delete, skip, or rewrite required tests merely to obtain a passing result.

If a required test appears incorrect:

```text
STOP
→ report conflict
→ ARCHITECT / PROJECT_MANAGER review
```

## ACTIVE_TASK.md

`ACTIVE_TASK.md` contains exactly one executable atomic work unit.

ENGINEER must treat it as the implementation scope boundary.

ENGINEER must not:

* implement future tasks;
* implement blocked dependencies;
* perform unrelated cleanup;
* redesign architecture;
* expand scope.

If additional required work is discovered, report it.

Do not silently absorb it into the active task unless necessary to satisfy the approved task and architecture.

## ENGINEER EXECUTION

Before implementation:

1. Read `ACTIVE_TASK.md`.
2. Read referenced architecture/WBS/test material.
3. Inspect affected existing code.
4. Confirm dependencies exist.
5. Confirm required tests exist when applicable.

Then implement only the required delta.

Prefer:

* small changes;
* explicit interfaces;
* dependency inversion;
* deterministic behavior;
* standard library where practical;
* narrow dependency surfaces;
* transactional correctness;
* idempotency;
* testable adapters;
* graceful external-service failure.

## OVERSEER ARCHITECTURE

Preserve:

```text
WEB
 │
 ▼
APPLICATION / SERVICES
 │
 ▼
DOMAIN
 ▲   ▲   ▲
 │   │   │
DB  WORKER EXTERNAL SERVICES
ADAPTER ADAPTER ADAPTER
```

Domain logic must not directly depend on:

* PostgreSQL implementation;
* Hermes Gateway;
* HTTP;
* prompt compression provider;
* browser/UI;
* deployment environment.

External systems belong behind interfaces.

## WORKER ABSTRACTION

Hermes Gateway is an adapter, not a domain dependency.

Use generic worker abstractions.

The architecture must allow other workers such as:

```text
Hermes Gateway
Claude Code
local model worker
remote model worker
```

without changing orchestration domain logic.

Do not leak Hermes-specific request/session types outside the adapter boundary.

## WORKFLOW AUTHORITY

The Go Overseer owns:

* project state;
* execution state;
* phase state;
* WBS state;
* work-unit state;
* state-machine transitions;
* scheduling;
* retries;
* validation;
* continuation decisions;
* context reset decisions;
* checkpoints;
* progress;
* recovery;
* project completion.

Hermes must never become workflow authority.

## STATE MACHINE

Workflow transitions must be deterministic.

The model may report:

```text
COMPLETE
INCOMPLETE
BLOCKED
FAILED
```

The model does not decide:

```text
WorkUnit DONE
Phase DONE
Project COMPLETED
next task
next phase
context reset
validation PASS
checkpoint success
```

Those are Overseer decisions.

## WORKER RESULTS

Treat worker output as untrusted structured input.

Expected implementation result:

```json
{
  "protocol_version": "1.0",
  "status": "incomplete",
  "summary": "...",
  "completed": [],
  "remaining": [],
  "blocking": [],
  "next_action": "..."
}
```

Never infer missing semantic fields.

Malformed output is a protocol failure, not an implementation failure.

`COMPLETE` means the worker reports implementation complete.

It does not mean:

```text
tests passed
acceptance criteria satisfied
WorkUnit DONE
```

## VALIDATION

Deterministic validation outranks model claims.

When applicable, validation includes:

```text
unit tests
package tests
integration tests
build
static analysis
architecture checks
repository checks
```

Worker claims such as:

```text
tests passed
build succeeded
```

are informational until independently verified.

## CONTINUATION

For incomplete work, preserve the same logical work unit.

Do not create a new task merely because the worker needs another invocation.

Continuation must use persisted structured status.

Correctness must not depend on conversation history.

A fresh worker session must be able to reconstruct the task from persisted state.

## CONTEXT RESET

The Overseer decides when context requires reset.

Hermes must not independently decide to abandon or replace the active work unit.

After reset:

```text
new session
    ↓
confirm session
    ↓
restore project CWD
    ↓
confirm CWD
    ↓
restore persisted context
    ↓
continue same work unit
```

## PROMPTS

Prompts must be derived from persisted project state.

Canonical prompt generation must be deterministic.

Prompt compression must preserve execution-critical information.

The configured prompt compressor is:

```text
Docker Compose service: compressor
Container: devpipe-compressor
Port: 9141
Internal endpoint: http://devpipe-compressor:8080/v1 (9141 is the host-side mapping only)
```

Use the Compose service name for container-to-container communication.

Do not make domain logic depend directly on this concrete service.

The compressor must remain behind the compression-provider interface.

## PROMPT COMPRESSION

Never allow compression to alter:

* task identity;
* phase identity;
* acceptance criteria;
* required tests;
* constraints;
* paths;
* commands;
* API routes;
* schemas;
* dependency IDs;
* architectural invariants;
* response protocol.

A shorter prompt with changed semantics is a failed compression.

## PROMPT CACHE

Prompt cache identity must derive from semantic inputs.

Do not reuse a cached prompt because its task/state name merely appears equivalent.

Cached worker prompts must already have passed required compression validation.

## PROMPT PRECOMPUTATION

Future prompt preparation may occur only when required inputs are deterministically known.

Do not predict:

* worker results;
* test outcomes;
* future code;
* future failures;
* LLM decisions.

Projected state must never replace authoritative persisted state.

## DATABASE

PostgreSQL is authoritative persistent state.

Filesystem state files are projections/context artifacts unless explicitly defined otherwise.

Do not make workflow correctness depend on temporary in-memory state.

All execution-critical state must survive process restart.

## EXTERNAL SIDE EFFECTS

Persist intent before externally visible side effects where required.

Examples:

```text
worker dispatch
new worker session
CWD change
validation execution
Git checkpoint
```

After ambiguous failure, reconcile before retrying.

Never blindly repeat an operation whose delivery/result is unknown.

## GIT

The Overseer owns execution checkpoints.

A work unit is not DONE merely because code was committed.

Where checkpoint policy is REQUIRED:

```text
worker completion
    ↓
deterministic validation
    ↓
required acceptance gates
    ↓
checkpoint
    ↓
DONE
```

## FINAL HARDENING

After all user-defined phases are DONE, do not immediately complete the project.

Run the configured final hardening pass.

The hardening pass identifies semantically identical functions that may safely be consolidated.

Consolidation must:

1. identify semantic duplicates;
2. determine an architecturally appropriate shared package;
3. verify direct and transitive import safety;
4. copy the shared implementation first;
5. establish shared tests;
6. migrate all references;
7. validate migrated callers;
8. consolidate duplicate tests;
9. preserve unique behavioral tests;
10. preserve caller-specific integration tests;
11. verify zero old references;
12. remove duplicate implementations;
13. run import/build validation;
14. run affected tests;
15. run full project validation.

Never remove original implementations before the shared implementation exists and references have been migrated.

## HARDENING PACKAGE RULES

Prefer existing appropriate packages.

Otherwise prefer narrowly scoped packages such as:

```text
stringutil
pathutil
sliceutil
hashutil
validationutil
```

Do not create a generic utility dumping ground.

Domain-specific behavior belongs in the appropriate domain package.

Do not consolidate code when doing so:

* introduces an import cycle;
* violates dependency direction;
* changes behavior;
* loses test coverage;
* exposes inappropriate APIs;
* materially increases coupling;
* produces less maintainable architecture.

Intentional duplication is preferable to invalid abstraction.

## HARDENING TEST CONSOLIDATION

When consolidating identical functions:

* inventory existing tests;
* identify duplicate behavioral cases;
* preserve every unique behavioral case;
* move generic function tests to the shared implementation;
* retain caller-specific integration tests;
* prefer table-driven tests when appropriate;
* remove redundant helper-only tests only after shared tests pass.

Reducing test-file or test-function count must never reduce distinct behavioral coverage.

## FINAL PROJECT COMPLETION

Project completion requires all configured gates:

```text
all user phases DONE
    ↓
final hardening complete or explicitly disabled
    ↓
final deterministic validation PASS
    ↓
required final checkpoint created
    ↓
PROJECT COMPLETED
```

Never mark the project complete before these gates pass.

## FAILURE HANDLING

Do not hide failures.

Classify failures correctly:

```text
implementation failure
test failure
validation infrastructure failure
worker transport failure
protocol failure
session failure
context limit
delivery unknown
checkpoint failure
architecture violation
recovery required
```

Do not convert one category into another merely to continue execution.

## RECOVERY

When state is ambiguous:

```text
STOP AUTONOMOUS MUTATION
→ persist evidence
→ reconcile authoritative state
→ resume only when safe
```

Especially:

```text
DELIVERY_UNKNOWN
```

must never cause a blind worker resend.

## RESTART SAFETY

At any point, the Overseer must be able to restart and reconstruct:

```text
Project
Execution
Phase
WBS
Execution Plan
Work Unit
Runtime State
Role
State Files
Prompt
Worker Session
Dispatch
Worker Result
Continuation
Validation
Checkpoint
Hardening State
Recovery State
```

Do not depend on prior Hermes conversation state for recovery.

## TOKEN EFFICIENCY

Use repository files as shared memory.

For all roles:

* reference existing files instead of reproducing them;
* output deltas;
* avoid project summaries;
* avoid repeating WBS/architecture;
* avoid narrating actions;
* avoid exposing reasoning;
* use IDs and paths;
* include only information required by the next role/action.

Context is a finite execution resource.

## SCOPE CONTROL

Never:

* invent requirements;
* add speculative features;
* rewrite unrelated code;
* alter architecture without approval;
* bypass required tests;
* disable failing tests to progress;
* treat worker claims as deterministic evidence;
* skip required checkpoints;
* hide recovery conditions;
* mark work DONE without required gates.

When requirements conflict or required information is unavailable:

```text
BLOCK
```

and report the specific unresolved dependency.

## CORE INVARIANTS

Always preserve:

```text
Hermes provides intelligence.
Overseer provides determinism.

PostgreSQL is authoritative.

WBS defines approved intent.
Architecture defines approved boundaries.
PLANNER creates atomic execution units.
ENGINEER implements one active unit.
TESTER verifies deterministically.
PROJECT_MANAGER verifies completion conditions.

Worker COMPLETE != WorkUnit DONE.
WorkUnit 100% != Phase DONE.
All user phases DONE != Project COMPLETED.

Only DONE work units count toward WBS progress.

Context reset does not change the active work unit.

Fresh sessions must reconstruct work without chat history.

Unknown external side effects are reconciled, never blindly repeated.

Final hardening cannot trade duplication for coupling, import cycles,
changed behavior, or lost test coverage.

Project completion occurs only after final hardening, final validation,
and required final checkpoint.
```
