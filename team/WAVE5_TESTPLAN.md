# WAVE5_TESTPLAN — Wave 5 TDD Coverage Plan (Phase 28, WBS-28.163–222)

Authority: TESTER wave-5 phase start 2026-09-20. Consumes: team/ARCH_WAVE5.md (Test requirements + Implementation constraints) + WBS "Phase 28 Live Trading.md" (src/wbs/) TDD slice L3726-3932. TASKS.md L885-900 task table w5-0..w5-9 + forward constraint.

Status: DEFINITION ONLY — no test files written yet (per-task PRE-TEST gate), no production code.
Conventions: TDD-28.x = WBS-pinned (must pass as written). "arch-req" = ARCH_WAVE5.md test requirement not TDD-pinned. Regression = package + Test func by name.

Baseline 2026-09-20: `go test ./internal/liveperformance/... ./internal/report/... -count=1` → ok (2 pkgs; 68 top-level test funcs: 62 liveperformance + 6 report).

## Per-task plan

### w5-0 — production wiring of drift path (W0-5; closes 28.154 nuance)
- Primary package: cmd/liveperf
- New test file(s) at PRE-TEST: cmd/liveperf/wiring_test.go
- TDD-pinned: none (composition gap, no WBS TDD row)
- arch-req (forward constraint TASKS L885, ARCH Test-reqs w5-0 acceptance):
  - A: real Live + LiveSafety + repo deps constructed (no nil Live/LiveSafety; no in-memory fakes on acceptance path)
  - B: periodic cadence invokes EvaluateDriftForCurrentRun repeatedly; caller supplies NO identity / sampleSize
  - C: WARN drift verdict → RuntimeAlert raised (28.154 closed; stats-only no longer acceptable)
  - D: PAUSE/BLOCK verdict → alert + pause record in gate-read pauserepo
- Expected failures at PRE-TEST:
  - A: fails: cmd/liveperf still constructs nil Live/LiveSafety (in-memory shell)
  - B: fails: composition root has no cadence timer/loop
  - C: fails: alert.go routes WARN drift to stats only, no RaiseAlert
  - D: fails: no PauseDispatchFn wiring to livesafety.Pause
- Regression: liveperformance TestW46WarningStatsOnly, TestW46PauseDispatch, TestW46BlockDispatch, TestW45SeamHappyPathEquivalence, TestW45SeamNilDepsFailClosed; livesafety TestEvaluate_PersistsEveryDecision, TestKillSwitch_BlocksSubsequentEvaluation
- Reviewer gate: REVIEWER (composition + risk-control routing + cadence concurrency)

### w5-1 — health aggregation (28.163-165)
- Primary packages: domain/live (pure classifier), liveperformance (component score computation)
- New test file(s): domain/live/health_classification_test.go; liveperformance/health_test.go
- TDD-pinned: none
- arch-req:
  - composite + blocking-component-wins classification → 28.163 enum (HEALTHY|WATCH|DEGRADED|PAUSED|BLOCKED|INSUFFICIENT_DATA)
  - INSUFFICIENT_DATA when recorded components below minimum / sample below threshold
  - unrecorded component absent from Scores, never 0 (incl. HealthBroker when broker evidence unavailable)
- Expected failures at PRE-TEST:
  - domain: fails: domain/live has no classification fn (recorded scores + threshold table → enum)
  - app: fails: liveperformance has no component-score computation from live/livesafety evidence
- Regression: domain/live TestW45SampleVerdictDistinguishesInsufficientFromHealthy (adjacent INSUFFICIENT semantics), TestRuntimeAlertValidation; liveperformance TestW45SeamHappyPathEquivalence (cold-path isolation)
- Reviewer gate: NO (reporting/alerting-only; no action-vocabulary change)

