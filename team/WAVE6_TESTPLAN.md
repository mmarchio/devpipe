# WAVE6_TESTPLAN — Wave 6 TDD Coverage Plan (Phase 28, WBS-28.223–249)

Authority: TESTER wave-6 phase start 2026-09-21. Consumes: team/ARCH_WAVE6.md (Test requirements L318-351 + per-task deltas L79-282 + Rulings L413-460) + WBS "Phase 28 Live Trading.md" (src/wbs/) TDD slice L3933-4090. FROZEN task map w6-1..w6-6 (branch A: w6-1→w6-2→w6-3; branch B: w6-1→w6-4→w6-5→w6-6). **PM reconciliation (2026-09-21):** execution uses the TASKS.md 8-task map (w6-1..w6-8); crosswalk by WBS slice: this doc's w6-1 (28.223-230) = TASKS w6-1 (28.223-226) + w6-2 (28.227-229) + w6-3 (28.230); w6-2 (28.231-233) = TASKS w6-4; w6-3 (28.234-236) = TASKS w6-5; w6-4 (28.237-240) = TASKS w6-6; w6-5 (28.241-245) = TASKS w6-7; w6-6 (28.246-249) = TASKS w6-8 (+ 28.293-298 audit-I characterization). Test function prefixes follow this doc's 6-block IDs — map by WBS slice, not prefix.

Status: DEFINITION ONLY — no test files written yet (per-task PRE-TEST gate), no production code.
Conventions: TDD-28.x = WBS-pinned (must pass as written). "arch-req" = ARCH_WAVE6.md test requirement not TDD-pinned. "arch-pin" = value pinned by ARCH_WAVE6.md L31-45 where the WBS TDD slice has NO row (see DISC-1). Execution model is inverted TDD: characterization tests pin existing behavior (expected PASS pre-implementation); P/M-gap tests expected FAIL pre-implementation for the expected reason (missing trigger/field/counter) — NEVER a compile error: gap tests are written against the observable surface (JSON unmarshal, ConfigHash, Validate result, checkpoint field read, stat counter) so the missing member manifests as an assertion failure. Regression = package + Test func by name.

Baseline 2026-09-21: existing test files (LOC): internal/live 13 files / 4089 (eventloop 385, linktest 229, oco_dispatch 280, parity 438, phase28_universegate 114, regimegate 334, replay 167, run 408, shadow_parity 242, startup 407, strategyeval 572, universegate 442, w55diag 71); internal/domain/live 6 files / 1351 (baseline_invalidation 149, comparison_window 327, driftcharacterization 262, driftsampleverdict 95, health_classification 243, live 275); internal/execution 1 file / 183; internal/risk 1 file / 217. Total 5840.

## Per-task plan

### w6-1 — checkpoint / restart recovery / no-self-arm (28.223-230)
- Primary packages: internal/live (+ domain/live, + domaintest reconciliation rig, reference only)
- New test file(s) at PRE-TEST: internal/live/checkpoint_recovery_test.go; internal/live/checkpoint_triggers_test.go; internal/live/analytics_watermark_test.go
- TDD-pinned (exact, WBS L3933-3988 as written):
  - TDD-28.226 [Mandatory] (L3933-3955): before crash `LONG10` / `trailing peak120` / `stop114`; restart → `Phase26 reconciles LONG10`, `Phase28 restores peak120/stop114`, `Phase27 remains disarmed until allowed`; `No reset to current price.`
  - TDD-28.229 (L3959-3976): `100 fills processed` → checkpoint watermark `F100`; `restart replay includes F100 again plus F101` → `F100 no-op`, `F101 applied`
  - TDD-28.230 [Mandatory] (L3980-3986): `Restart completes recovery. Phase28 cannot move Phase27 from DISARMED to ARMED itself.`
- arch-pin (no WBS TDD row; values per ARCH L33-34, consistent with WBS digest):
  - 28.227: 2 processed + 1 in-flight → restart → 2 dedup'd, in-flight resolved exactly once; metric/condition survives via Phase 7 opaque `StrategyStateRefs`
  - 28.228: exit engine after restart holds peak120/stop114 (trailing/break-even/time exits preserved), not reset to current price; `ExitStreamReconstMiss`/`ExitStreamReconstRefusal` stay 0
