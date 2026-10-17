package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/goanalysis"
	"overseer/internal/gorefactor"
	"overseer/internal/hardening"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// Final hardening (P16-119 … P16-133). One hardening operation per Step:
// analysis, planning, each consolidation operation, rescans. Mechanical
// edits are applied by internal/gorefactor (language-aware, idempotent);
// the worker only repairs failures (HARDENING_FIX). The overseer decides
// eligibility, import-cycle safety, gates, validation and completion.

func (e *Engine) hardeningEnabled(st *engineState) bool {
	return st.hard.Enabled && st.hard.DuplicateFunctions.Enabled
}

// repoFingerprint is the repository identity guarding against external
// modification (WBS 16.144).
func (e *Engine) repoFingerprint(ctx context.Context, st *engineState) (string, error) {
	if e.d.Inspector == nil {
		return "", nil
	}
	snap, err := e.d.Inspector.Inspect(ctx, st.cwd())
	if err != nil {
		return "", err
	}
	return snap.Fingerprint, nil
}

// ensurePass returns the pass of the execution's finalization generation,
// creating it once with its REQUIRED baseline checkpoint (WBS 16.87–16.89).
func (e *Engine) ensurePass(ctx context.Context, st *engineState, step *persistence.StepRecord) (*persistence.HardeningPassRecord, *execution.StepResult, error) {
	gen := st.rec.FinalizationGeneration
	if p, err := e.d.Repo.GetPass(ctx, nil, st.rec.ID, gen); err == nil {
		return p, nil, nil
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, nil, err
	}
	target := uuid.NewSHA1(st.rec.ID, []byte(fmt.Sprintf("hardening-%d", gen)))
	cp, err := e.checkpoint(ctx, st, step, execution.RepoCheckpointHardeningBaseline, target, nil, execution.CheckpointRequired, "final hardening baseline")
	if err != nil {
		return nil, nil, err
	}
	if cp.Status != persistence.CheckpointCreated {
		reason, ev := e.checkpointFailure(ctx, st, "final hardening baseline checkpoint failed", cp.Error, map[string]any{"error": cp.Error})
		r, err := e.requireRecovery(ctx, st, step, reason, ev)
		return nil, &r, err
	}
	fp, err := e.repoFingerprint(ctx, st)
	if err != nil {
		return nil, nil, err
	}
	p, err := hardening.NewPass(st.rec.ID, st.rec.ProjectID, gen, e.d.Now())
	if err != nil {
		return nil, nil, err
	}
	cpID := cp.ID
	p.BaselineCheckpointID = &cpID
	rec, created, err := e.d.Repo.CreatePass(ctx, &persistence.HardeningPassRecord{Pass: *p, BaselineFingerprint: fp, RepoFingerprint: fp})
	if err != nil {
		return nil, nil, err
	}
	if created {
		_ = e.event(ctx, nil, st, execution.EventHardeningStarted, map[string]any{"pass_id": rec.ID.String(), "generation": gen,
			"baseline_checkpoint_id": cpID.String(), "baseline_commit": cp.Commit})
	}
	return rec, nil, nil
}

// setPass persists a pass status / field change.
func (e *Engine) setPass(ctx context.Context, p *persistence.HardeningPassRecord, mut func(*persistence.HardeningPassRecord)) (*persistence.HardeningPassRecord, error) {
	next := *p
	mut(&next)
	return e.d.Repo.UpdatePass(ctx, nil, &next)
}

// handleFinalHardening drives the hardening pass (WBS 16.86–16.149) and
// the repair loop of a failed final validation (WBS 16.152).
func (e *Engine) handleFinalHardening(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if !e.hardeningEnabled(st) {
		refs := persistence.StepRefs{Detail: map[string]any{"hardening": "disabled"}}
		if run, err := e.d.Repo.LatestValidationRun(ctx, nil, st.rec.ID, execution.ScopeFinal, st.rec.ID); err == nil && run.Status == string(execution.ValidationFail) {
			return e.repairFinal(ctx, st, step, st.rec.ID, run, refs)
		}
		return e.advance(ctx, st, step, "COMPLETE", advanceOpts{Refs: refs,
			Events: []pendingEvent{{Type: execution.EventHardeningCompleted, Payload: map[string]any{"disabled": true}}}})
	}
	pass, stop, err := e.ensurePass(ctx, st, step)
	if err != nil || stop != nil {
		if stop == nil {
			return execution.StepResult{}, err
		}
		return *stop, err
	}
	refs := persistence.StepRefs{Detail: map[string]any{"pass_id": pass.ID.String()}}
	// Back from a worker repair: the worker changed the repository on
	// purpose — adopt the new fingerprint.
	if prev := e.lastApplied(ctx, st, execution.ActionHardeningFix); prev != nil && st.last != nil && st.last.ID == prev.ID {
		fp, err := e.repoFingerprint(ctx, st)
		if err != nil {
			return execution.StepResult{}, err
		}
		if pass, err = e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.RepoFingerprint = fp }); err != nil {
			return execution.StepResult{}, err
		}
	}
	switch pass.Status {
	case hardening.PassDone:
		return e.afterHardening(ctx, st, step, pass, refs)
	case hardening.PassRecoveryRequired, hardening.PassFailed:
		return e.requireRecovery(ctx, st, step, "hardening pass "+string(pass.Status)+": "+pass.RecoveryReason, map[string]any{"pass_id": pass.ID.String()})
	case hardening.PassPending:
		if _, err := e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.Status = hardening.PassAnalyzing }); err != nil {
			return execution.StepResult{}, err
		}
		return e.finishNoTransition(ctx, st, step, "ANALYZING", refs)
	case hardening.PassAnalyzing:
		return e.hardeningAnalyze(ctx, st, step, pass, refs)
	case hardening.PassPlanning:
		return e.hardeningPlan(ctx, st, step, pass, refs)
	case hardening.PassExecuting:
		return e.hardeningExecute(ctx, st, step, pass, refs)
	case hardening.PassValidating:
		if _, err := e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.Status, p.Round = hardening.PassAnalyzing, p.Round+1 }); err != nil {
			return execution.StepResult{}, err
		}
		return e.finishNoTransition(ctx, st, step, "RESCAN", refs)
	}
	return e.requireRecovery(ctx, st, step, "unknown hardening pass status "+string(pass.Status), nil)
}

