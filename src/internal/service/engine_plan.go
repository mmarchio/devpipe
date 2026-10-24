package service

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// Planning (P16-94 … P16-98, P16-113).

// handlePlanDispatch dispatches the planning prompt of the working phase
// through the Phase 8–11 pipeline and the generic worker (WBS 16.13/16.14).
func (e *Engine) handlePlanDispatch(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	work := st.workPhase()
	if work == nil {
		return e.requireRecovery(ctx, st, step, "no phase to plan", nil)
	}
	if st.wbsVer <= 0 {
		return e.requireRecovery(ctx, st, step, "the phase has no WBS to plan from", map[string]any{"phase_id": work.ID.String()})
	}
	if e.d.GC != nil {
		// Collect expired artifacts before planning (decision U1); never
		// blocks planning.
		if err := e.d.GC(ctx); err != nil {
			e.d.Logger.ErrorContext(ctx, "garbage collection before planning failed", "error", err.Error())
		}
	}
	pid := work.ID
	if st.reusablePlan() {
		// A valid current plan is never regenerated (WBS 16.11).
		return e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), advanceOpts{SetPhase: true, PhaseID: &pid,
			Refs: persistence.StepRefs{PlanID: &st.plan.ID, Detail: map[string]any{"reuse_plan": true}}})
	}
	out, stop, err := e.dispatchStep(ctx, st, step, DispatchRequest{PhaseID: &pid})
	if err != nil || stop != nil {
		if stop == nil {
			return execution.StepResult{}, err
		}
		return *stop, err
	}
	id := out.dispatch.ID
	return e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), advanceOpts{SetPhase: true, PhaseID: &pid, SetUnit: true,
		Refs: persistence.StepRefs{DispatchID: &id}})
}

// planMode decides whether the phase is being planned from scratch or
// amended after a failed phase validation (WBS 16.65/16.66): amendment
// when the current plan is current, fully DONE, and the phase's latest
// phase validation failed.
func (e *Engine) planMode(ctx context.Context, st *engineState) string {
	work := st.workPhase()
	if work == nil || st.plan == nil || st.plan.WBSVersion != st.wbsVer || !wbs.PhaseCompletionEligible(st.plan, st.units) {
		return wbs.PlanKindInitial
	}
	run, err := e.d.Repo.LatestValidationRun(ctx, nil, st.rec.ID, execution.ScopePhase, work.ID)
	if err == nil && run.Status == string(execution.ValidationFail) {
		return wbs.PlanKindAmendment
	}
	return wbs.PlanKindInitial
}

// candidateFor turns the planning dispatch into a persisted plan candidate
// (WBS 16.15). Statefile workflow: the worker's reply is not parsed (only
// its arrival counts); the plan is the one submitted through the MCP
// server (submit_result). Composed workflow: the plan travels as the
// result's `plan` object. A failed dispatch, a protocol failure or a
// missing / malformed plan is a REJECTED candidate, never a plan.
func (e *Engine) candidateFor(ctx context.Context, st *engineState, dispatchID uuid.UUID, mode string) (*persistence.PlanCandidateRecord, *wbs.PlanCandidate, error) {
	if c, err := e.d.Repo.GetCandidateByDispatch(ctx, nil, dispatchID); err == nil {
		pc, _ := wbs.DecodePlanCandidate(c.Payload)
		return c, pc, nil
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, nil, err
	}
	work := st.workPhase()
	rec := &persistence.PlanCandidateRecord{ProjectID: st.rec.ProjectID, ExecutionID: st.rec.ID, PhaseID: work.ID, DispatchID: &dispatchID,
		WBSVersion: st.wbsVer, WBSHash: st.wbsHash, Mode: mode, Status: persistence.CandidatePending}
	reject := func(code, msg string) {
		rec.Status = persistence.CandidateRejected
		rec.Report = wbs.PlanProblems{{Code: code, Message: msg}}
	}
	var pc *wbs.PlanCandidate
	if st.statefileWorkflow() {
		var err error
		if pc, err = e.planCandidate(ctx, st, dispatchID, mode, rec, reject); err != nil {
			return nil, nil, err
		}
	} else {
		ev, err := e.evaluateDispatch(ctx, st, dispatchID)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case ev.token == execution.TokenProtocolError:
			reject("PROTOCOL_ERROR", "planner response violates the result protocol")
		case ev.result == nil || ev.token != execution.TokenComplete:
			reject("NOT_COMPLETE", "planner did not report a complete plan ("+string(ev.token)+")")
		case ev.result.Result.Plan == nil:
			reject("MISSING_PLAN", "planner result has no plan object")
		default:
			rec.Payload = ev.result.Result.Plan
			if pc, err = wbs.DecodePlanCandidate(ev.result.Result.Plan); err != nil {
				reject("MALFORMED_PLAN", err.Error())
				pc = nil
			}
		}
	}
	got, err := e.d.Repo.CreateCandidate(ctx, nil, rec)
	return got, pc, err
}