- arch-req (test-only characterization):
  - 28.223: `SaveCheckpoint` orchestrates, never duplicates — delegates Phase 26 (`Execution.Checkpoint`), Phase 8 `SnapshotAll`, persists `LiveRuntimeCheckpoint` via `RuntimeStateRepo.SaveState`; live adds only ActiveAssignments, RecentSignalKeys, PendingIntentIDs, ProtectiveLinks, OCOLinks, ExitOrderSeq
  - 28.225: Seal/VerifyIntegrity covers every field incl. `AnalyticsWatermark`; corrupt checkpoint → fresh empty state, never partially-trusted
- New test functions:
  - checkpoint_recovery_test.go: TestW61RestartReconcilesLong10RestoresPeakStop (TDD-28.226); TestW61RestartNoResetToCurrentPrice (TDD-28.226); TestW61CheckpointOrchestrationDelegatesPhase26 (28.223); TestW61CorruptCheckpointRejectedFreshState (28.225); TestW61StrategyStateDedupRestored (28.227a); TestW61InFlightSignalResolvedExactlyOnce (28.227b); TestW61ExitStatePeakStopSurviveRestart (28.228, asserts ReconstMiss/Refusal = 0); TestW61NoSelfArmPostRestart (TDD-28.230: post-restart Phase28 RUNNING, Phase27 DISARMED, entry denied)
  - checkpoint_triggers_test.go: TestW61CheckpointOnFill (28.224b); TestW61CheckpointPeriodic (28.224a, fake ticker on `CheckpointInterval`); TestW61CheckpointOnCriticalTransition (28.224c, RECOVERING→READY / RUNNING↔DEGRADED/PAUSED / →STOPPED); TestW61CheckpointFailureNonBlocking (invariant 7: SaveCheckpoint failure logged/counted, trading + exposure reduction continue)
  - analytics_watermark_test.go: TestW61AnalyticsWatermarkF100NoOpF101Applied (TDD-28.229, gap); TestW61WatermarkRestoreSeedsRunStateAndGetter (restore leg of 28.229, gap)
- Existing tests to reuse (named): startup rig full pipeline (internal/live/startup_test.go: TestStartupRecoveryCompletesBeforeEvaluation, TestStartupRefusesToRunWhenRecoveryNotReady, TestNoSignalDuringRecovery, TestPipelineOrderingCanonicalToSignal, TestSignalToTradeIntentCanonicalPath); run_test.go `driveTo` + mock broker (TestLiveRunStatusTransitions, TestLiveRunStopVariants); TestLiveRuntimeCheckpointIntegrity; domaintest.NewReconciliationRig / NewReconciliationRigWithRiskConfig (reconciliation.go:153/159); domaintest.NewLiveSafetyRig (Phase 27 rig); adapters/liveruntimestaterepo (existing in-memory, name only)
- Expected pre-implementation outcome:
  - PASS (characterization): 28.223, 28.225, TDD-28.226, 28.227a (dedup via `RecentSignalKeys`→`processedSignalIDs` already restored), 28.228, TDD-28.230. 28.227b: expected PASS; if it fails (in-flight resolution unwired), that failure is the trigger for the minimal ARCH delta — report, do not weaken.
  - FAIL (gap, expected reason — not compile error): 28.224a/b/c → checkpoint count stays at the graceful-stop value; missing periodic/on-fill/on-critical-transition triggers (assert on saved-checkpoint count/last-seq via repo, no new symbols referenced). TDD-28.229 + restore leg → checkpoint `AnalyticsWatermark` reads 0 (field exists, never populated; no provider/getter to call — assert via restored-state observable, e.g. liveperformance replay re-applies F100 non-idempotently or getter absent ⇒ use repo-stored checkpoint field read = 0)
- Race: no dedicated race test; package gate `go test -race ./internal/live/... ./internal/domain/live/...` must pass at task close (event-loop concurrency in package).
- Regression: TestStartupRefusesToRunWhenRecoveryNotReady, TestNoSignalDuringRecovery, TestLiveRunStatusTransitions, TestLiveRunStopVariants, TestLiveRuntimeCheckpointIntegrity, TestLiveRuntimeImportBoundary, TestStrategyParityBacktestPaperLive
- Reviewer gate: REVIEWER (persistence integrity + restart recovery + no-self-arm risk-control boundary)

