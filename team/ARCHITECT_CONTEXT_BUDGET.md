# Phase 28 — Sub-Agent Context Budget (64k per invocation; 160k model ceiling)

Authority: operator directive 2026-09-18 (64k TOTAL per sub-agent invocation, PLANNED; model hard ceiling 160k). 64k covers EVERYTHING an invocation consumes: system/role overhead + task brief + every file read + all tool I/O (test runs, build errors, fix cycles). 160k is a failure last-resort, never a plan: a task that cannot be planned within 64k goes back to PLANNER for decomposition. 64k is also the speed target — small contexts execute faster and never approach the ceiling. Read by PLANNER inside its own 64k.

Measured inputs (src/): WBS doc `wbs/Phase 28 Live Trading.md` 5,162 L / 71,843 B. Packages (files / LOC): live 24/3025 (incl. 2 test files 815 L), liveperformance 9/840, domain/live 19/1655, execution 16/1622, reconciliation 20/2744, livesafety 15/2339, risk 17/1550, analytics 11/1937, tui 120/11203, report 5/309, strategy 15/1419, paper 34/6160, ports 73/2831, adapters 135/15327, livecapture 23/3305, broker 26/3600, cmd/tui 1/246.

## 1. BUDGET MODEL

Token-per-unit (measured, then rounded conservative for planning):
- Go source: sample `internal/live/assignment.go` = 170 L / 4,793 B = 28.2 B/L. At 3.2–4 B/token → 7.0–8.8 tok/L. **Planning constant: 8 tok/LOC** (underestimate = budget blowout; dense identifier code lands high).
- WBS/TDD markdown: measured ~12 B/L (e.g. W4 WBS slice 449 L/5,346 B). **Planning constant: 3 tok/L** (DoD slice is denser, ~27 B/L → ~4.5 tok/L).
- Tool I/O: `go test -race ./pkg/` verbose with a failure ≈ 3–15k tok; green full-package run ≈ 2–4k; `go vet`+`go build` ≈ 1–3k.

64k allocation for a typical ENGINEER task:
| Bucket | tok | note |
|---|---|---|
| System prompt (Hermes CLI base) | 5,000 | fixed |
| Role def (ENGINEER.md) + workflow ref | 3,000 | fixed |
| Task brief (ACTIVE_TASK.md) | 2,000 | 1 primary objective |
| WBS slice | 1,000 | per-task line-range only |
| TDD slice | 1,000 | per-task line-range only |
| Source files (primary pkg, named) | 11,000 | ~1,400 LOC @8 |
| Test files (existing refs + new) | 11,000 | ~1,400 LOC |
| Shared-context refs (map, named fn) | 1,000 | referenced, never re-pasted |
| **PLANNED INPUT subtotal** | **27,000** | **HARD CAP** |
| Tool-I/O headroom (build+test+-race, 3 fix cycles) | 26,000 | reserve |
| Output / summary reserve | 1,000 | |
| Safety margin | 2,000 | |
| **TOTAL** | **64,000** | |

Hard planning rules:
- **Max planned INPUT ≤ 27k** (everything the brief tells the agent to read: brief + WBS + TDD + source + tests + shared refs) ≈ 3,400 LOC of Go.
- **Tool-I/O headroom ≥ 26k.** Justification: TDD = run-fail → implement → run → iterate. 3 cycles × ~6k + full gate (incl. one `-race` run) ~4–8k ≈ 25k < 26k. Allowing 37k input would cut headroom to 16k → mid-iteration truncation on chatty `-race` failures.
- **No single read > ~11k tok (~1,400 LOC).** Never read `tui` (89.6k), `adapters` (122.6k), or `paper` (49.3k) as a package — each alone breaks the input cap.