// afterHardening: the pass is DONE. A failed final validation since then
// routes to a bounded worker repair; otherwise the final validation runs.
func (e *Engine) afterHardening(ctx context.Context, st *engineState, step *persistence.StepRecord, pass *persistence.HardeningPassRecord, refs persistence.StepRefs) (execution.StepResult, error) {
	run, err := e.d.Repo.LatestValidationRun(ctx, nil, st.rec.ID, execution.ScopeFinal, st.rec.ID)
	if err == nil && run.Status == string(execution.ValidationFail) {
		return e.repairFinal(ctx, st, step, pass.ID, run, refs)
	}
	return e.advance(ctx, st, step, "COMPLETE", advanceOpts{Refs: refs})
}

// repairFinal handles a failed final validation (WBS 16.152): once
// repaired since the failure, validate again; otherwise a bounded repair.
func (e *Engine) repairFinal(ctx context.Context, st *engineState, step *persistence.StepRecord, target uuid.UUID, run *persistence.ValidationRunRecord, refs persistence.StepRefs) (execution.StepResult, error) {
	attempts, err := e.d.Repo.ListFixAttempts(ctx, st.rec.ID, &target)
	if err != nil {
		return execution.StepResult{}, err
	}
	for _, a := range attempts {
		if a.FailedRunID != nil && *a.FailedRunID == run.ID && a.DispatchID != nil {
			return e.advance(ctx, st, step, "COMPLETE", advanceOpts{Refs: refs})
		}
	}
	return e.hardeningFixRoute(ctx, st, step, target, &run.ID, map[string]any{"final_validation_run_id": run.ID.String()}, refs)
}

// hardeningFixRoute records a bounded repair attempt (WBS 16.131/16.132)
// and routes to HARDENING_FIX; exhausted → recovery.
func (e *Engine) hardeningFixRoute(ctx context.Context, st *engineState, step *persistence.StepRecord, target uuid.UUID, runID *uuid.UUID,
	evidence map[string]any, refs persistence.StepRefs) (execution.StepResult, error) {
	attempts, err := e.d.Repo.ListFixAttempts(ctx, st.rec.ID, &target)
	if err != nil {
		return execution.StepResult{}, err
	}
	used, err := e.attemptsSinceReset(ctx, st, attempts, LimitHardeningFixes)
	if err != nil {
		return execution.StepResult{}, err
	}
	if execution.AttemptDecision(used, st.engine.MaxFixAttempts) == execution.AttemptExhausted {
		evidence["attempts"] = used
		return e.requireRecovery(ctx, st, step, reasonHardeningFixExhausted, evidence)
	}
	evidence["step_seq"] = step.Seq
	f := &persistence.FixAttemptRecord{ExecutionID: st.rec.ID, TargetKind: persistence.FixTargetHardening, TargetID: target, FailedRunID: runID,
		Attempt: len(attempts) + 1, Evidence: evidence}
	if err := e.d.Repo.CreateFixAttempt(ctx, nil, f); err != nil {
		return execution.StepResult{}, err
	}
	return e.advance(ctx, st, step, "FIX", advanceOpts{Refs: refs, Events: []pendingEvent{{Type: execution.EventFixAttempt,
		Payload: map[string]any{"target_id": target.String(), "attempt": f.Attempt, "kind": "HARDENING"}}}})
}

// handleHardeningFix dispatches the repair prompt (failure evidence in
// TEST_STATE) and returns to FINAL_HARDENING, which re-validates — the
// worker's claim that the fix works is never sufficient (WBS 16.52).
func (e *Engine) handleHardeningFix(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	out, stop, err := e.dispatchStep(ctx, st, step, DispatchRequest{})
	if err != nil || stop != nil {
		if stop == nil {
			return execution.StepResult{}, err
		}
		return *stop, err
	}
	id := out.dispatch.ID
	// Link the dispatch to the open fix attempt.
	if attempts, err := e.d.Repo.ListFixAttempts(ctx, st.rec.ID, nil); err == nil {
		for i := len(attempts) - 1; i >= 0; i-- {
			if attempts[i].TargetKind == persistence.FixTargetHardening && attempts[i].DispatchID == nil {
				_ = e.d.Repo.SetFixDispatch(ctx, nil, attempts[i].ID, id)
				break
			}
		}
	}
	return e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), advanceOpts{Refs: persistence.StepRefs{DispatchID: &id}})
}