### w5-2 — risk realization (28.166-170)
- Primary package: liveperformance
- New test file(s): liveperformance/risk_realization_test.go
- TDD-pinned: 28.169 — planned $100 / actual $175 → overrun $75, ratio 1.75x, policy response evaluated [pinned to WBS as-written; see CONFLICT-1 re ARCH digest "$180"]
- arch-req:
  - planned risk per trade sourced from live sizing/exit-plan records (new captured data), never re-derived
  - repeated-overrun (28.170) + loss-beyond-planned verdicts route via w5-8 response pipeline — no new dispatch mechanism
- Expected failures at PRE-TEST:
  - 28.169: fails: liveperformance has no loss-beyond-planned-risk metric/verdict
  - arch-req: fails: no planned-risk-per-trade capture on live records
- Regression: liveperformance TestW42Metrics_RiskUtilization_Live, TestW43SlippageTDD, TestW46WarningStatsOnly
- Reviewer gate: NO (pure computation; dispatch owned by w5-8)

### w5-3 — execution anomaly detection (28.171-176)
- Primary package: liveperformance (app-layer detectors)
- New test file(s): liveperformance/execution_anomaly_test.go
- TDD-pinned: 28.172 (bid 100 / ask 100.10, market BUY fill 102 → PRICE_OUT_OF_BAND anomaly); 28.173 (10 orders, 5 rejected within configured interval → degradation/alert per policy); 28.176 (suspected duplicate economic order → CRITICAL + runtime entries blocked)
- arch-req:
  - slippage-band + partial-fill-cluster detectors (28.171/174), config thresholds
  - duplicate routes CRITICAL alert + ACTIVATE_KILL_SWITCH_REQUEST through EXISTING kill-switch request path (request only, no direct activation)
  - anomalies = evidence for operator/alert only; never automatic strategy action
- Expected failures at PRE-TEST:
  - 28.172: fails: no price-out-of-band detector vs bid/ask bounds
  - 28.173: fails: no rejection-cluster detector
  - 28.176: fails: no duplicate economic-order detection / no CRITICAL+kill-switch-request wiring
- Regression: domain/live TestExecutionAnomalyValidation; liveperformance TestW43SlippageTDD, TestW43CountsWindow
- Reviewer gate: REVIEWER (CRITICAL severity + kill-switch request path + entries-blocked semantics)

### w5-4 — market/archive health (28.177-182)
- Primary package: liveperformance (archive-failure policy config + routing)
- New test file(s): liveperformance/archive_health_test.go
- TDD-pinned: 28.180 — "block live on archive failure" → runtime degrades/blocks new entries; "warn" config → warn only, trading unaffected
- arch-req:
  - "degrade" policy mode (TDD covers block + warn only)
  - exits unaffected in ALL modes
  - 28.177/178 reuse existing evidence (staleness evidence, livecapture gap records) — no re-implemented feed tracker; 28.179 hook only
- Expected failures at PRE-TEST:
  - 28.180 block: fails: no archive-failure policy in liveperformance config; entries never blocked
  - 28.180 warn: fails: no ARCHIVE_FAILURE alert routing
  - degrade: fails: same missing policy
- Regression: liveperformance TestColdPathComposition (failure isolation), TestW45SeamNilDepsFailClosed
- Reviewer gate: NO (entries-only blocking via w5-8 responses; exits untouched)

### w5-5 — regime/universe/volatility attribution (28.183-191)
- Primary package: internal/live (entry-path gates)
- New test file(s): internal/live/regimegate_test.go; internal/live/universegate_test.go
- TDD-pinned: 28.185 (allowed TRENDING, current CHOPPY → new entry denied, existing position exits still evaluate); 28.187 [Mandatory] (AAPL leaves universe with LONG 10 open → position visible, exit management continues, new AAPL increase denied)
- arch-req:
  - exit path NEVER consults regime allowance or universe membership (negative assertion in both TDDs)
  - new entry after removal denied (28.188, same shape as 187)
  - regime/vol attribution pure, Phase 12 vocabulary only (28.183/184/189/190); 28.191 hook only
- Expected failures at PRE-TEST:
  - 28.185: fails: entry submission does not consult current regime (gating bound at activation only)
  - 28.187: fails: entry path does not consult universe membership (increase not denied)