Whole-package read cost @8 tok/LOC (fits only when package + tests ≤ 22k, i.e. package ≤ ~15k with small test additions):
- report 2.5k ✓ · cmd/tui 2.0k ✓ · liveperformance 6.7k ✓ — safe to read whole.
- analytics 15.5k / domain/live 13.2k / execution 13.0k / risk 12.4k / strategy 11.4k — whole read only with minimal new tests (≤ ~7k); otherwise name specific files.
- **live 24.2k ✗** · **reconciliation 22.0k ✗** · **livesafety 18.7k ✗** · **ports 22.6k ✗** · **livecapture 26.4k ✗** · **broker 28.8k ✗** · **paper 49.3k ✗** · **tui 89.6k ✗** · **adapters 122.6k ✗** — name specific files only.

## 2. WBS/TDD SLICING RULE

Task briefs cite ONLY a slice. Exact line ranges in the WBS doc (verified by grep of `## WBS-28.N`, `## TDD-28.N`, `## Integration Test`):

WBS item slices:
| Wave | WBS range | Lines | TDD slice | Lines |
|---|---|---|---|---|
| w1 (remainder 28.1–30) | 28.1–30 | 167–606 | 28.5/12/13/15/25/27/30 | 2865–3000 |
| Wave 2 | 28.31–75 | 607–1003 | 28.33–71 (exit/risk/safety/order/fill/position) | 3001–3291 |
| Wave 3 | 28.76–105 | 1004–1256 | 28.80–104 (protective/oco/eod/crypto/pause/stop) | 3292–3463 |
| Wave 4 | 28.106–162 | 1257–1705 | 28.108–157 (analytics/execq/baseline/drift) | 3464–3725 |
| Wave 5 | 28.163–222 | 1706–2236 | 28.169–219 (riskreal/anomaly/archive/univ/regime/report/snapshot/pause/shadow) | 3726–3932 |
| Wave 6 | 28.223–249 | 2237–2444 | 28.226–248 (recovery/pending/order/conc/storm/config) | 3933–4090 |
| Wave 7 | 28.250–310 | 2445–2864 | — (uses Integration slice below) | — |

Per-wave WBS slice ≈ 1.2–1.6k tok; per-wave TDD slice ≈ 0.3–1.0k tok. Together < 2.5k — the dominant cost is always source.

Integration tests (L4091–4790, ~2.1k whole): Critical A–W = L4091–4700; Additional AA–AF = L4701–4790. Per-test start lines (end = next test −1): A4093 B4130 C4149 D4185 E4208 F4230 G4260 H4278 I4296 J4317 K4335 L4357 M4390 N4411 O4438 P4455 Q4474 R4500 S4522 T4553 U4571 V4588 W4608 X4628 Y4647 Z4671 AA4703 AB4715 AC4731 AD4747 AE4757 AF4779. A single test ≈ 0.1k tok.
DoD = L4791–5070 (~1.9k). Recommended Agent Execution Order = L5071–5162 (~0.3k).

## 3. PER-WAVE ARCHITECTURE FOOTPRINT