// ---------------------------------------------------------------------------
// Analysis (P16-120) and planning (P16-121).

func (e *Engine) loadModule(ctx context.Context, st *engineState) (*goanalysis.Module, error) {
	return goanalysis.LoadModule(ctx, st.cwd(), goanalysis.Options{GoBinary: e.d.GoBinary, Exclusions: st.hard.Exclusions})
}

func moduleLayers(m *goanalysis.Module, s hardening.Settings) hardening.Layers {
	var l hardening.Layers
	for _, d := range s.DomainPackages {
		l.Domain = append(l.Domain, path.Join(m.Path, d))
	}
	for _, d := range s.InfraPackages {
		l.Infra = append(l.Infra, path.Join(m.Path, d))
	}
	return l
}

func newBase(m *goanalysis.Module) string {
	for _, p := range m.Packages {
		if p.RelDir == "internal" || strings.HasPrefix(p.RelDir, "internal/") {
			return "internal"
		}
	}
	return ""
}

func memberKeys(g []hardening.FunctionInfo) (map[string]bool, map[string]string, []string) {
	keys, pkgs := map[string]bool{}, map[string]string{}
	var names []string
	for _, m := range g {
		keys[m.QualifiedName()] = true
		pkgs[m.ImportPath] = m.Package
		names = append(names, m.QualifiedName())
	}
	return keys, pkgs, names
}

// hardeningAnalyze builds the language-aware inventory, groups duplicates,
// classifies every group and persists it (WBS 16.91–16.109); skipped groups
// carry their reason and join the accepted-duplication baseline (16.148).
// Then the round decision (16.135–16.138).
func (e *Engine) hardeningAnalyze(ctx context.Context, st *engineState, step *persistence.StepRecord, pass *persistence.HardeningPassRecord, refs persistence.StepRefs) (execution.StepResult, error) {
	if stop, err := e.guardRepo(ctx, st, step, pass); stop != nil || err != nil {
		if stop == nil {
			return execution.StepResult{}, err
		}
		return *stop, err
	}
	m, err := e.loadModule(ctx, st)
	if err != nil {
		return e.requireRecovery(ctx, st, step, "hardening analysis could not load the module", map[string]any{"error": err.Error()})
	}
	fns, err := goanalysis.Inventory(m)
	if err != nil {
		return execution.StepResult{}, err
	}
	all := map[string]bool{}
	for _, f := range fns {
		all[f.QualifiedName()] = true
	}
	refsByKey := goanalysis.References(m, all)
	for i := range fns {
		fns[i].References = refsByKey[fns[i].QualifiedName()]
	}
	retained, err := e.d.Repo.ListRetained(ctx, st.rec.ProjectID)
	if err != nil {
		return execution.StepResult{}, err
	}
	actx := hardening.AnalysisContext{ModulePath: m.Path, NewBase: newBase(m), Packages: m.PackageInfos(), Graph: m.ImportGraph(),
		Layers: moduleLayers(m, st.hard), Settings: st.hard, Retained: retained}
	groups := hardening.GroupByFingerprint(fns)
	safe := 0
	for _, g := range groups {
		checks, cl, why := hardening.Assess(g, actx)
		rec := &persistence.HardeningGroupRecord{PassID: pass.ID, Round: pass.Round, Key: g.ID, Fingerprint: g.Fingerprint, Members: g.Members,
			Classification: cl, Reason: why, Status: hardening.GroupSkipped}
		if cl == hardening.ClassSafe {
			rec.Destination = checks.Destination
			rec.Status = hardening.GroupAnalyzed
			keys, _, _ := memberKeys(g.Members)
			tests := goanalysis.TestInventory(m, keys)
			var cases []hardening.TestCase
			for _, t := range tests {
				cases = append(cases, t.Cases...)
			}
			rec.Tests, rec.Matrix, rec.TestsBefore = tests, hardening.BuildTestMatrix(cases), len(tests)
			safe++
		}
		saved, err := e.d.Repo.UpsertGroup(ctx, rec)
		if err != nil {
			return execution.StepResult{}, err
		}
		if saved.Status == hardening.GroupSkipped {
			_ = e.d.Repo.UpsertRetained(ctx, st.rec.ProjectID, g.Fingerprint, string(cl), why)
			_ = e.event(ctx, nil, st, execution.EventHardeningGroupSkipped, map[string]any{"group": g.ID, "classification": string(cl), "reason": why})
		}
	}
	verdict := hardening.AfterAnalysis(pass.Round, st.hard.DuplicateFunctions.MaxRounds, safe)
	_ = e.event(ctx, nil, st, execution.EventHardeningAnalyzed, map[string]any{"pass_id": pass.ID.String(), "round": pass.Round,
		"functions": len(fns), "groups": len(groups), "safe": safe, "decision": string(verdict)})
	refs.Detail["decision"] = string(verdict)
	switch verdict {
	case hardening.RoundConsolidate:
		if _, err := e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.Status = hardening.PassPlanning }); err != nil {
			return execution.StepResult{}, err
		}
		return e.finishNoTransition(ctx, st, step, "PLANNING", refs)
	case hardening.RoundMaxRounds:
		cur, err := e.d.Repo.ListGroups(ctx, pass.ID, pass.Round)
		if err != nil {
			return execution.StepResult{}, err
		}
		for _, g := range cur {
			if g.Classification == hardening.ClassSafe {
				g.Status = hardening.GroupRemaining
				if err := e.d.Repo.UpdateGroup(ctx, g); err != nil {
					return execution.StepResult{}, err
				}
			}
		}
	}
	return e.completePass(ctx, st, step, pass, verdict, len(fns), refs)
}