### w6-2 — pending intents (28.231-233)
- Primary packages: internal/live (checkpoint.go, run.go) + execution/submit.go (named, reference)
- New test file(s) at PRE-TEST: internal/live/pending_intent_test.go
- TDD-pinned (exact, WBS as written):
  - TDD-28.231 (L3992-4000): `Crash before risk evaluation. Restart 5 minutes later. Signal freshness policy says expired. Expected discarded/audited, not submitted.`
  - TDD-28.233 (L4004-4008): `Durable order operation exists before crash. Expected Phase26 resolves operation before strategy creates any replacement entry.`
- arch-req: 28.232 (no TDD row): pending intents persist as `LiveRuntimeCheckpoint.PendingIntentIDs`, restored into `rs.pendingIntents`, each reconciled against Phase 9's current reservation before resuming
- New test functions (pending_intent_test.go): TestW62StaleSignalDiscardedNotSubmitted (TDD-28.231: 5-min gap, freshness expired → discard + audit record, zero broker submissions); TestW62StaleSignalFreshPolicyReevaluated (companion: within freshness window → re-evaluated, still no blind resubmit); TestW62PendingIntentIDsPersistedAndRestored (28.232); TestW62PendingIntentReconciledAgainstPhase9Reservation (28.232)
  - (28.233 reuses execution submit-guard tests, named below, plus one new: TestW62DurableOpResolvedBeforeReplacement (TDD-28.233, no double-submit after restart))
- Existing tests to reuse (named): TestSubmitApproved_IdempotencyConflict, TestSubmitApproved_DuplicateIsNoBlindRetry, TestSubmitApproved_Ambiguous (internal/execution/execution_test.go); TestApplyFill_DuplicateIsNoOp; run_test.go `driveTo` + mock broker; startup rig; domaintest.NewReconciliationRig
- Expected pre-implementation outcome: ALL PASS (characterization — 28.231/232/233 are audit-I, code pre-exists). Any failure here is a real defect report (FAILURE REPORT format), not a planned gap.
- Race: no dedicated race test; package gate `go test -race ./internal/live/...` at task close.
- Regression: TestSubmitApproved_Success, TestIntentProvenance, TestLiveRunStatusTransitions, TestNoSignalDuringRecovery
- Reviewer gate: NO (tests only, audit-I, no production diff expected; execution/submit.go referenced by name only)

### w6-3 — ordering + duplicate recovery (+ D4 debt) (28.234-236)
- Primary package: internal/live (eventloop.go, marketevent.go, service.go)
- New test file(s) at PRE-TEST: internal/live/event_ordering_recovery_test.go
- TDD-pinned (exact, WBS as written):
  - TDD-28.234 (L4014-4018): `Fill event and next quote arrive close together. Per-instrument ordering must ensure position after fill is visible before next strategy decision where semantics require.`
  - TDD-28.236 (L4024-4032): `BUY signal was already submitted and filled before crash. After restart same metric state still bullish. Duplicate-signal state restored. Expected no automatic second BUY unless strategy policy genuinely calls for increase.`
- arch-req: 28.235 (no TDD row): strategy/exit engines read current canonical position from `execution.Runtime` (`r.positions`), never a cached copy
- D4 debt (shadow-mirror 30s tick vs drift 5s cadence — absorbed here, characterization-only "fix"): pin that a mirror tick arriving out-of-cadence relative to the drift tick does NOT perturb (a) per-instrument arrival ordering or (b) the drift-evaluation cadence, and never feeds a live decision (`ShadowMirrorFailures` counted, never propagated)
- New test functions (event_ordering_recovery_test.go): TestW63FillVisibleBeforeNextStrategyDecision (TDD-28.234: fill + near-simultaneous quote on same instrument → decision sees post-fill position); TestW63PositionStateFreshnessCanonical (28.235); TestW63NoAutomaticSecondBuyAfterRestart (TDD-28.236); TestW63SecondBuyOnlyIfPolicyCallsForIncrease (TDD-28.236 policy leg, `ti.ExistingPositionPolicy` = increase); TestW63D4MirrorTickNoOrderingPerturbation (D4a); TestW63D4MirrorTickNoDriftCadencePerturbation (D4b); TestW63D4MirrorNeverFeedsLiveDecision (D4c)
- Existing tests to reuse (named): TestMarketEventPerInstrumentOrdering, TestMarketEventNoSilentLoss, TestMarketEventCrossInstrumentParallelism, TestMarketEventBoundedBackpressure (eventloop_test.go); TestDuplicateSignalPolicyOnStateChange (strategyeval_test.go); TestW59ShadowMirrorFireAndForget, TestW59ShadowPaperSignalParity (shadow_parity_test.go); run_test.go `driveTo` + mock broker
- Expected pre-implementation outcome: ALL PASS (characterization — audit-I items; D4 expected tests-only per ARCH L181-190). Any failure = real defect report, not a planned gap.
- Race: no dedicated race test; package gate `go test -race ./internal/live/...` at task close (event-loop ordering tests are inherently concurrent).
- Regression: TestMarketEventPerInstrumentOrdering, TestMarketEventNoSilentLoss, TestDuplicateSignalPolicyOnStateChange, TestW59ShadowMirrorFireAndForget, TestPipelineOrderingCanonicalToSignal
- Reviewer gate: NO (tests only; ordering/delegation already correct per ARCH)

