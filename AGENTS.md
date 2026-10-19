# AGENTS.md — DevPipe Architectural Contract

## Project Identity

DevPipe (Overseer) is a local-first, self-contained AI-assisted software development orchestration system.

Its intended deployment is Docker on a single developer-controlled native Linux workstation.

DevPipe is NOT:
- A public cloud SaaS platform.
- A multi-tenant application.
- A horizontally scaled distributed service.
- A hosted CI/CD platform.
- A general-purpose remote code execution service.

Do not infer requirements from frameworks, dependencies, or generic industry practices.

## Primary Objectives

In priority order:

1. Deterministic orchestration of nondeterministic AI agents.
2. Correctness through independently verified state transitions.
3. Prevention of runaway agent loops and unnecessary token expenditure.
4. Proactive optimization of prompt and context-window usage.
5. Durable, restartable execution with auditable state.
6. Efficient use of local workstation resources.
7. Operator visibility and recovery.

## Deployment Assumptions

- Primary host: native Linux.
- Application deployment: Docker containers.
- Expected operator: one trusted local developer.
- Expected concurrency: bounded by local resources.
- PostgreSQL is used for persistent execution state and recovery.
- Local deployment does not imply that containers or external dependencies are inherently secure.

Do not recommend cloud-native scaling, multi-tenancy, orchestration clusters, or distributed infrastructure without a demonstrated requirement.

## Architectural Invariants

### Deterministic Orchestration

AI workers propose or produce artifacts.

The overseer independently validates artifacts, acceptance criteria, and state transitions.

Worker claims of completion are not authoritative.

### Three-Strike Guardrail

Repeated malformed or unacceptable outputs are retried within a bounded budget.

The retry limit exists to detect repeated agent failure, prevent unproductive loops, and control token expenditure.

Do not classify the retry mechanism as accidental brittleness solely because it has a fixed limit.

Reviewers may still identify defects in parsing, retry accounting, recovery, or failure detection.

### Prompt Compression

Prompt compression is a proactive, general-purpose token-efficiency optimization.

It is not evidence that the original prompts are poorly designed.

Assess compression by instruction preservation, token reduction, semantic fidelity, and downstream task correctness.

### Container Execution

The deployment boundary is Docker on a trusted Linux workstation.

Assess execution risks against actual container permissions, mounts, networks, capabilities, and process identities.

Do not assume unrestricted native-host execution.

Do not assume Docker provides complete isolation.

### Web Console

The web console is a secondary operator interface.

Its development priority is subordinate to the orchestration engine and deterministic workflow.

Do not infer project maturity or core architectural quality solely from the console's state.

## Review Protocol

Before proposing architectural changes:

1. Read this file and README.md.
2. Identify the project's deployment and threat models.
3. Inspect relevant source code.
4. Distinguish documented design decisions from implementation defects.
5. Identify the specific invariant affected.
6. Cite file paths and functions supporting findings.
7. State assumptions and uncertainties explicitly.

Classify every finding as one of:

- **DEFECT:** Violates intended behavior or a documented invariant.
- **RISK:** Plausible failure under the intended operating environment.
- **TRADEOFF:** Intentional design choice with measurable disadvantages.
- **OUT_OF_SCOPE:** Requires unsupported deployment or product assumptions.
- **UNKNOWN:** Insufficient evidence to determine correctness.

Do not report speculative cloud scaling or multi-tenant problems as current defects.

## Precedence

When information conflicts, use:

1. Verified current implementation for claims about actual behavior.
2. Approved architectural contracts for intended behavior.
3. Accepted WBS and design decisions for planned behavior.
4. README and operational documentation.
5. Generic engineering conventions.

Implementation behavior does not automatically override design intent. A mismatch may be a defect.

Do not silently resolve contradictions. Report them explicitly.