// completePass persists the report and moves the pass to DONE when the
// completion rule holds (WBS 16.146–16.149).
func (e *Engine) completePass(ctx context.Context, st *engineState, step *persistence.StepRecord, pass *persistence.HardeningPassRecord,
	verdict hardening.RoundVerdict, scanned int, refs persistence.StepRefs) (execution.StepResult, error) {
	rep, recs, err := e.buildReport(ctx, pass, verdict, scanned)
	if err != nil {
		return execution.StepResult{}, err
	}
	// Only the final round's groups decide completion (earlier rounds'
	// consolidated groups are done; recurring skipped groups re-appear).
	var last []hardening.GroupRecord
	for _, g := range recs {
		if g.Round == pass.Round {
			last = append(last, g)
		}
	}
	ok, why := hardening.HardeningDone(last, true, false)
	if !ok {
		return e.requireRecovery(ctx, st, step, "hardening cannot complete: "+why, map[string]any{"pass_id": pass.ID.String()})
	}
	if err := e.d.Repo.UpsertReport(ctx, pass.ID, rep); err != nil {
		return execution.StepResult{}, err
	}
	now := e.d.Now().UTC()
	if _, err := e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.Status, p.CompletedAt = hardening.PassDone, &now }); err != nil {
		return execution.StepResult{}, err
	}
	return e.finishNoTransition(ctx, st, step, "HARDENING_DONE", refs, pendingEvent{Type: execution.EventHardeningCompleted,
		Payload: map[string]any{"pass_id": pass.ID.String(), "rounds": rep.Rounds, "termination": string(verdict), "consolidated": rep.ConsolidatedGroups,
			"skipped": rep.SkippedGroups, "remaining_safe": rep.RemainingSafeGroups, "warning": rep.Warning}})
}

// buildReport aggregates every round's groups (WBS 16.146).
func (e *Engine) buildReport(ctx context.Context, pass *persistence.HardeningPassRecord, verdict hardening.RoundVerdict, scanned int) (hardening.Report, []hardening.GroupRecord, error) {
	groups, err := e.d.Repo.ListGroups(ctx, pass.ID, 0)
	if err != nil {
		return hardening.Report{}, nil, err
	}
	rep := hardening.Report{FunctionsScanned: scanned, Rounds: pass.Round, Termination: verdict, ValidationResult: "PASS", Groups: []hardening.GroupRecord{}}
	if verdict == hardening.RoundMaxRounds {
		rep.Warning = "maximum hardening rounds reached; safe duplicate groups remain"
	}
	// Every round's records are kept (lineage / history); the counts are
	// over distinct groups, each at its latest round (a skipped group that
	// re-appears in a rescan is one group, not two).
	var recs []hardening.GroupRecord
	latest := map[string]*persistence.HardeningGroupRecord{}
	var order []string
	for _, g := range groups {
		gr := hardening.GroupRecord{ID: g.Key, Round: g.Round, Classification: g.Classification, Reason: g.Reason, Status: g.Status,
			Members: len(g.Members), Lineage: g.Lineage}
		recs = append(recs, gr)
		rep.Groups = append(rep.Groups, gr)
		if _, ok := latest[g.Key]; !ok {
			order = append(order, g.Key)
		}
		if cur, ok := latest[g.Key]; !ok || g.Round >= cur.Round {
			latest[g.Key] = g
		}
		rep.FunctionsRemoved += g.FunctionsRemoved
		rep.ReferencesMigrated += g.RefsMigrated
		rep.TestsBefore += g.TestsBefore
		rep.TestsAfter += g.TestsAfter
	}
	for _, k := range order {
		g := latest[k]
		rep.DuplicateGroups++
		if g.Classification == hardening.ClassSafe {
			rep.SafeGroups++
		}
		if g.Classification == hardening.ClassUnsafeImportCycle {
			rep.ImportCycleFailures++
		}
		switch g.Status {
		case hardening.GroupConsolidated:
			rep.ConsolidatedGroups++
			rep.UniqueCasesPreserved += len(g.Matrix)
		case hardening.GroupSkipped:
			rep.SkippedGroups++
		case hardening.GroupRemaining:
			rep.RemainingSafeGroups++
		}
	}
	return rep, recs, nil
}