// otherPhaseKeys returns the unit keys of every plan of the project's
// phases other than the working phase.
func (e *Engine) otherPhaseKeys(ctx context.Context, st *engineState) (map[string]bool, error) {
	work := st.workPhase()
	keys := map[string]bool{}
	for _, p := range st.phases {
		if p.ID == work.ID {
			continue
		}
		plans, err := e.d.Repo.ListPhasePlans(ctx, nil, p.ID)
		if err != nil {
			return nil, err
		}
		for _, pl := range plans {
			units, err := e.d.Repo.ListPlanUnits(ctx, nil, pl.ID)
			if err != nil {
				return nil, err
			}
			for _, u := range units {
				keys[wbs.UnitKey(u)] = true
			}
		}
	}
	return keys, nil
}

// withoutOtherPhases drops the tasks whose keys belong to other phases'
// plans, and the dependencies on them (satisfied in their own phase).
func withoutOtherPhases(pc *wbs.PlanCandidate, other map[string]bool) *wbs.PlanCandidate {
	if len(other) == 0 {
		return pc
	}
	out := &wbs.PlanCandidate{Summary: pc.Summary}
	for _, u := range pc.Units {
		if other[u.Key] {
			continue
		}
		var deps []string
		for _, d := range u.DependsOn {
			if !other[d] {
				deps = append(deps, d)
			}
		}
		u.DependsOn = deps
		out.Units = append(out.Units, u)
	}
	return out
}

// withoutCarried drops the tasks whose keys belong to the current plan.
func withoutCarried(pc *wbs.PlanCandidate, current []*wbs.WorkUnit) *wbs.PlanCandidate {
	carried := map[string]bool{}
	for _, u := range current {
		carried[wbs.UnitKey(u)] = true
	}
	out := &wbs.PlanCandidate{Summary: pc.Summary}
	for _, u := range pc.Units {
		if !carried[u.Key] {
			out.Units = append(out.Units, u)
		}
	}
	return out
}

// validateCandidate runs every deterministic plan check (WBS 16.16/16.17);
// carried are the keys an amendment may depend on.
func validateCandidate(pc *wbs.PlanCandidate, pol wbs.AtomicPolicy, carried []string) wbs.PlanProblems {
	// Every group is reported, so a rejected planner can fix all of them
	// in one attempt.
	p := wbs.ValidateAmendmentStructure(pc, carried)
	p = append(p, wbs.ValidateAtomicPolicy(pc, pol)...)
	p = append(p, wbs.ValidatePlanScope(pc)...)
	return append(p, wbs.ValidatePrograms(pc, programFound)...)
}

// programFound reports whether the overseer's validation runner can find
// a program (the same PATH lookup it does before running a command).
func programFound(program string) bool {
	_, err := exec.LookPath(program)
	return err == nil
}

