package service

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// Phases (P16-112 / P16-114).

// handlePhaseValidation runs phase-level deterministic validation once
// every current-plan unit is DONE (WBS 16.62–16.65): 100 % progress alone
// never completes a phase. PASS → phase DONE (+ PHASE_DONE event, plan
// COMPLETED); FAIL → the repair route (amendment planning), the phase
// stays non-DONE.
func (e *Engine) handlePhaseValidation(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	work := st.workPhase()
	if work == nil {
		return e.requireRecovery(ctx, st, step, "no phase to validate", nil)
	}
	if !wbs.PhaseCompletionEligible(st.plan, st.units) {
		return e.requireRecovery(ctx, st, step, "phase validation reached with non-DONE current-plan units", e.unitEvidence(st))
	}
	pid := work.ID
	run, err := e.runValidation(ctx, st, step, &persistence.ValidationRunRecord{Scope: execution.ScopePhase, PhaseID: &pid, Label: work.Name},
		execution.ResolveValidationCommands(nil, st.engine, execution.ScopePhase), nil, nil)
	if err != nil {
		return execution.StepResult{}, err
	}
	refs := persistence.StepRefs{ValidationRunID: &run.ID}
	ev := map[string]any{"phase_id": pid.String(), "run_id": run.ID.String(), "outcome": run.Status}
	switch run.Status {
	case string(execution.ValidationPass):
		if err := e.durableInvariants(ctx, st, "before", "phase advancement"); err != nil {
			return execution.StepResult{}, err
		}
		planID := st.plan.ID
		return e.advance(ctx, st, step, "COMPLETE", advanceOpts{Refs: refs, SetUnit: true,
			Tx: func(tx persistence.Transaction) error {
				// Fault point: inside the phase-advancement transaction.
				if err := faults.Hit(ctx, faults.PhaseAdvance); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE phases SET status = 'DONE', completed_at = COALESCE(completed_at, now()), updated_at = now()
					WHERE id = $1 AND status <> 'DONE'`, pid); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `UPDATE execution_plans SET status = 'COMPLETED', updated_at = now() WHERE id = $1`, planID)
				return err
			},
			Events: []pendingEvent{{Type: execution.EventPhaseDone, Payload: ev}}})
	case string(execution.ValidationFail):
		return e.advance(ctx, st, step, "FAIL", advanceOpts{Refs: refs, Outcome: "FAIL",
			Events: []pendingEvent{{Type: execution.EventPhaseValidationFailed, Payload: ev}}})
	case string(execution.ValidationNotConfigured):
		return e.requireRecovery(ctx, st, step, "no phase validation is configured", ev)
	}
	return e.requireRecovery(ctx, st, step, "phase validation could not run (infrastructure)", ev)
}

// handleMorePhases picks the smallest-order non-DONE phase (WBS 16.69 —
// ordering gaps don't matter, appended phases are found naturally, 16.70);
// none → final hardening, never PROJECT_DONE directly (16.10/16.86).
func (e *Engine) handleMorePhases(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	next := wbs.CurrentPhase(st.phases)
	if next != nil {
		id := next.ID
		return e.advance(ctx, st, step, "YES", advanceOpts{SetPhase: true, PhaseID: &id, SetUnit: true,
			Refs: persistence.StepRefs{Detail: map[string]any{"next_phase": id.String(), "phase_order": next.Order}}})
	}
	return e.advance(ctx, st, step, "NO", advanceOpts{SetUnit: true})
}

// reusablePlan reports a current plan of the working phase that must not
// be regenerated (WBS 16.11): its WBS version is current, it is usable, and
// work remains.
func (st *engineState) reusablePlan() bool {
	if st.plan == nil || st.plan.WBSVersion != st.wbsVer || st.plan.Status == wbs.PlanStatusGENERATING || st.plan.Status == wbs.PlanStatusFAILED {
		return false
	}
	return !wbs.PhaseCompletionEligible(st.plan, st.units)
}

// ---------------------------------------------------------------------------
// Finalization (P16-134 / P16-135).

// finalEligible checks WBS 16.150: every user phase DONE, hardening DONE
// or disabled, no unresolved recovery, a valid repository.
func (e *Engine) finalEligible(ctx context.Context, st *engineState) (bool, map[string]any) {
	why := map[string]any{}
	for _, p := range st.phases {
		if p.Status != phase.StatusDONE {
			why["phase_not_done"] = p.ID.String()
		}
	}
	if st.hard.Enabled && st.hard.DuplicateFunctions.Enabled {
		pass, err := e.d.Repo.GetPass(ctx, nil, st.rec.ID, st.rec.FinalizationGeneration)
		if err != nil || pass.Status != "DONE" {
			why["hardening"] = "not DONE"
		}
	}
	if st.rec.RecoveryReason != "" {
		why["recovery"] = st.rec.RecoveryReason
	}
	if e.d.Inspector != nil {
		if snap, err := e.d.Inspector.Inspect(ctx, st.cwd()); err != nil || (st.content.Execution.RequireGitRepository && !snap.IsGitRepository) {
			why["repository"] = "invalid"
		}
	}
	return len(why) == 0, why
}

// handleFinalValidation runs the complete configured project validation
// against the hardened repository (WBS 16.150–16.153). FAIL never
// completes the project: it routes back into final hardening's fix path.
func (e *Engine) handleFinalValidation(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if ok, why := e.finalEligible(ctx, st); !ok {
		return e.requireRecovery(ctx, st, step, "final validation is not eligible yet", why)
	}
	target := st.rec.ID
	run, err := e.runValidation(ctx, st, step, &persistence.ValidationRunRecord{Scope: execution.ScopeFinal, TargetID: &target, Label: "final project validation"},
		execution.ResolveValidationCommands(nil, st.engine, execution.ScopeFinal), nil, nil)
	if err != nil {
		return execution.StepResult{}, err
	}
	refs := persistence.StepRefs{ValidationRunID: &run.ID}
	ev := map[string]any{"run_id": run.ID.String(), "outcome": run.Status}
	switch run.Status {
	case string(execution.ValidationPass):
		return e.advance(ctx, st, step, "PASS", advanceOpts{Refs: refs, Events: []pendingEvent{{Type: execution.EventFinalValidationPassed, Payload: ev}}})
	case string(execution.ValidationFail):
		return e.advance(ctx, st, step, "FAIL", advanceOpts{Refs: refs, Outcome: "FAIL", Events: []pendingEvent{{Type: execution.EventFinalValidationFailed, Payload: ev}}})
	}
	return e.requireRecovery(ctx, st, step, "final validation could not run", ev)
}

// handleFinalCheckpoint creates the final checkpoint of the validated,
// hardened repository and completes the project (WBS 16.154–16.158): one
// transaction sets the execution and project COMPLETED, writes the
// immutable completion record and the PROJECT_COMPLETED event — once.
func (e *Engine) handleFinalCheckpoint(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	target := st.rec.ID
	run, err := e.d.Repo.LatestValidationRun(ctx, nil, st.rec.ID, execution.ScopeFinal, target)
	if err != nil || run.Status != string(execution.ValidationPass) {
		return e.requireRecovery(ctx, st, step, "final checkpoint requires a passing final validation", nil)
	}
	cp, err := e.checkpoint(ctx, st, step, execution.RepoCheckpointFinal, target, &run.ID, st.engine.CheckpointPolicy, "final checkpoint")
	if err != nil {
		return execution.StepResult{}, err
	}
	if cp.Status == persistence.CheckpointFailed && execution.OnCheckpointFailure(st.engine.CheckpointPolicy) == execution.CheckpointBlock {
		reason, ev := e.checkpointFailure(ctx, st, "required final checkpoint failed", cp.Error, map[string]any{"checkpoint_id": cp.ID.String(), "error": cp.Error})
		return e.requireRecovery(ctx, st, step, reason, ev)
	}
	var finalPhase *uuid.UUID
	var best *phase.Phase
	for _, p := range st.phases {
		if best == nil || p.Order > best.Order {
			best = p
		}
	}
	if best != nil {
		id := best.ID
		finalPhase = &id
	}
	var passID *uuid.UUID
	if pass, err := e.d.Repo.GetPass(ctx, nil, st.rec.ID, st.rec.FinalizationGeneration); err == nil {
		id := pass.ID
		passID = &id
	}
	runID, cpID := run.ID, cp.ID
	now := e.d.Now().UTC()
	var notify func()
	payload := map[string]any{"execution_id": st.rec.ID.String(), "final_validation_run_id": runID.String(), "final_checkpoint_id": cpID.String(),
		"completed_at": now.Format("2006-01-02T15:04:05.000000Z07:00")}
	if finalPhase != nil {
		payload["final_phase_id"] = finalPhase.String()
	}
	if passID != nil {
		payload["hardening_pass_id"] = passID.String()
	}
	res, err := e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), advanceOpts{Status: execution.ExecutionCompleted,
		Refs: persistence.StepRefs{ValidationRunID: &runID, CheckpointID: &cpID},
		Tx: func(tx persistence.Transaction) error {
			// Fault point: inside the completion transaction.
			if err := faults.Hit(ctx, faults.ProjectCompletion); err != nil {
				return err
			}
			_, n, err := e.d.Lifecycle.TransitionTx(ctx, tx, st.rec.ProjectID, "execution completed", project.StatusCOMPLETED,
				func(p *project.Project) (bool, error) { return p.Status != project.StatusCOMPLETED, nil })
			if err != nil {
				return err
			}
			notify = n
			_, _, err = e.d.Repo.CreateCompletion(ctx, tx, &persistence.CompletionRecord{ExecutionID: st.rec.ID, ProjectID: st.rec.ProjectID,
				FinalPhaseID: finalPhase, HardeningPassID: passID, FinalValidationRunID: &runID, FinalCheckpointID: &cpID, CompletedAt: now})
			return err
		},
		Events: []pendingEvent{{Type: execution.EventProjectCompleted, Payload: payload}}})
	if err == nil && notify != nil {
		notify()
	}
	return res, err
}