// hardeningPlan persists the ordered operations of every SAFE group of the
// round (WBS 16.110/16.111).
func (e *Engine) hardeningPlan(ctx context.Context, st *engineState, step *persistence.StepRecord, pass *persistence.HardeningPassRecord, refs persistence.StepRefs) (execution.StepResult, error) {
	groups, err := e.d.Repo.ListGroups(ctx, pass.ID, pass.Round)
	if err != nil {
		return execution.StepResult{}, err
	}
	for _, g := range groups {
		if g.Classification != hardening.ClassSafe || g.Status != hardening.GroupAnalyzed {
			continue
		}
		dg := hardening.DuplicateGroup{ID: g.Key, Fingerprint: g.Fingerprint, Members: g.Members}
		for _, m := range g.Members {
			dg.References = append(dg.References, m.References...)
		}
		if err := e.d.Repo.CreateOperations(ctx, g.ID, hardening.BuildConsolidationPlan(dg, st.hard.DuplicateFunctions.ConsolidateTests)); err != nil {
			return execution.StepResult{}, err
		}
		g.Status = hardening.GroupPlanned
		if err := e.d.Repo.UpdateGroup(ctx, g); err != nil {
			return execution.StepResult{}, err
		}
	}
	if _, err := e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.Status = hardening.PassExecuting }); err != nil {
		return execution.StepResult{}, err
	}
	return e.finishNoTransition(ctx, st, step, "EXECUTING", refs)
}

// ---------------------------------------------------------------------------
// Execution of operations (P16-122 … P16-130, P16-132).

// guardRepo detects repository changes nobody in the pass made (WBS
// 16.144): recovery instead of overwriting external work.
func (e *Engine) guardRepo(ctx context.Context, st *engineState, step *persistence.StepRecord, pass *persistence.HardeningPassRecord) (*execution.StepResult, error) {
	fp, err := e.repoFingerprint(ctx, st)
	if err != nil || pass.RepoFingerprint == "" || fp == pass.RepoFingerprint {
		return nil, err
	}
	if _, err := e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) {
		s := p.Status
		p.ResumeStatus, p.Status, p.RecoveryReason = &s, hardening.PassRecoveryRequired, "repository changed outside the hardening pass"
	}); err != nil {
		return nil, err
	}
	r, err := e.requireRecovery(ctx, st, step, "repository changed outside the hardening pass", map[string]any{"pass_id": pass.ID.String(),
		"expected_fingerprint": pass.RepoFingerprint, "observed_fingerprint": fp})
	return &r, err
}