Shared context — reference by file+function, NEVER re-paste into briefs:
- `internal/live/run_test.go` (408 L): `driveTo`, test deps/Service builder, event sink, mock broker.
- `internal/live/startup_test.go` (407 L): full-pipeline rig (reconciliation+strategy+tradeintent+risk+execution), EMA200 warm-up probe, driveTo-RECOVERING.
- `internal/domaintest/reconciliation.go:153` `NewReconciliationRig` / `:159` `NewReconciliationRigWithRiskConfig` (Phase 26 rig); `internal/domaintest/livesafety.go` (Phase 27 rig).
- 9 in-memory live port adapters (ports/* + adapters/*).

Per-wave primary packages (measured). One primary package under test per task.
- **w1-3** (28.17–20): live `eventloop.go`(169)+`marketevent.go`(72)+`service.go`(222)+`run.go`(106)=569 L≈4.6k + ports/clock. Fits.
- **w1-4** (28.24/26/27/28/29/30): live `strategyeval.go`(367)+`submitgate.go`(77)+`fills.go`(91)+`service.go`(222)=757 L≈6.1k. Fits.
- **w1-5** (28.1/51 import guard): live import graph + go.mod + linktest. <2k. Fits.
- **w1-6** (28.25 parity, EXIT GATE): live `strategyeval.go`+`service.go` + paper `strategyrun.go`(406)+`runtime.go`(191)+`exitrun.go`(205) + analytics run path ≈ 5–8k. Fits **only if paper is named-file, never whole package** (paper whole=49.3k ✗).
- **Wave 2** (28.31–75, 7 sub-tasks, 1 pkg each): 2a exit→live+domain/exit; 2b risk→live+risk(12.4k); 2c gate→live+livesafety(18.7k); 2d exec coordinator→live+execution(13.0k)+reconciliation(named `unknown.go`/`repair.go` ~8k, NOT whole 22.0k); 2e cancel/replace→execution+live; 2f fills→live`fills.go`+execution`fill.go`; 2g position→live+domain/live. All fit. (28.76/77 mark policy = M, small gap-fill attach to 2g or W3a.)
- **Wave 3** (28.76–105): 3a mark→domain/live+live; 3b protective→live`protective.go`(107)+livesafety; 3c OCO→live`oco.go`(69)+execution; 3d EOD→live`sessionend.go`(99); 3e crypto→live`marketevent.go`+risk; 3f corporate→live`corporateaction.go`(70); 3g short borrow(M)→domain+live; 3h pause/stop→live`pause.go`(41)+`stop.go`(72)+livesafety. All fit.
- **Wave 4** (28.106–162): **primary = liveperformance whole (6.7k, FITS) + analytics whole (15.5k, FITS).** 4a source/parity, 4b metrics, 4c exec quality, 4d baselines, 4e drift/policy — all within one read. The one wave where the whole primary package fits.
- **Wave 5** (28.163–222): liveperformance + live(alert/drift) + analytics; 5d archive health touches livecapture (26.4k — name files, not whole); 5f reports→report whole(2.5k)+liveperformance. All fit.
- **Wave 6** (28.223–249): live`checkpoint.go`(105)/`startup.go`(83)/`service.go`(222)+domain/live checkpoint+reconciliation rig; TASKS w6-6 (concurrency) + w6-7 (storm 28.243/245) under `-race`. All fit.
- **Wave 7** (28.250–310): 7a queries/7b events/7c audit/7d observability→live`query.go`(65)/`events.go`(54)/`stats.go`(75). 7e TUI ✗(tui 89.6k)→split §5. 7f reports→report whole(2.5k)+new live types. 7g repos ✗(adapters 122.6k)→split §5. 7h fixtures(M)→NEW testutil. 7i integration(M)→NEW, split §5.

## 4. CONSTRAINTS FOR THE PLANNER

1. One primary package under test per task; the brief names the primary pkg explicitly.
2. WBS + TDD cited by line-range only (table §2); never "read the phase".
3. Any task touching `tui`/`adapters`/`paper` names specific files/screens/port-pairs, NEVER the package (each exceeds the input cap alone).
4. Source read ≤ ~1,400 LOC (11k); if a task's minimal source + tests > 22k combined, split.
5. Integration suite (A–AF) shares ONE harness file (§6); each integration task loads harness + its own test file only.
6. Test-first TDD order preserved: TESTER gate (confirm expected failure) before ENGINEER; brief always lists the TDD slice + expected-failure reason.
7. Existing test files (run_test.go, startup_test.go) referenced by file+function, never re-pasted or duplicated.
8. `-race` tasks (Wave 6 w6-6/w6-7, Wave 2/3 concurrency, Wave 7i-*) must reserve extra tool-I/O (race output is chatty) — keep source on the low side for those.
9. Reviewer-gated areas (risk, execution, orders, positions, persistence, concurrency, external APIs) get the same input discipline; no budget inflation for review.

## 5. TASKS THAT CANNOT FIT (split required)

**TUI (WBS-28.273–279)** — whole `internal/tui` = 89.6k tok, 1.6× input cap. Split (each names 1 new `screen/live/<file>.go` + at most ONE existing screen dir ≤550 L as reference):
- 7e-1 live runtime dashboard (28.273, M): NEW `screen/live/runtime.go`+test; ref `screen/livesafety`(419) or `screen/dashboard`(470). ~12k.
- 7e-2 live strategy screen drift/baseline (28.274, P): extend `screen/strategies`(300)+`screen/live`. ~4k.
- 7e-3 live execution quality view (28.275, M): NEW `screen/live/quality.go`. ~4k.
- 7e-4 performance drift view + SourceLive (28.276, M): extend `screen/performance`(292), add `SourceLive` to `query.go`(L20–21)+`model.go`. ~4k.
- 7e-5 command bindings pause/resume/graceful-stop/stop-and-flatten (28.277–279): `tui/command/command.go`(169)+`screen/live` bindings. ~4k.

**Repos (WBS-28.286–292)** — whole `internal/adapters` = 122.6k, 2.2× budget. Split per port-group (each: 1–3 ports/* dirs + their adapter file + contract/race test; never the adapters package):
- 7g-1: liverunrepo + liveassignmentrepo (28.286–287)
- 7g-2: liveruntimestaterepo + liveorderrepo + livefillrepo (28.288–289)
- 7g-3: livesafetyauditrepo + performancesnapshotrepo + driftobservationrepo + runtimealertrepo (28.290–292)

**Integration suite (WBS-28.310, Tests A–AF, 32 tests)** — full app wiring; 32 tests cannot fit one task. Split:
- 7i-0 SHARED HARNESS (build FIRST): NEW `internal/live/integration/harness.go` = buildApp (wire live.Service + in-memory adapters + `domaintest.NewReconciliationRig` + fake broker/venue + fake clock + fixture loader). Reads live`service.go`(222)+deps+named adapters+domaintest. ~500–700 L ≈ 4–5.6k.
- 7i-1..7i-6: ~5 tests each; each reads harness.go (~5k, already built) + own new test file (~200–400 L ≈ 1.6–3.2k) + that test's integration slice (~0.1k) + WBS-28.310 (L2829–2864). Fits. Grouping (mandated first, priority order from doc L5085+):
  - G1: B,D,E,F,G · G2: H,I,J,L,N · G3: O,Q,R,S,T · G4: U,W,X,Y,Z · G5: AA,AB,AD,AE,AF · G6 (completers): A,C,K,M,P,AC,V.
  - `t.Parallel()` only where the fake venue is isolated.

**Parity (w1-6, TDD-28.25)** — conditionally fits. Keep to named entry points (live`strategyeval.go`+paper`strategyrun.go`/`runtime.go`+canonical evaluator+analytics ≈ 8–10k). If a task reaches for whole paper (49.3k), split into "live emits canonical signals" (w1-4) + "parity comparison harness" (small).

## 6. SHARED CONTEXT ARTIFACTS (build once early; stop re-paying)

Build these in the early waves so later briefs reference, not re-read:
1. **Integration harness** — `internal/live/integration/harness.go`: buildApp + fake broker/venue + fake clock + fixture loader. **~600 LOC (≈4.8k tok).** Built in 7i-0; every integration task references by function (re-read ≤5k only if changed).
2. **Fixture set** — `internal/live/testutil/`: the 11 fixtures for WBS-28.299–309 (normal trade, partial fill, stop-loss, trailing stop, session-end, risk lock, gate denial, unknown submit, perf drift, exec degradation, restart/open-position). **~500 LOC (≈4.0k tok).** Shared by unit + integration tests.
3. **internal/live context map** — one line per file → responsibility, for all 24 live files (e.g. `run.go — run lifecycle/status transitions`; `submitgate.go — Phase 27 gate check; submit.go — gated SubmitApproved; fills.go — ApplyFill fan-out; …`). **~24 lines (<1k tok).** Lets briefs name files precisely without re-reading the package.

Build order: context map (trivial, do in w1) → fixtures (before Wave 2 fills/position tasks and Wave 7) → harness (7i-0, before any integration test).
