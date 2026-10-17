# CONTEXT MAP — internal/live (Phase 28 hot-path live-trading app layer)
One line per production .go file -> primary responsibility. Derived from file
doc headers plus full reads of service.go, run.go, eventloop.go, marketevent.go.

service.go (222) — Service composition root: Deps wiring (NewService installs the Phase 27 live gate), single-run runState container, Stats() exposure, emitter/now helpers.
run.go (106) — Run lifecycle: Create (CREATED), transitionRun (validated+persisted status transitions with app events), CurrentRun snapshot, requireActive gate.
startup.go (83) — Start sequence (WBS-28.12): STARTING -> RECOVERING (reconciliation must reach READY) -> READY -> RUNNING.
eventloop.go (169) — Async market-event consumer: per-instrument worker goroutines over bounded channels, overflow policies (block / drop-oldest / drop-newest), Publish / Shutdown (drain+join) / Errors / ActiveInstrumentCount.
marketevent.go (72) — OnMarketEvent: per-instrument market-state cache update, run-active gate, exit re-evaluation dispatch to execution.Runtime, entry-strategy evaluation.
strategyeval.go (367) — Entry-strategy evaluation pipeline: StreamingEvaluator Push -> signal -> tradeintent.Compose -> risk authorize -> execution submit (the one divergence from paper).
assignment.go (170) — ActivateAssignment: binds an immutable strategy version + per-instrument context into the live run.
checkpoint.go (105) — Execution checkpoint save/restore for startup recovery and graceful stop.
stop.go (72) — GracefulStop / StopAndFlatten / CrashStop terminal transitions.
sessionend.go (99) — Session-end cutoff handling (end-of-session stop logic).
protective.go (107) — Protective (stop-loss/bracket) order lifecycle and ProtectiveOrderLink tracking.
oco.go (69) — Synthetic OCO link lifecycle for protective order pairs.
fills.go (91) — Broker fill event application to run state (orders/fills accounting).
submitgate.go (77) — gateAdapter: installs the Phase 27 livesafety gate into execution.Runtime so every submission (entries+exits) is gated.
corporateaction.go (70) — CorporateActionHandler wiring (livesafety) for the live run.
query.go (65) — Read-only queries over run state (Assignments snapshot, etc.).
events.go (54) — EventSink seam + emitter (clock-stamped app events, unique IDs, run-ID correlation).
pause.go (41) — Assignment-level Pause: blocks NEW entries while preserving exits.
universegate.go (38) — IsInUniverse: universe-membership check against the snapshot.
regimegate.go (33) — RegimeAllowedFor: regime filter for entry eligibility.
stats.go (75) — StatsCollector: operational counters (market events, evaluations, signals, risk/gate decisions, orders/fills) + MaxQueueDepth high-water mark.
errors.go (25) — Package error sentinels (ErrNoActiveRun, ErrRunAlreadyActive, ...).