// nextOperation returns the first group of the round with an unfinished
// operation, and that operation.
func (e *Engine) nextOperation(ctx context.Context, pass *persistence.HardeningPassRecord) (*persistence.HardeningGroupRecord, *persistence.HardeningOperationRecord, error) {
	groups, err := e.d.Repo.ListGroups(ctx, pass.ID, pass.Round)
	if err != nil {
		return nil, nil, err
	}
	for _, g := range groups {
		if g.Status != hardening.GroupPlanned && g.Status != hardening.GroupInProgress && g.Status != hardening.GroupFailed {
			continue
		}
		ops, err := e.d.Repo.ListOperations(ctx, g.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range ops {
			if o.Status != hardening.OpDone {
				return g, o, nil
			}
		}
	}
	return nil, nil, nil
}

func (e *Engine) hardeningExecute(ctx context.Context, st *engineState, step *persistence.StepRecord, pass *persistence.HardeningPassRecord, refs persistence.StepRefs) (execution.StepResult, error) {
	g, op, err := e.nextOperation(ctx, pass)
	if err != nil {
		return execution.StepResult{}, err
	}
	if g == nil {
		if _, err := e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.Status = hardening.PassValidating }); err != nil {
			return execution.StepResult{}, err
		}
		return e.finishNoTransition(ctx, st, step, "ROUND_CONSOLIDATED", refs)
	}
	refs.Detail["group"], refs.Detail["operation"], refs.Detail["op_seq"] = g.Key, string(op.Kind), op.Seq
	// An operation left RUNNING by a crash may have changed the source
	// itself: it is reconciled from the source below (and the fingerprint
	// re-baselined), not mistaken for an external modification.
	if op.Status != hardening.OpRunning {
		if stop, err := e.guardRepo(ctx, st, step, pass); stop != nil || err != nil {
			if stop == nil {
				return execution.StepResult{}, err
			}
			return *stop, err
		}
	}
	if op.Status == hardening.OpFailed {
		// Retry only after a repair since the failure (WBS 16.131).
		attempts, err := e.d.Repo.ListFixAttempts(ctx, st.rec.ID, &g.ID)
		if err != nil {
			return execution.StepResult{}, err
		}
		repaired := false
		for _, a := range attempts {
			if id, _ := a.Evidence["operation_id"].(string); id == op.ID.String() && a.DispatchID != nil && op.FinishedAt != nil && !a.CreatedAt.Before(*op.FinishedAt) {
				repaired = true
			}
		}
		if !repaired {
			ev := map[string]any{"group": g.Key, "operation": string(op.Kind), "package": op.Package, "operation_id": op.ID.String()}
			for k, v := range op.Evidence {
				ev[k] = v
			}
			return e.hardeningFixRoute(ctx, st, step, g.ID, op.ValidationRunID, ev, refs)
		}
	}
	now := e.d.Now().UTC()
	if op.Status == hardening.OpRunning {
		// Crash recovery (WBS 16.139–16.142): never assume an operation
		// finished because it started — observe the source.
		if done, err := e.resumeDone(ctx, st, g, op); err != nil {
			return execution.StepResult{}, err
		} else if done {
			op.Status, op.FinishedAt = hardening.OpDone, &now
			op.Evidence = map[string]any{"resumed": true}
			if fp, err := e.repoFingerprint(ctx, st); err == nil {
				op.RepoFingerprint = fp
				if _, err := e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.RepoFingerprint = fp }); err != nil {
					return execution.StepResult{}, err
				}
			}
			if err := e.d.Repo.UpdateOperation(ctx, op); err != nil {
				return execution.StepResult{}, err
			}
			return e.finishNoTransition(ctx, st, step, "OPERATION_RESUMED", refs)
		}
	}
	op.Status, op.StartedAt, op.FinishedAt = hardening.OpRunning, &now, nil
	if err := e.d.Repo.UpdateOperation(ctx, op); err != nil {
		return execution.StepResult{}, err
	}
	if g.Status != hardening.GroupInProgress {
		g.Status = hardening.GroupInProgress
		if err := e.d.Repo.UpdateGroup(ctx, g); err != nil {
			return execution.StepResult{}, err
		}
	}
	if err := e.hook(ctx, step.Action, "hardening:"+string(op.Kind)); err != nil {
		return execution.StepResult{}, err
	}
	if err := e.fenceNow(ctx, st); err != nil {
		return execution.StepResult{}, err
	}
	ok, evidence, runID, opErr := e.runOperation(ctx, st, step, pass, g, op)
	if opErr != nil {
		return execution.StepResult{}, opErr
	}
	if err := e.hook(ctx, step.Action, "hardening-done:"+string(op.Kind)); err != nil {
		return execution.StepResult{}, err
	}
	fin := e.d.Now().UTC()
	op.FinishedAt, op.Evidence, op.ValidationRunID = &fin, evidence, runID
	if ok {
		op.Status = hardening.OpDone
	} else {
		op.Status = hardening.OpFailed
	}
	if fp, err := e.repoFingerprint(ctx, st); err == nil {
		op.RepoFingerprint = fp
		if pass, err = e.setPass(ctx, pass, func(p *persistence.HardeningPassRecord) { p.RepoFingerprint = fp }); err != nil {
			return execution.StepResult{}, err
		}
	}
	if err := e.d.Repo.UpdateOperation(ctx, op); err != nil {
		return execution.StepResult{}, err
	}
	if !ok {
		g.Status = hardening.GroupFailed
		_ = e.d.Repo.UpdateGroup(ctx, g)
	}
	if blocked, _ := evidence["checkpoint_blocked"].(bool); blocked {
		cpErr, _ := evidence["checkpoint_error"].(string)
		reason, ev := e.checkpointFailure(ctx, st, "required hardening group checkpoint failed", cpErr, evidence)
		return e.requireRecovery(ctx, st, step, reason, ev)
	}
	return e.finishNoTransition(ctx, st, step, string(op.Kind)+map[bool]string{true: "_DONE", false: "_FAILED"}[ok], refs,
		pendingEvent{Type: execution.EventHardeningOperation, Payload: map[string]any{"group": g.Key, "operation": string(op.Kind),
			"package": op.Package, "ok": ok}})
}

// sharedKey is the destination symbol's object key.
func sharedKey(d *hardening.Destination) string { return d.ImportPath + "." + d.Symbol }

// resumeDone observes the source to decide whether an interrupted
// operation's effect is already present (WBS 16.139–16.142).
func (e *Engine) resumeDone(ctx context.Context, st *engineState, g *persistence.HardeningGroupRecord, op *persistence.HardeningOperationRecord) (bool, error) {
	switch op.Kind {
	case hardening.OpCopyToShared, hardening.OpUpdateReferences, hardening.OpConsolidateTests, hardening.OpRemoveDuplicates:
	default:
		return false, nil
	}
	m, err := e.loadModule(ctx, st)
	if err != nil {
		return false, nil
	}
	keys, _, _ := memberKeys(g.Members)
	f := hardening.Facts{RemainingRefs: map[string]int{}}
	if g.Destination != nil {
		f.SharedExists = goanalysis.Scan(m, map[string]bool{sharedKey(g.Destination): true})[sharedKey(g.Destination)].Declared
		_, statErr := os.Stat(filepath.Join(m.Dir, filepath.FromSlash(gorefactor.SharedTestFile(*g.Destination))))
		f.SharedTestsExist = statErr == nil
	}
	for _, s := range goanalysis.Scan(m, keys) {
		if s.Declared {
			f.OldSymbolsPresent = true
		}
	}
	for _, rs := range goanalysis.References(m, keys) {
		for _, r := range rs {
			f.RemainingRefs[r.ImportPath]++
		}
	}
	f.HelperTestsRemain = len(e.helperTestsPresent(m, g)) > 0
	return hardening.ResumeDone(hardening.Operation{Kind: op.Kind, Package: op.Package}, f), nil
}