### w6-4 — concurrency: per-instrument + per-account serialization / parallel accounts / bounded workers (28.237-240)  — -race MANDATORY
- Primary packages: internal/live (eventloop.go, service.go) + risk/lock.go (named, reference)
- New test file(s) at PRE-TEST: internal/live/concurrency_race_test.go; internal/risk/authorize_atomic_test.go
- TDD-pinned (exact, WBS L4038-4044 as written): TDD-28.238 — `Two strategy threads simultaneously produce valid entries. Phase9/27 account-wide limits must serialize/reserve appropriately. No stale-snapshot over-allocation.`
- arch-req:
  - 28.237: `runState.exitEvals` per-instrument `*sync.Mutex` serializes the two callers into the SAME non-thread-safe Phase 8 `StreamingEvaluator` (EventLoop worker + CheckTimeExits clock tick); NEVER across instruments (cross-instrument concurrency preserved)
  - 28.239 [M]: two `live.Service` instances (two accounts) → independent limit enforcement by construction (separate risk state/locks); `Create` rejects a second run via `ErrRunAlreadyActive`; doc note
  - 28.240 [P gap]: bounded worker-pool CAP — queue depth N → no unbounded worker/goroutine growth, no panic; new `WorkerPoolCap` (0 = current unbounded default); at cap, overflow policy applies
- New test functions:
  - concurrency_race_test.go: TestW64PerInstrumentExitEvalSerialization (28.237, -race, concurrent worker + clock-tick into one instrument); TestW64ConcurrentEntriesNoOverAllocation (TDD-28.238, -race, two strategy threads, account ceiling exactly met, never exceeded); TestW64ParallelAccountsIndependentLimits (28.239, -race, two Service instances, two accounts, independent ceilings); TestW64WorkerPoolCapBounded (28.240, -race, gap)
  - risk/authorize_atomic_test.go: TestW64AuthorizeLimitCheckReserveAtomic (28.238 verify, -race, N goroutines × Authorize under one account ceiling → exactly ceiling approved)
- Existing tests to reuse (named): TestConcurrentAuthorizationRace (internal/risk/risk_test.go), TestMarketEventCrossInstrumentParallelism, TestMarketEventBoundedBackpressure (eventloop_test.go), TestLiveStrategyAllocationCeilings (run_test.go), domaintest.NewRiskRig / NewReconciliationRigWithRiskConfig / NewLiveSafetyRigWithRiskConfig
- Expected pre-implementation outcome:
  - PASS (characterization, -race): 28.237, 28.239, TestW64ConcurrentEntriesNoOverAllocation IF `risk.Service.Authorize` is already atomic under one `lockFor(accountID)` hold.
  - FAIL (expected reason, not compile error): 28.240 → goroutine/worker count grows past any bound for N distinct instruments (assert via runtime.NumGoroutine delta or observable queue depth; no new field referenced at PRE-TEST — cap field absent ⇒ unbounded growth is the failure). TestW64AuthorizeLimitCheckReserveAtomic: expected PASS; if it FAILS with over-allocation, that failure is the trigger for the minimal ARCH delta (wrap check+allocate in one `lockFor` hold) — report, do not weaken.
- Race: **MANDATORY** — every w6-4 test runs under `go test -race`; keep source side lean (race output chatty, budget §4.8). Package gate `go test -race ./internal/live/... ./internal/risk/...`.
- Regression: TestConcurrentAuthorizationRace, TestMarketEventCrossInstrumentParallelism, TestLiveStrategyAllocationCeilings, TestSubmitApproved_IdempotencyConflict
- Reviewer gate: REVIEWER (concurrency + Phase 9 risk critical section; deltas touch risk/lock.go and live eventloop)