- Regression: domain/live TestLiveRunStatusTransitions, TestAssignmentStatusTransitions; live TestProtectionStateBlocks (adjacent entry blocking)
- Reviewer gate: REVIEWER (Mandatory TDD; entry/exit risk-control boundary)

### w5-6 — reports build + export (28.192-196)
- Primary packages: internal/report + live TradingRunSource adapter (ONLY new report seam; adapter package path per PLANNER at task start)
- New test file(s): internal/report/live_report_test.go; TradingRunSource adapter test (package per PLANNER; implements analytics.TradingRunSource over live repos)
- TDD-pinned: 28.192 (live report contains P&L, fees, drawdown, risk utilization, slippage, reconciliation events, gate denials, drift)
- arch-req:
  - determinism: byte-identical ExportJSON across two runs on identical input; CSV column schema explicit/stable (explicit schema edits, never silent)
  - batch/incremental parity: live snapshot via TradingRunSource agrees with full recompute (shared computeAnalyticsResult)
- Expected failures at PRE-TEST:
  - 28.192: fails: report model/StrategyReportInput lacks live-only fields (runtime duration, reconciliation events, gate denials, broker errors, drift)
  - adapter: fails: no live TradingRunSource adapter (analytics.Run has no live source)
  - determinism: fails: new live fields not yet exported / ExportJSON not asserted byte-stable over them
- Regression: report TestBuildStrategyReport, TestExportJSON, TestExportTradeRowsCSV, TestExportDailyPnLCSV; liveperformance TestParityFixtureNonVacuous, TestIncrementalHundredTradesEqualBatch
- Reviewer gate: NO (pure reports; structural boundary preserved)

### w5-7 — snapshots + comparison window (28.197-201)
- Primary packages: domain/live (additive snapshot fields + pure window selection), liveperformance (query surface)
- New test file(s): domain/live/comparison_window_test.go; liveperformance/snapshot_idempotency_test.go
- TDD-pinned: 28.199 (same fill delivered twice → snapshot/trade count changes once); 28.201 ("first 50 trades" window policy must never switch to "best 20" based on result)
- arch-req:
  - IdempotencyKey stable across additive snapshot fields: same run+strategy+watermark → same key (key inputs f(run, strategy, watermark) unchanged)
  - window policy = fixed/versioned struct persisted with each comparison; selection result-independent
- Expected failures at PRE-TEST:
  - 28.199: fails: no provider-fill-ID dedup / watermark double-contribution guard in snapshot path (source adapter new in w5-6)
  - 28.201: fails: no comparable-window selector (pure domain fn) in domain/live
  - key stability: fails: LivePerformanceSnapshot lacks BaselineComparison/Drift/Health fields (compile error)
- Regression: liveperformance TestSnapshotWatermarkIdempotent, TestIncrementalTradeCountMonotonic; domain/live TestBaselineComparability
- Reviewer gate: NO (pure domain; determinism)

### w5-8 — alerts + auto-pause (28.202-213)
- Primary package: liveperformance (RaiseAlert dedup + auto-pause triggers); live/livesafety semantics
- New test file(s): liveperformance/alert_dedup_test.go; liveperformance/auto_pause_test.go
- TDD-pinned: 28.211 (critical drift → strategy entries paused, existing exits remain active); 28.213 (no auto-resume while critical pause active)
- arch-req:
  - alert dedup: OPEN alert with same DedupKey not re-raised (single row); acknowledge clears dedup
  - auto-pause → exits still dispatched (28.212)
  - no response directly mutates strategy state (28.210, Mandatory)
  - severity mapping ERROR→WARN documented; no additive ERROR constant
  - invariant: alert-repo failure injected → run continues, exits still dispatchable, no panic
- Expected failures at PRE-TEST:
  - 28.211: fails: no auto-pause trigger on critical drift (dispatch not wired to livesafety.Pause / run paused mode)
  - 28.213: fails: no manual-only resume guard (liveperformance could resume automatically)
  - dedup: fails: RaiseAlert has no open-alert/DedupKey check (re-raises)
  - invariant: fails: RaiseAlert error path not yet isolated/counted (stat+log, no propagation)