// handlePlanValidate validates the planner's candidate and activates it
// transactionally (WBS 16.16–16.19) or rejects it back to planning, bounded
// by MaxPlanAttempts. In amendment mode the new plan version carries the
// DONE units and adds the repair units (WBS 16.66–16.68).
func (e *Engine) handlePlanValidate(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	work := st.workPhase()
	pend, err := e.pendingDispatch(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	if pend == nil && work != nil && st.reusablePlan() {
		return e.advance(ctx, st, step, "ACCEPTED", advanceOpts{SetUnit: true, Refs: persistence.StepRefs{PlanID: &st.plan.ID,
			Detail: map[string]any{"reuse_plan": true}}})
	}
	if pend == nil || work == nil {
		return e.requireRecovery(ctx, st, step, "no planning dispatch to validate", nil)
	}
	mode := e.planMode(ctx, st)
	cand, pc, err := e.candidateFor(ctx, st, *pend, mode)
	if err != nil {
		return execution.StepResult{}, err
	}
	problems := cand.Report
	var units []wbs.PlannedUnit
	if cand.Status == persistence.CandidatePending {
		var carried []string
		if mode == wbs.PlanKindAmendment {
			for _, u := range st.units {
				carried = append(carried, wbs.UnitKey(u))
			}
		}
		problems = validateCandidate(pc, st.engine.AtomicPolicy, carried)
		if len(problems) == 0 && mode == wbs.PlanKindAmendment {
			deps, err := e.d.Repo.UnitDependencies(ctx, nil, st.plan.ID)
			if err != nil {
				return execution.StepResult{}, err
			}
			units, problems = wbs.BuildAmendment(st.units, deps, pc)
		} else if len(problems) == 0 {
			units = wbs.PlannedUnitsFromCandidate(pc)
		}
	}
	refs := persistence.StepRefs{DispatchID: pend, Detail: map[string]any{"candidate_id": cand.ID.String(), "mode": mode}}
	if cand.Status == persistence.CandidateAccepted {
		// Already activated (crash after the activation commit).
		return e.advance(ctx, st, step, "ACCEPTED", advanceOpts{Refs: refs, SetUnit: true})
	}
	if len(problems) > 0 || cand.Status == persistence.CandidateRejected {
		return e.rejectCandidate(ctx, st, step, cand, problems, refs)
	}
	// Activation (WBS 16.18/16.19): one transaction, the WBS re-read.
	var plan *wbs.ExecutionPlan
	planID := uuid.New()
	refs.PlanID = &planID
	stale := false
	activate := func(tx persistence.Transaction) error {
		cur, err := e.d.WBSVersions.GetCurrentVersion(ctx, work.ID.String())
		if err != nil {
			return err
		}
		if cur != cand.WBSVersion {
			stale = true
			return errStaleCandidate
		}
		next, err := e.d.Plans.WithTx(tx).NextVersion(ctx, work.ID)
		if err != nil {
			return err
		}
		plan = &wbs.ExecutionPlan{ID: planID, PhaseID: work.ID, WBSVersion: cand.WBSVersion, Version: next, Status: wbs.PlanStatusEXECUTING,
			Kind: mode, SourceWBSHash: cand.WBSHash, CreatedAt: e.d.Now().UTC()}
		if mode == wbs.PlanKindAmendment {
			old := st.plan.ID
			plan.AmendsPlanID = &old
		}
		if _, err := e.d.Repo.CreatePlanWithUnits(ctx, tx, plan, units); err != nil {
			return err
		}
		if err := e.d.Repo.SupersedePlans(ctx, tx, work.ID, plan.ID); err != nil {
			return err
		}
		pid := plan.ID
		if err := e.d.Repo.DecideCandidate(ctx, tx, cand.ID, persistence.CandidateAccepted, nil, &pid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE phases SET status = 'EXECUTING', started_at = COALESCE(started_at, now()), updated_at = now()
			WHERE id = $1 AND status IN ('PENDING', 'PLANNING', 'READY', 'VALIDATING')`, work.ID); err != nil {
			return err
		}
		return nil
	}
	evType := execution.EventPlanActivated
	if mode == wbs.PlanKindAmendment {
		evType = execution.EventPlanAmended
	}
	payload := map[string]any{"candidate_id": cand.ID.String(), "units": len(units), "wbs_version": cand.WBSVersion, "mode": mode}
	res, err := e.advance(ctx, st, step, "ACCEPTED", advanceOpts{Refs: refs, SetUnit: true, Tx: func(tx persistence.Transaction) error {
		if err := activate(tx); err != nil {
			return err
		}
		payload["plan_id"], payload["plan_version"] = plan.ID.String(), plan.Version
		return nil
	}, Events: []pendingEvent{{Type: evType, Payload: payload}}})
	if stale {
		refs.PlanID = nil
		return e.rejectCandidate(ctx, st, step, cand, wbs.PlanProblems{{Code: "STALE_WBS", Message: "the WBS changed while the plan was generated"}}, refs)
	}
	return res, err
}

var errStaleCandidate = errors.New("service: plan candidate is stale")

// recoveryPlanExhausted is the recovery reason when planning attempts run
// out; resolving it resets the planning attempt count to 0.
const recoveryPlanExhausted = "planning attempts exhausted"

// rejectCandidate records the rejection and routes back to planning, or to
// recovery once MaxPlanAttempts rejections accumulated since the phase's
// last activated plan or the last resolved planning-exhausted recovery
// (which resets the attempts to 0).
func (e *Engine) rejectCandidate(ctx context.Context, st *engineState, step *persistence.StepRecord, cand *persistence.PlanCandidateRecord,
	problems wbs.PlanProblems, refs persistence.StepRefs) (execution.StepResult, error) {
	if cand.Status == persistence.CandidatePending {
		if err := e.d.Repo.DecideCandidate(ctx, nil, cand.ID, persistence.CandidateRejected, problems, nil); err != nil && !errors.Is(err, persistence.ErrConflict) {
			return execution.StepResult{}, err
		}
	}
	var since *time.Time
	for _, p := range st.plans {
		if since == nil || p.CreatedAt.After(*since) {
			t := p.CreatedAt
			since = &t
		}
	}
	reset, err := e.d.Repo.LastRecoveryResolved(ctx, nil, st.rec.ID, recoveryPlanExhausted)
	if err != nil {
		return execution.StepResult{}, err
	}
	if reset != nil && (since == nil || reset.After(*since)) {
		since = reset
	}
	n, err := e.d.Repo.CountRejectedCandidates(ctx, nil, st.rec.ID, cand.PhaseID, since)
	if err != nil {
		return execution.StepResult{}, err
	}
	payload := map[string]any{"candidate_id": cand.ID.String(), "problems": problems, "rejections": n}
	if execution.AttemptDecision(n, st.engine.MaxPlanAttempts) == execution.AttemptExhausted {
		if err := e.event(ctx, nil, st, execution.EventPlanRejected, payload); err != nil {
			return execution.StepResult{}, err
		}
		_ = e.d.Repo.SetStepRefs(ctx, nil, step.ID, refs)
		return e.requireRecovery(ctx, st, step, recoveryPlanExhausted, payload)
	}
	return e.advance(ctx, st, step, "REJECTED", advanceOpts{Refs: refs, Outcome: "REJECTED",
		Events: []pendingEvent{{Type: execution.EventPlanRejected, Payload: payload}}})
}