### w6-5 — rate limit / debounce / storm detection / orders-min / trades-day (28.241-245)  — -race for 28.243/245
- Primary package: internal/live (submitgate.go, stats.go, service.go)
- New test file(s) at PRE-TEST: internal/live/storm_detection_test.go; internal/live/trades_per_day_test.go; internal/live/rate_debounce_test.go
- TDD-pinned:
  - TDD-28.243 (exact, WBS L4050-4070 as written): `Buggy strategy produces: 100 intents/second`; `runtime safety threshold: 10/sec`; Expected: `entry pause/block` / `alert` / `no > configured allowed external submissions`
  - TDD-28.245 [arch-pin, no WBS TDD row; ARCH L43 + Ruling 9; WBS digest: "Strategy Trades/Day — optional control"]: 50/50 trades allowed → 51st rejected (per strategy, per risk day)
- arch-req (characterization): 28.241 (`StrategyEvaluationRateLimit` bounds per-instrument strategy re-evaluation, 0 = unbounded); 28.242 (signal debounce via canonical Phase 7 duplicate policy — `DuplicatePolicy` in `ActivateStrategyParams`); 28.244 (runtime orders/minute coordinated with Phase 27 safety ceiling already enforced inside `livesafety.Evaluate` gate decision)
- New test functions:
  - rate_debounce_test.go: TestW65RateLimitBindsStrategyEval (28.241); TestW65SignalDebouncePhase7Policy (28.242); TestW65OrdersMinuteCoordinatesPhase27 (28.244)
  - storm_detection_test.go (-race): TestW65StormEntryPauseBlock (TDD-28.243, 100/s vs threshold 10/s → entry pause/block) [gap FAIL]; TestW65StormAlertRaised (28.243 alert leg, runtime-alert path) [gap FAIL]; TestW65StormExternalSubmissionsCapped (28.243 cap leg: observed external submissions never exceed configured 10/s) [gap FAIL]
  - trades_per_day_test.go (-race): TestW65TradesPerDay50Allowed51stRejected (TDD-28.245) [gap FAIL]; TestW65TradesPerDayZeroDisabled (0 = control off, no rejection) [gap FAIL]; TestW65TradesPerDayResetsOnNewRiskDay (per-strategy per-risk-day counter reset) [gap FAIL]
- Existing tests to reuse (named): TestRuntimeAlertValidation, TestProtectionStateBlocks (internal/domain/live/live_test.go); TestLiveRunStatusTransitions (pause-state machine); gate path via startup rig (gateAdapter → LiveSafety.Evaluate); domaintest.NewLiveSafetyRig
- Expected pre-implementation outcome:
  - PASS (characterization): 28.241, 28.242, 28.244.
  - FAIL (gap, expected reason — not compile error): TDD-28.243 all three legs → no storm/throttle detection: at 100/s all entries pass the gate, no alert row, external submissions uncapped (assert via broker-call count + alert repo; threshold config absent ⇒ unbounded behavior is the failure). TDD-28.245 all legs → 51st trade NOT rejected (no per-strategy per-risk-day counter; assert via approved-submission count; `MaxTradesPerDayPerStrategy` absent ⇒ zero = disabled behavior is the failure).
- Race: **-race MANDATORY for TDD-28.243 and TDD-28.245 tests** (concurrent submission pressure); package gate `go test -race ./internal/live/...`.
- Regression: TestRuntimeAlertValidation, TestProtectionStateBlocks, TestLiveRunStatusTransitions, TestConcurrentAuthorizationRace
- Reviewer gate: REVIEWER (risk controls: storm → entry pause/block + alert + external cap; trades/day rejection)