// helperTests are the group's helper-only tests outside the destination.
func helperTests(g *persistence.HardeningGroupRecord) []hardening.TestInfo {
	keys, _, _ := memberKeys(g.Members)
	var out []hardening.TestInfo
	for _, t := range g.Tests {
		if g.Destination != nil && t.ImportPath == g.Destination.ImportPath {
			continue
		}
		if hardening.ClassifyTest(t, keys) == hardening.TestHelperOnly && hardening.MatrixCovers(g.Matrix, t.Cases) {
			out = append(out, t)
		}
	}
	return out
}

// helperTestsPresent returns the helper-only tests still declared.
func (e *Engine) helperTestsPresent(m *goanalysis.Module, g *persistence.HardeningGroupRecord) []hardening.TestInfo {
	var out []hardening.TestInfo
	for _, t := range helperTests(g) {
		if p := m.Package(t.ImportPath); p != nil {
			for _, f := range p.Files {
				if f.Rel != t.File {
					continue
				}
				for _, obj := range f.AST.Scope.Objects {
					if obj.Name == t.Name {
						out = append(out, t)
					}
				}
			}
		}
	}
	return out
}

func goTest(pkgDir string) wbs.ValidationSpec {
	return wbs.ValidationSpec{Name: "go test " + pkgDir, Program: "go", Args: []string{"test", "-count=1", "./" + pkgDir}}
}

// hardeningValidate runs a HARDENING-scope validation for a group.
func (e *Engine) hardeningValidate(ctx context.Context, st *engineState, g *persistence.HardeningGroupRecord, label string, cmds []wbs.ValidationSpec) (*persistence.ValidationRunRecord, error) {
	gid := g.ID
	return e.runValidation(ctx, st, nil, &persistence.ValidationRunRecord{Scope: execution.ScopeHardening, TargetID: &gid, Label: label}, cmds, nil, nil)
}

func relDir(m *goanalysis.Module, importPath string) string {
	if p := m.Package(importPath); p != nil {
		if p.RelDir == "" {
			return "."
		}
		return p.RelDir
	}
	return strings.TrimPrefix(strings.TrimPrefix(importPath, m.Path), "/")
}