- Regression: liveperformance TestW46PauseDispatch, TestW46BoundedActionsNoMutation; livesafety TestKillSwitch_BlocksSubsequentEvaluation, TestArm_FailsWithoutAuthorization; domain/live TestRuntimeAlertValidation
- Reviewer gate: REVIEWER (risk controls: pause/resume/response dispatch/dedup)

### w5-9 — baseline invalidation / shadow paper / replay / research export (28.214-222)
- Primary packages: internal/live (signal-level shadow mirror + parity; offline replay), domain/live (NOT_COMPARABLE marking)
- New test file(s): internal/live/shadow_parity_test.go; domain/live/baseline_invalidation_test.go; internal/live/replay_test.go
- TDD-pinned: 28.219 (live + shadow paper on same canonical market sequence → identical SignalIDs / logical signal contents before execution branching)
- arch-req:
  - NOT_COMPARABLE marking on version/config/venue change via BaselineReference.IsComparableTo only (single comparability point); venue change A→B recorded (28.216) — never silently misleading drift
  - shadow mirror fire-and-forget after live signal generation, before risk/intent/execution; zero effect on live decisions; paper output never feeds live
  - post-session replay OFFLINE through Phase 11 (livecapture archive input); not on live execution path
  - research export via canonical read tools; no automatic research→live feedback (28.222)
- Expected failures at PRE-TEST:
  - 28.219: fails: no shadow-paper mirror hook in live signal path
  - NOT_COMPARABLE: fails: no NOT_COMPARABLE marker/recorded reason on comparability failure
  - replay: fails: no offline replay pass over archived live day
- Regression: domain/live TestBaselineComparability, TestW44TDDVersionNotComparable, TestW44VenueMismatchGateRefused, TestW44ExecutionConfigGateRefused
- Reviewer gate: REVIEWER (hook inside live signal path + baseline comparability financial correctness)

## Shared test infrastructure (wave level)
- Fake clock / manual ticker: none in wave-4 inventory → needed by w5-0 (cadence), w5-3 (rejection-cluster interval), w5-4 (staleness). Test-only helper (cmd/liveperf test package; shared testutil only if reuse demanded). Production change limited to injectable ticker in the composition root.
- In-memory adapters (existing; name only): adapters/runtimealertrepo, adapters/pauserepo, adapters/armstaterepo, adapters/authorizationrepo, adapters/killswitchrepo, adapters/gatedecisionrepo, adapters/driftobservationrepo, adapters/performancesnapshotrepo, adapters/liveorderrepo, adapters/livefillrepo, adapters/liverunrepo, adapters/liveassignmentrepo, adapters/liveruntimestaterepo, adapters/regimeobservationrepo, adapters/universesnapshotrepo, adapters/capturestore, adapters/adapterauditrepo, adapters/analyticsrepo.
- New fixture data: planned-risk-per-trade annotation (w5-2); canonical market sequence for live+paper parity — reuse existing strategy/paper fixtures, located at PRE-TEST (w5-9).
- OPEN ITEM (not a conflict): ARCH references existing boundary test "report layer only depends on analytics" (TestReportLayerOnlyDependsOnAnalytics) — not present in the wave-4 inventory of the 4 packages. TESTER to locate at PRE-TEST; if absent, add as arch-req regression under w5-6.

## Conflicts (route to ARCHITECT — NOT resolved here)
- CONFLICT-1 (TDD-28.169 numbers, NON-BLOCKING for plan persistence): WBS TDD slice L3726-3748 pins actual $175 → overrun $75, ratio 1.75x. ARCH_WAVE5.md (WBS digest L26 + Test requirements L175) states "planned $100 / actual $180". Plan pins the w5-2 test to WBS as-written values ($175/$75/1.75x). Needs ARCHITECT reconciliation before w5-2 PRE-TEST gate.