### w6-6 — LiveRuntimeConfig completeness + canonicalization + hash + hot-change (+ D2 debt) (28.246-249)
- Primary packages: domain/live (config.go, hash.go) + internal/live (service.go Deps, reference)
- New test file(s) at PRE-TEST: domain/live/config_completeness_test.go; domain/live/config_hash_test.go
- TDD-pinned:
  - TDD-28.248 (exact, WBS L4076-4087 as written): Change: `performance snapshot interval` / `drift threshold` / `assignment` / `shutdown policy` → `produces different behavior hash where behaviorally relevant`. Scope note: per ARCH Ruling 10, assignment + shutdown policy are composition-root Deps / runtime state (NOT behavior config — documented); the test asserts the two behaviorally-relevant legs (performance snapshot interval, drift threshold) flip the hash.
  - TDD-28.246 [arch-pin, no WBS TDD row; ARCH L44 + Ruling 10; WBS digest field list]: all WBS-28.246-listed parameters settable + validated: account/adapter, strategy assignments, market subscriptions, warm-up, checkpointing, shutdown, performance snapshot cadence, drift policies, alert policies. Disposition: account/adapter, market subscriptions, assignments, shutdown = composition-root Deps (test asserts wiring surface, not config fields); additive config fields `PerformanceSnapshotInterval`, `AlertPolicy`, `WarmUp` + consolidation of w6-4/w6-5 fields (`WorkerPoolCap`, `MaxExternalSubmissionsPerSecond`, `MaxTradesPerDayPerStrategy`), zero = disabled/default.
- arch-req: 28.247 (`ConfigHash = hashJSON(cfg)` deterministic over canonical JSON, field-order-independent); 28.249 (behavioral change requires controlled restart / fresh run — config bound at `Create` via `ConfigHash`, pinned by run validation; no hot-reload path exists); **D2** (all-zero `DriftPolicy` is inert no-op today → "fixed" = explicit sane `DefaultDriftPolicy()` and/or `Validate()` rejecting all-zero `PerformanceDriftPolicy`)
- New test functions:
  - config_completeness_test.go: TestW66ListedParamsSettableViaJSON (TDD-28.246: JSON with all listed params round-trips to non-zero values) [gap FAIL]; TestW66ListedParamsValidated (TDD-28.246: invalid values rejected by Validate) [gap FAIL]; TestW66DefaultDriftPolicySane (D2: default has non-zero sensible thresholds) [gap FAIL]; TestW66ValidateRejectsAllZeroDriftPolicy (D2) [gap FAIL]
  - config_hash_test.go: TestW66ConfigHashDeterministic (28.247, PASS); TestW66ConfigHashDriftThresholdFlip (TDD-28.248 leg 1, PASS — `DriftPolicy` already in config); TestW66ConfigHashPerfSnapshotIntervalFlip (TDD-28.248 leg 2) [gap FAIL]; TestW66BehavioralChangeRequiresControlledRestart (28.249, no hot-reload path, PASS)
- Existing tests to reuse (named): TestConfigYAMLRoundTrip (existing config round-trip — file confirmed at PRE-TEST); TestW46PolicyValidateMatrix, TestW46ThresholdLookup, TestW46DriftMetricEnum, TestW46DriftActionBoundedSet, TestW46DecideActionMatrix, TestW46BaselineSourceSet (domain/live drift-policy matrix); TestDriftPolicyConsecutiveBreachVsCatastrophicOverride, TestDriftSeverityClassificationAndMinimumSample (driftcharacterization_test.go); TestLiveRunStatusTransitions (config bound at Create)
- Expected pre-implementation outcome:
  - PASS (characterization): 28.247, TDD-28.248 leg 1 (drift threshold already in `LiveRuntimeConfig` + hash view), 28.249.
  - FAIL (gap, expected reason — not compile error): TDD-28.246 both legs → JSON keys for `PerformanceSnapshotInterval`/`AlertPolicy`/`WarmUp`/`WorkerPoolCap`/`MaxExternalSubmissionsPerSecond`/`MaxTradesPerDayPerStrategy` unmarshal to zero (field absent ⇒ old zero behavior) and Validate accepts the incomplete config; D2 both legs → all-zero `PerformanceDriftPolicy` accepted by `Validate()` and no `DefaultDriftPolicy()` sane default observable (assert via Validate result + zero-value config behavior). TDD-28.248 leg 2 → perf-snapshot-interval JSON change leaves `ConfigHash` unchanged (field not in hash view).
- Race: none (pure domain config; no concurrency).
- Regression: TestW46PolicyValidateMatrix, TestConfigYAMLRoundTrip, TestDriftPolicyConsecutiveBreachVsCatastrophicOverride, TestLiveRuntimeImportBoundary
- Reviewer gate: NO (pure config/validation; additive fields, zero = old behavior; no action-vocabulary change)