// runOperation applies one operation. ok=false is a deterministic failure
// (evidence recorded, FIX follows); an error is infrastructure.
func (e *Engine) runOperation(ctx context.Context, st *engineState, step *persistence.StepRecord, pass *persistence.HardeningPassRecord,
	g *persistence.HardeningGroupRecord, op *persistence.HardeningOperationRecord) (bool, map[string]any, *uuid.UUID, error) {
	ev := map[string]any{}
	if g.Destination == nil {
		ev["error"] = "group has no destination"
		return false, ev, nil, nil
	}
	dest := *g.Destination
	keys, oldPkgs, sources := memberKeys(g.Members)
	m, err := e.loadModule(ctx, st)
	if err != nil {
		ev["error"] = "module does not load: " + err.Error()
		return false, ev, nil, nil
	}
	validated := func(label string, cmds []wbs.ValidationSpec) (bool, map[string]any, *uuid.UUID, error) {
		run, err := e.hardeningValidate(ctx, st, g, label, cmds)
		if err != nil {
			return false, ev, nil, err
		}
		ev["run_id"], ev["outcome"] = run.ID.String(), run.Status
		id := run.ID
		if run.Status == string(execution.ValidationInfraError) || run.Status == string(execution.ValidationNotConfigured) {
			return false, ev, &id, fmt.Errorf("hardening validation could not run: %s", run.Status)
		}
		return run.Status == string(execution.ValidationPass), ev, &id, nil
	}
	switch op.Kind {
	case hardening.OpCopyToShared:
		changed, err := gorefactor.CopyFunction(m, g.Members[0], dest, sources)
		if err != nil {
			ev["error"] = err.Error()
			return false, ev, nil, nil
		}
		ev["changed"] = changed
		return true, ev, nil, nil
	case hardening.OpTestShared:
		if _, err := gorefactor.WriteSharedTests(m.Dir, dest, g.Members[0].Signature, g.Matrix); err != nil {
			ev["error"] = err.Error()
			return false, ev, nil, nil
		}
		return validated("shared tests "+dest.ImportPath, []wbs.ValidationSpec{goTest(dest.Dir)})
	case hardening.OpUpdateReferences:
		n, err := gorefactor.MigrateReferences(m, op.Package, keys, oldPkgs, dest)
		if err != nil {
			ev["error"] = err.Error()
			return false, ev, nil, nil
		}
		ev["migrated"] = n
		g.RefsMigrated += n
		_ = e.d.Repo.UpdateGroup(ctx, g)
		return true, ev, nil, nil
	case hardening.OpTestCallers:
		return validated("caller tests "+op.Package, []wbs.ValidationSpec{goTest(relDir(m, op.Package))})
	case hardening.OpConsolidateTests:
		helpers := e.helperTestsPresent(m, g)
		n, err := gorefactor.RemoveTests(m, helpers)
		if err != nil {
			ev["error"] = err.Error()
			return false, ev, nil, nil
		}
		ev["removed_tests"] = n
		g.TestsAfter = len(g.Tests) - len(helperTests(g)) + 1
		_ = e.d.Repo.UpdateGroup(ctx, g)
		return true, ev, nil, nil
	case hardening.OpRemoveDuplicates:
		scan := goanalysis.Scan(m, keys)
		old := 0
		for _, s := range scan {
			old += s.ProductionRefs + s.TestRefs
		}
		ops, _ := e.d.Repo.ListOperations(ctx, g.ID)
		gs := hardening.GroupState{SharedExists: goanalysis.Scan(m, map[string]bool{sharedKey(&dest): true})[sharedKey(&dest)].Declared,
			Migrated: map[string]bool{}, CallerTestsPass: map[string]bool{}, OldReferences: old}
		var callers []string
		for _, o := range ops {
			switch o.Kind {
			case hardening.OpTestShared:
				gs.SharedTestsPass = o.Status == hardening.OpDone
			case hardening.OpUpdateReferences:
				gs.Migrated[o.Package] = o.Status == hardening.OpDone
				callers = append(callers, o.Package)
			case hardening.OpTestCallers:
				gs.CallerTestsPass[o.Package] = o.Status == hardening.OpDone
			}
		}
		if ok, why := hardening.CanRemoveDuplicates(gs, callers); !ok {
			ev["error"] = "removal gate: " + why
			return false, ev, nil, nil
		}
		removed := 0
		for _, mem := range g.Members {
			did, err := gorefactor.RemoveFunction(m, mem)
			if err != nil {
				ev["error"] = err.Error()
				return false, ev, nil, nil
			}
			if did {
				removed++
			}
			if m, err = e.loadModule(ctx, st); err != nil {
				ev["error"] = "module does not load after removal: " + err.Error()
				return false, ev, nil, nil
			}
		}
		for k, s := range goanalysis.Scan(m, keys) {
			if s.Declared {
				ev["error"] = "post-removal scan: " + k + " still declared"
				return false, ev, nil, nil
			}
		}
		g.FunctionsRemoved += removed
		_ = e.d.Repo.UpdateGroup(ctx, g)
		ev["removed"] = removed
		return true, ev, nil, nil
	case hardening.OpValidate:
		cmds := []wbs.ValidationSpec{{Name: "go build", Program: "go", Args: []string{"build", "./..."}},
			{Name: "go vet", Program: "go", Args: []string{"vet", "./..."}}, goTest(dest.Dir)}
		pk := map[string]bool{}
		for _, mem := range g.Members {
			pk[mem.ImportPath] = true
			for _, r := range mem.References {
				pk[r.ImportPath] = true
			}
		}
		var names []string
		for p := range pk {
			names = append(names, p)
		}
		sort.Strings(names)
		for _, p := range names {
			cmds = append(cmds, goTest(relDir(m, p)))
		}
		if st.hard.TransitiveTests {
			cmds = append(cmds, wbs.ValidationSpec{Name: "go test ./...", Program: "go", Args: []string{"test", "-count=1", "./..."}})
		}
		return validated("consolidation validation "+g.Key, cmds)
	case hardening.OpGroupCheckpoint:
		policy := execution.CheckpointPolicy(st.hard.GroupCheckpointPolicy)
		cp, err := e.checkpoint(ctx, st, step, execution.RepoCheckpointHardeningGroup, g.ID, nil, policy, "consolidate "+g.Key+" into "+dest.ImportPath+"."+dest.Symbol)
		if err != nil {
			return false, ev, nil, err
		}
		ev["checkpoint_id"], ev["checkpoint_status"] = cp.ID.String(), cp.Status
		if cp.Status == persistence.CheckpointFailed && execution.OnCheckpointFailure(policy) == execution.CheckpointBlock {
			ev["checkpoint_blocked"], ev["checkpoint_error"] = true, cp.Error
			return true, ev, nil, nil
		}
		ops, _ := e.d.Repo.ListOperations(ctx, g.ID)
		valOK := false
		for _, o := range ops {
			if o.Kind == hardening.OpValidate {
				valOK = o.Status == hardening.OpDone
			}
		}
		if ok, why := hardening.CanCompleteGroup(hardening.GroupState{ValidationPass: valOK, CheckpointPolicy: string(policy),
			CheckpointDone: cp.Status == persistence.CheckpointCreated}); !ok {
			ev["error"] = why
			return false, ev, nil, nil
		}
		cpID := cp.ID
		g.Status, g.CheckpointID = hardening.GroupConsolidated, &cpID
		g.Lineage = &hardening.Lineage{Sources: sources, Destination: sharedKey(&dest)}
		if err := e.d.Repo.UpdateGroup(ctx, g); err != nil {
			return false, ev, nil, err
		}
		_ = e.event(ctx, nil, st, execution.EventHardeningGroupDone, map[string]any{"group": g.Key, "destination": sharedKey(&dest),
			"sources": sources, "checkpoint_id": cpID.String()})
		return true, ev, nil, nil
	}
	ev["error"] = "unknown operation"
	return false, ev, nil, nil
}