## Shared test infrastructure (wave level)
- Fake clock / manual ticker: NOT in wave-5 inventory → needed by w6-1 (periodic `CheckpointInterval` trigger), w6-5 (100/s vs 10/s rolling window; risk-day rollover for 28.245). Test-only helper; production change limited to injectable ticker where ARCH already mandates it (checkpoint ticker in run loop, w6-1).
- Reuse, do not duplicate (ARCH L348-351): `internal/live/startup_test.go` full-pipeline rig (reconciliation+strategy+tradeintent+risk+execution); `internal/live/run_test.go` `driveTo` + mock broker; `domaintest.NewReconciliationRig` (reconciliation.go:153) / `NewReconciliationRigWithRiskConfig` (:159); `domaintest.NewLiveSafetyRig` / `NewLiveSafetyRigWithRiskConfig` (livesafety.go Phase 27 rig); `domaintest.NewRiskRig`, `NewExecutionRig(WithRiskConfig)`, `NewStrategyRig`, `NewTradeIntentRig`.
- In-memory adapters (existing; name only): adapters/liveruntimestaterepo (checkpoint save/restore), adapters/runtimealertrepo (storm/drift alerts), adapters/liveorderrepo, adapters/livefillrepo (F100/F101 watermark fixture), adapters/liverunrepo.
- Fixture data: 100-fill sequence with stable fill IDs F1..F101 (w6-1 TDD-28.229); LONG10/peak120/stop114 pre-crash checkpoint state (w6-1 TDD-28.226/228 — reuse Phase 8 exit-stream fixture from startup rig); 100-intent burst driver at 10/s granularity (w6-5 TDD-28.243, deterministic fake clock, no real-time sleep).

## Pre-test gate checklist (per-task READY gate)
Before ENGINEER starts any task, TESTER must show ALL of:
1. New test file(s) exist at the named path(s) above with every named test function present (no placeholders/`t.Skip`).
2. Characterization tests (expected PASS) run green against UNMODIFIED pre-implementation code — exact `go test` output captured.
3. Gap tests (expected FAIL) fail, and the failure output names the expected reason (missing trigger / missing field / missing counter / zero-valued checkpoint field) — NOT a compile error, NOT a panic, NOT a flaky timeout. Gap tests verified to reference no new production symbols.
4. `git diff --stat` since wave start shows ONLY `_test.go` files (and domaintest test helpers if any) — no production code touched.
5. Baseline package gate captured: `go test ./internal/live/... ./internal/domain/live/... ./internal/execution/... ./internal/risk/... -count=1` ok (or failures pre-dated and named).
6. Race gate: for w6-4 (all tests) and w6-5 (28.243/245 tests) the exact `-race` command is stated in the task section and run at PRE-TEST to establish a clean race baseline on existing code; package gate `go test -race` commands for w6-1/w6-2/w6-3 recorded.
7. Report format per team/TESTER.md: STATUS: READY + TESTS_ADDED (file + func names) + expected-FAIL list with one-line expected reason each + baseline evidence.
Per-task READY gate summary: w6-1 (13 new fns; 6 gap-expected-FAIL: 28.224×3 + 28.229×2 + 28.227b conditional), w6-2 (5 new fns; 0 gap-FAIL — all characterization), w6-3 (7 new fns; 0 gap-FAIL), w6-4 (5 new fns; 1 gap-FAIL: 28.240; 28.238 conditional on atomicity verify; -race MANDATORY), w6-5 (9 new fns; 6 gap-FAIL: 28.243×3 + 28.245×3; -race for 28.243/245), w6-6 (8 new fns; 5 gap-FAIL: 28.246×2 + D2×2 + 28.248 leg 2).

## Conflicts (route to ARCHITECT — NOT resolved here)
- DISC-1 (NON-BLOCKING for plan persistence): WBS TDD slice L3933-4090 contains TDD rows for only 10 items (28.226, 28.229, 28.230, 28.231, 28.233, 28.234, 28.236, 28.238, 28.243, 28.248). ARCH_WAVE6.md L31-45 additionally pins 28.227, 28.228, 28.240, 28.245, 28.246, which have NO WBS TDD row in the slice. This plan pins those five to the ARCH values (marked "arch-pin"); all are consistent with the WBS digest (no numeric conflict found). For the 10 WBS rows, ARCH and WBS values match exactly — no discrepancy.
- DISC-2 (scope note, not a conflict): TDD-28.248 lists four changed parameters; per ARCH Ruling 10, assignment + shutdown policy are runtime state / Deps (not behavior config). The w6-6 hash test asserts only the two behaviorally-relevant legs (performance snapshot interval, drift threshold); WBS "where behaviorally relevant" is honored.
