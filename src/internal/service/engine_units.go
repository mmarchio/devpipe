package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/gitcheckpoint"
	"overseer/internal/persistence"
	"overseer/internal/recovery"
	"overseer/internal/resilience"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// Work-unit selection (P16-99 / P16-100).

// nextUnit returns the unit to work on: this execution's in-progress unit
// first (resume), else the lowest-sequence dependency-ready unit of the
// current plan (WBS 16.20/16.21). deadlock = non-DONE units remain but none
// is executable (WBS 16.61).
func (e *Engine) nextUnit(ctx context.Context, st *engineState) (u *wbs.WorkUnit, allDone, deadlock bool, err error) {
	if st.plan == nil {
		return nil, false, false, nil
	}
	allDone = len(st.units) > 0
	for _, x := range st.units {
		if x.Status != wbs.WorkUnitStatusDONE {
			allDone = false
		}
		switch x.Status {
		case wbs.WorkUnitStatusEXECUTING, wbs.WorkUnitStatusINCOMPLETE, wbs.WorkUnitStatusVALIDATING:
			if x.ClaimedExecutionID != nil && *x.ClaimedExecutionID == st.rec.ID && u == nil {
				u = x
			}
		}
	}
	if u != nil || allDone {
		return u, allDone, false, nil
	}
	el, err := e.d.Units.ListEligible(ctx, st.plan.ID)
	if err != nil {
		return nil, false, false, err
	}
	for _, x := range el {
		if x.Status == wbs.WorkUnitStatusPENDING || x.Status == wbs.WorkUnitStatusREADY {
			if u == nil || x.Sequence < u.Sequence {
				u = x
			}
		}
	}
	return u, false, u == nil && len(st.units) > 0, nil
}

// handleSelectUnit claims the next unit atomically (WBS 16.22), syncs
// ACTIVE_TASK to exactly that unit (WBS 16.23) and moves to its
// implementation. A blocked dependency prevents selection (16.20); a
// dependency deadlock never looks like phase completion (16.61).
func (e *Engine) handleSelectUnit(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	u, allDone, deadlock, err := e.nextUnit(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	switch {
	case st.plan == nil:
		return e.requireRecovery(ctx, st, step, "no current plan to select a work unit from", nil)
	case deadlock:
		return e.requireRecovery(ctx, st, step, "dependency deadlock: pending units remain but none is executable", e.unitEvidence(st))
	case allDone:
		return e.advance(ctx, st, step, execution.OutcomeWorkComplete, advanceOpts{})
	}
	if _, err := e.d.ActiveTask.DeriveActiveTaskForUnit(ctx, st.rec.ProjectID, u); err != nil {
		return execution.StepResult{}, fmt.Errorf("sync ACTIVE_TASK: %w", err)
	}
	uid := u.ID
	return e.advance(ctx, st, step, execution.OutcomeReady, advanceOpts{SetUnit: true, UnitID: &uid,
		Tx: func(tx persistence.Transaction) error {
			if err := e.d.Repo.ClaimUnit(ctx, tx, uid, st.rec.ID); err != nil {
				return err
			}
			return nil
		},
		Events: []pendingEvent{{Type: execution.EventUnitClaimed, Payload: map[string]any{"work_unit_id": uid.String(), "sequence": u.Sequence, "name": u.Name}}}})
}

func (e *Engine) unitEvidence(st *engineState) map[string]any {
	var units []string
	for _, u := range st.units {
		units = append(units, fmt.Sprintf("%d:%s:%s", u.Sequence, u.Name, u.Status))
	}
	ev := map[string]any{"units": units}
	if st.plan != nil {
		ev["plan_id"] = st.plan.ID.String()
	}
	return ev
}

// handleMoreWork (WBS 16.60–16.62): YES while an executable unit of the
// current plan remains, NO only when every current-plan unit is DONE
// (superseded plans never count), recovery on a deadlock.
func (e *Engine) handleMoreWork(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	u, allDone, deadlock, err := e.nextUnit(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	switch {
	case allDone:
		return e.advance(ctx, st, step, "NO", advanceOpts{SetUnit: true})
	case u != nil:
		return e.advance(ctx, st, step, "YES", advanceOpts{})
	case deadlock:
		return e.requireRecovery(ctx, st, step, "dependency deadlock: pending units remain but none is executable", e.unitEvidence(st))
	}
	return e.requireRecovery(ctx, st, step, "no current plan", nil)
}

// ---------------------------------------------------------------------------
// Deterministic validation (P16-104 … P16-107).

// runValidation executes commands under overseer control and persists the
// run (WBS 16.44). The RUNNING row is committed first; a step resumed with
// a finished run reuses it; an interrupted run is abandoned and rerun —
// PASS is never inferred (WBS 16.79).
func (e *Engine) runValidation(ctx context.Context, st *engineState, step *persistence.StepRecord, run *persistence.ValidationRunRecord,
	cmds []wbs.ValidationSpec, checks []wbs.AcceptanceCheckSpec, approvals map[int]bool) (*persistence.ValidationRunRecord, error) {
	if step != nil && step.ValidationRunID != nil {
		prev, err := e.d.Repo.GetValidationRun(ctx, *step.ValidationRunID)
		if err != nil {
			return nil, err
		}
		switch prev.Status {
		case persistence.RunAbandoned:
			// Interrupted and abandoned by restart recovery: rerun.
		case persistence.RunRunning:
			if err := e.d.Repo.FinishValidationRun(ctx, nil, prev.ID, persistence.RunAbandoned, nil, nil); err != nil && !errors.Is(err, persistence.ErrConflict) {
				return nil, err
			}
		default:
			return prev, nil
		}
	}
	if reason := e.d.Gate.BlockedSideEffects(); reason != "" {
		return nil, &gateWaitError{reason: "validation held: " + reason}
	}
	if err := e.guardRepository(ctx, st, "validation"); err != nil {
		return nil, err
	}
	if err := e.durableInvariants(ctx, st, "before", "validation"); err != nil {
		return nil, err
	}
	target := run.WorkUnitID
	if target == nil {
		target = run.PhaseID
	}
	if target == nil {
		target = run.TargetID
	}
	if target != nil {
		n, err := e.d.Repo.CountValidationRuns(ctx, st.rec.ID, run.Scope, *target)
		if err != nil {
			return nil, err
		}
		run.Attempt = n + 1
	}
	run.ProjectID, run.ExecutionID, run.CWD = st.rec.ProjectID, st.rec.ID, st.cwd()
	if len(cmds) > 0 && !e.validationCapacity() {
		// The global validation limit is reached (WBS 18.93): the
		// execution waits; nothing has been started or recorded.
		return nil, ErrValidationCapacity
	}
	err := e.inTx(ctx, st, func(tx persistence.Transaction) error {
		if err := e.fence(ctx, tx, st); err != nil {
			return err
		}
		if err := e.d.Repo.StartValidationRun(ctx, tx, run); err != nil {
			return err
		}
		if step != nil {
			if err := e.d.Repo.SetStepRefs(ctx, tx, step.ID, persistence.StepRefs{ValidationRunID: &run.ID}); err != nil {
				return err
			}
		}
		return e.event(ctx, tx, st, execution.EventValidationStarted, map[string]any{"run_id": run.ID.String(), "scope": string(run.Scope),
			"commands": len(cmds), "label": run.Label})
	})
	if err != nil {
		return nil, translate(err)
	}
	if step != nil {
		step.ValidationRunID = &run.ID
		if err := e.hook(ctx, step.Action, "validation"); err != nil {
			return nil, err
		}
		if err := e.fenceNow(ctx, st); err != nil {
			return nil, err
		}
	}
	// Fault point (WBS 18.141): the run is durably RUNNING, nothing ran.
	if err := faults.Hit(ctx, faults.BeforeValidation); err != nil {
		return nil, err
	}
	var records []execution.ValidationRecord
	if len(cmds) > 0 {
		release, serr := e.validationSlot(ctx)
		if serr != nil {
			_ = e.d.Repo.FinishValidationRun(context.WithoutCancel(ctx), nil, run.ID, persistence.RunAbandoned, nil, nil)
			return nil, serr
		}
		attempts := 0
		// An infrastructure failure (the runner itself could not run the
		// commands) is retried under the bounded validation_infra policy —
		// never routed to FIX (WBS 18.67). A full disk or a cancellation
		// is not retried.
		_, _ = resilience.MustDefault().Do(ctx, resilience.PolicyValidationInfra, func(ctx context.Context, _ int) error {
			attempts++
			records = e.d.Runner.RunAll(ctx, cmds, st.cwd(), st.engine.ValidationTimeout, st.engine.OutputCapBytes)
			if execution.AggregateValidation(records) != execution.ValidationInfraError || validationDiskFull(records) {
				return nil
			}
			return resilience.Mark(errors.New("validation infrastructure failure"), resilience.Transient)
		})
		release()
		if attempts > 1 {
			e.d.Logger.WarnContext(ctx, "validation infrastructure retried", "execution_id", st.rec.ID.String(), "run_id", run.ID.String(), "attempts", attempts)
		}
	}
	if execution.AggregateValidation(records) == execution.ValidationCancelled {
		// Stopped by the overseer (shutdown): the run is ABANDONED — never
		// PASS / FAIL — and is rerun after restart (WBS 18.127).
		_ = e.d.Repo.FinishValidationRun(context.WithoutCancel(ctx), nil, run.ID, persistence.RunAbandoned, records, nil)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: validation cancelled", ErrConflict)
	}
	// Post-side-effect check (WBS 18.179 / 18.52): a repository that
	// vanished, was replaced or became unwritable while the commands ran
	// makes their outcome meaningless — the run is ABANDONED (never PASS
	// or FAIL) and the execution stops for recovery. A full disk does the
	// same (WBS 18.54).
	if gerr := e.guardRepository(ctx, st, "validation"); gerr != nil {
		var guard *repositoryGuardError
		if errors.As(gerr, &guard) {
			_ = e.d.Repo.FinishValidationRun(ctx, nil, run.ID, persistence.RunAbandoned, records, nil)
		}
		return nil, gerr
	}
	for _, r := range records {
		if validationDiskFull([]execution.ValidationRecord{r}) {
			_ = e.d.Repo.FinishValidationRun(ctx, nil, run.ID, persistence.RunAbandoned, records, nil)
			return nil, &repositoryGuardError{condition: recovery.DiskFull,
				reason:   "disk full: validation could not run (no space left on device); no PASS / FAIL recorded",
				evidence: map[string]any{"condition": string(recovery.DiskFull), "operation": "validation", "run_id": run.ID.String(), "command": r.Name}}
		}
	}
	outcome := execution.AggregateValidation(records)
	if missing := execution.MissingResults(cmds, records, ""); len(missing) > 0 && outcome == execution.ValidationPass {
		// A required command produced no result: never PASS (WBS 18.202).
		outcome = execution.ValidationInfraError
		e.d.Logger.WarnContext(ctx, "validation results missing", "execution_id", st.rec.ID.String(), "run_id", run.ID.String(), "missing", missing)
	}
	var acc *execution.AcceptanceResult
	if run.Scope == execution.ScopeUnit {
		a := execution.EvaluateAcceptance(checks, records, approvals)
		acc = &a
	}
	// Fault point: the commands ran, their results are not recorded yet.
	if err := faults.Hit(ctx, faults.AfterValidation); err != nil {
		return nil, err
	}
	if err := e.d.Repo.FinishValidationRun(ctx, nil, run.ID, string(outcome), records, acc); err != nil {
		return nil, err
	}
	typ := map[execution.ValidationOutcome]execution.EventType{execution.ValidationPass: execution.EventValidationPassed,
		execution.ValidationFail: execution.EventValidationFailed}[outcome]
	if typ == "" {
		typ = execution.EventValidationInfraError
	}
	payload := map[string]any{"run_id": run.ID.String(), "scope": string(run.Scope), "outcome": string(outcome)}
	if acc != nil {
		payload["acceptance"] = string(acc.Outcome)
	}
	if err := e.event(ctx, nil, st, typ, payload); err != nil {
		return nil, err
	}
	out, err := e.d.Repo.GetValidationRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	e.writeTestState(ctx, st, out)
	if err := e.durableInvariants(ctx, st, "after", "validation"); err != nil {
		return nil, err
	}
	return out, nil
}

// writeTestState renders TEST_STATE from deterministic records only (WBS
// 16.49, 15.49): the failing commands, exit codes and capped output — the
// FIX evidence the next prompt carries. ACTIVE_TASK is not touched.
func (e *Engine) writeTestState(ctx context.Context, st *engineState, run *persistence.ValidationRunRecord) {
	if e.files.crud == nil {
		return
	}
	var recs []execution.TestRecord
	for _, r := range run.Records {
		name := strings.TrimSpace(r.Program + " " + strings.Join(r.Args, " "))
		sum := ""
		if r.ExitCode != nil {
			sum = fmt.Sprintf("exit %d", *r.ExitCode)
		}
		if r.TimedOut {
			sum += " (timed out)"
		}
		if r.InfraReason != "" {
			sum += " infrastructure: " + r.InfraReason
		}
		if r.Outcome != execution.ValidationPass {
			sum += "\n" + indent(tail(r.Stdout+"\n"+r.Stderr, 4000))
		}
		recs = append(recs, execution.TestRecord{Name: fmt.Sprintf("[%s run %d] %s", run.Scope, run.Attempt, name), Passed: r.Outcome == execution.ValidationPass, Summary: strings.TrimSpace(sum)})
	}
	if run.Acceptance != nil {
		for _, v := range run.Acceptance.Verdicts {
			recs = append(recs, execution.TestRecord{Name: "acceptance: " + v.Text, Passed: v.Verdict == execution.VerdictPass, Summary: v.Verdict})
		}
	}
	name := ""
	phaseID := st.runtime.PhaseID
	if p := st.phaseByID(phaseID); p != nil {
		name = p.Name
	}
	if _, err := e.files.Derive(ctx, st.rec.ProjectID, "TEST_STATE", phaseID, nil, execution.RenderTestState(name, recs)); err != nil {
		e.d.Logger.WarnContext(ctx, "TEST_STATE not written", "execution_id", st.rec.ID.String(), "error", err.Error())
	}
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

// handleUnitValidation runs the unit's required deterministic validation
// and evaluates its acceptance criteria (WBS 16.43–16.48): PASS → the PASS
// route, FAIL → FIX; an inability to validate (missing executable, no
// command configured) or an unverified manual criterion is a recovery
// condition, never a PASS. The unit stays non-DONE either way.
func (e *Engine) handleUnitValidation(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if st.unit == nil {
		return e.requireRecovery(ctx, st, step, "no work unit to validate", nil)
	}
	spec := st.unit.Specification
	if spec == nil {
		spec = &wbs.WorkUnitSpecification{}
	}
	cmds := execution.ResolveValidationCommands(spec, st.engine, execution.ScopeUnit)
	approvals, err := e.d.Repo.Approvals(ctx, nil, st.unit.ID)
	if err != nil {
		return execution.StepResult{}, err
	}
	uid := st.unit.ID
	run, err := e.runValidation(ctx, st, step, &persistence.ValidationRunRecord{Scope: execution.ScopeUnit, WorkUnitID: &uid, PhaseID: st.runtime.PhaseID,
		Label: st.unit.Name}, cmds, spec.AcceptanceChecks, approvals)
	if err != nil {
		return execution.StepResult{}, err
	}
	refs := persistence.StepRefs{ValidationRunID: &run.ID}
	ev := map[string]any{"run_id": run.ID.String(), "outcome": run.Status}
	switch run.Status {
	case string(execution.ValidationInfraError):
		return e.requireRecovery(ctx, st, step, "validation could not run (infrastructure)", ev)
	case string(execution.ValidationNotConfigured):
		return e.requireRecovery(ctx, st, step, "no deterministic validation is configured for the work unit", ev)
	}
	token := "PASS"
	if run.Status == string(execution.ValidationFail) || (run.Acceptance != nil && run.Acceptance.Outcome == execution.AcceptanceFail) {
		token = "FAIL"
	} else if run.Acceptance != nil && run.Acceptance.Outcome == execution.AcceptanceReviewRequired {
		ev["acceptance"] = run.Acceptance.Verdicts
		return e.requireRecovery(ctx, st, step, "acceptance criteria require manual review", ev)
	}
	if token == "FAIL" && st.statefileWorkflow() {
		// The same failure over and over: the task is replanned (or, in a
		// workflow without a replan state, stopped) instead of repeated.
		if n, _, err := e.identicalFailures(ctx, st, st.unit.ID); err != nil {
			return execution.StepResult{}, err
		} else if n >= maxIdenticalFailures {
			ev["identical_failures"] = n
			if d, err := execution.RouteToken(st.machine, st.cur.Key, "REPLAN"); err == nil && d.Transition != nil {
				return e.advance(ctx, st, step, "REPLAN", advanceOpts{Refs: refs, Outcome: "REPLAN"})
			}
			return e.requireRecovery(ctx, st, step, fmt.Sprintf("%s %d times for task %s (validation runs as %s): replan the task so it can pass, then resolve",
				reasonIdenticalFailuresPrefix, n, wbs.UnitKey(st.unit), validationUser()), ev)
		}
	}
	return e.advance(ctx, st, step, token, advanceOpts{Refs: refs, Outcome: token})
}

// ---------------------------------------------------------------------------
// FIX (P16-108).

// isFixState reports a state whose dispatch is a repair attempt.
func isFixState(st *engineState) bool {
	return st.cur.Key == "FIX" || (st.curRec != nil && st.curRec.PromptTemplate == "FIX_WORK_UNIT")
}

// attemptsSinceReset counts the fix attempts made since the limit's last
// reset (a resolved recovery for it); attempt numbers keep increasing.
func (e *Engine) attemptsSinceReset(ctx context.Context, st *engineState, attempts []*persistence.FixAttemptRecord, limit string) (int, error) {
	reset, err := e.limitResetAt(ctx, st, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range attempts {
		if a.CreatedAt.After(reset) {
			n++
		}
	}
	return n, nil
}

// fixAttempt records a bounded repair attempt before the FIX dispatch
// (WBS 16.50/16.51), linked to the unit and the failed validation run.
// Returns nil + a recovery result when the limit is exhausted.
func (e *Engine) fixAttempt(ctx context.Context, st *engineState, step *persistence.StepRecord) (*persistence.FixAttemptRecord, *execution.StepResult, error) {
	if st.unit == nil || !isFixState(st) {
		return nil, nil, nil
	}
	uid := st.unit.ID
	attempts, err := e.d.Repo.ListFixAttempts(ctx, st.rec.ID, &uid)
	if err != nil {
		return nil, nil, err
	}
	for _, a := range attempts {
		if seq, ok := a.Evidence["step_seq"].(float64); ok && int64(seq) == step.Seq {
			return a, nil, nil
		}
	}
	used, err := e.attemptsSinceReset(ctx, st, attempts, LimitFixAttempts)
	if err != nil {
		return nil, nil, err
	}
	if execution.AttemptDecision(used, st.engine.MaxFixAttempts) == execution.AttemptExhausted {
		r, err := e.requireRecovery(ctx, st, step, reasonFixExhausted, map[string]any{"work_unit_id": uid.String(), "attempts": used})
		return nil, &r, err
	}
	f := &persistence.FixAttemptRecord{ExecutionID: st.rec.ID, TargetKind: persistence.FixTargetUnit, TargetID: uid, Attempt: len(attempts) + 1,
		Evidence: map[string]any{"step_seq": step.Seq}}
	if run, err := e.d.Repo.LatestValidationRun(ctx, nil, st.rec.ID, execution.ScopeUnit, uid); err == nil && run.Status == string(execution.ValidationFail) {
		id := run.ID
		f.FailedRunID = &id
		var failed []string
		for _, r := range run.Records {
			if r.Outcome != execution.ValidationPass {
				failed = append(failed, strings.TrimSpace(r.Program+" "+strings.Join(r.Args, " ")))
			}
		}
		f.Evidence["failed_commands"] = failed
	} else {
		f.Evidence["reason"] = "worker reported FAILED"
	}
	if err := e.d.Repo.CreateFixAttempt(ctx, nil, f); err != nil {
		return nil, nil, err
	}
	if err := e.event(ctx, nil, st, execution.EventFixAttempt, map[string]any{"work_unit_id": uid.String(), "attempt": f.Attempt}); err != nil {
		return nil, nil, err
	}
	return f, nil, nil
}

// ---------------------------------------------------------------------------
// Checkpoints (P16-109).

// checkpoint creates (or reconciles) an overseer-owned git checkpoint
// (WBS 16.54–16.57, 16.80, 16.143): the PENDING row is keyed; a commit
// already carrying the key is adopted; DISABLED never touches git.
func (e *Engine) checkpoint(ctx context.Context, st *engineState, step *persistence.StepRecord, kind execution.RepoCheckpointKind, target uuid.UUID,
	runID *uuid.UUID, policy execution.CheckpointPolicy, subject string) (*persistence.RepoCheckpointRecord, error) {
	attempt := 1
	if list, err := e.d.Repo.ListRepoCheckpoints(ctx, st.rec.ID); err == nil {
		for _, c := range list {
			if c.Kind == kind && c.TargetID != nil && *c.TargetID == target && c.Status == persistence.CheckpointFailed {
				attempt++
			}
		}
	}
	if err := e.durableInvariants(ctx, st, "before", subject); err != nil {
		return nil, err
	}
	if policy != execution.CheckpointDisabled {
		// Committing into a different repository or onto another branch is
		// never acceptable: stop before the commit. A missing / unwritable
		// / full repository cannot be committed to at all — the attempt
		// fails, is recorded as evidence, and the checkpoint policy decides
		// (REQUIRED blocks the unit, WBS 16.53–16.59 / 18.61).
		if reason := e.d.Gate.BlockedSideEffects(); reason != "" {
			return nil, &gateWaitError{reason: "checkpoint held: " + reason}
		}
		var guard *repositoryGuardError
		if err := e.guardRepository(ctx, st, subject); errors.As(err, &guard) {
			if guard.condition == recovery.RepositoryIdentityMismatch || guard.condition == recovery.RepositoryHeadChanged {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
	}
	key := execution.CheckpointKey(st.rec.ID, kind, target, attempt)
	tid := target
	rec, err := e.d.Repo.CreateCheckpointIntent(ctx, &persistence.RepoCheckpointRecord{ProjectID: st.rec.ProjectID, ExecutionID: st.rec.ID,
		Kind: kind, TargetID: &tid, Key: key, ValidationRunID: runID})
	if err != nil {
		return nil, err
	}
	if step != nil {
		id := rec.ID
		_ = e.d.Repo.SetStepRefs(ctx, nil, step.ID, persistence.StepRefs{CheckpointID: &id})
	}
	if rec.Status != persistence.CheckpointPending {
		return rec, nil
	}
	payload := map[string]any{"checkpoint_id": rec.ID.String(), "kind": string(kind), "target": target.String(), "key": key}
	if policy == execution.CheckpointDisabled {
		out, err := e.d.Repo.CompleteCheckpoint(ctx, rec.ID, persistence.CheckpointSkipped, "", "", "", "checkpoint policy DISABLED")
		if err == nil {
			_ = e.event(ctx, nil, st, execution.EventCheckpointSkipped, payload)
		}
		return out, err
	}
	// Reconcile before committing (WBS 18.58 / 18.59 / 18.147): a commit
	// carrying this checkpoint's trailer may already exist (a crash after
	// the commit, before its record). A failed lookup never leads to a
	// second commit.
	found, ferr := e.d.Checkpointer.FindAllByKey(ctx, st.cwd(), key)
	if ferr != nil && !errors.Is(ferr, gitcheckpoint.ErrNotRepository) {
		return nil, ferr
	}
	// (Not a repository: nothing can exist or be committed — the commit
	// attempt below fails and the checkpoint policy decides.)
	if len(found) > 0 {
		if why := e.ambiguousCheckpoint(ctx, st, found); why != "" {
			commits := make([]string, 0, len(found))
			for _, c := range found {
				commits = append(commits, c.Commit)
			}
			return nil, &repositoryGuardError{condition: recovery.CheckpointAmbiguous,
				reason: "checkpoint ambiguous: " + why + "; the checkpoint is not adopted and the work is not marked done — " +
					"repair the repository history so exactly one checkpoint commit sits on the expected HEAD, then retry the checkpoint",
				evidence: map[string]any{"condition": string(recovery.CheckpointAmbiguous), "checkpoint_id": rec.ID.String(), "key": key,
					"commits": commits}}
		}
		payload["reconciled"] = true
		return e.completeCheckpoint(ctx, st, rec, found[0].Commit, found[0].Parent, found[0].Tree, payload)
	}
	if step != nil {
		if err := e.hook(ctx, step.Action, "checkpoint"); err != nil {
			return nil, err
		}
		if err := e.fenceNow(ctx, st); err != nil {
			return nil, err
		}
	}
	c, cerr := e.d.Checkpointer.Create(ctx, st.cwd(), execution.CheckpointMessage(kind, key, st.rec.ID, subject))
	if faults.IsCrash(cerr) {
		// A simulated process crash abandons the step (the PENDING row and
		// any landed commit are reconciled after restart).
		return nil, cerr
	}
	if errors.Is(cerr, gitcheckpoint.ErrLocked) || errors.Is(cerr, gitcheckpoint.ErrTimeout) {
		// Another git process holds the lock, or git hung and was killed:
		// the checkpoint stays PENDING (a commit that did land is found by
		// its trailer on the retry) — never FAILED by contention (WBS
		// 18.57 / 18.60).
		return nil, cerr
	}
	if cerr != nil {
		out, err := e.d.Repo.CompleteCheckpoint(ctx, rec.ID, persistence.CheckpointFailed, "", "", "", cerr.Error())
		if err == nil {
			payload["error"] = cerr.Error()
			_ = e.event(ctx, nil, st, execution.EventCheckpointFailed, payload)
		}
		return out, err
	}
	if step != nil {
		if err := e.hook(ctx, step.Action, "checkpoint-committed"); err != nil {
			return nil, err
		}
	}
	return e.completeCheckpoint(ctx, st, rec, c.Commit, c.Parent, c.Tree, payload)
}

func (e *Engine) completeCheckpoint(ctx context.Context, st *engineState, rec *persistence.RepoCheckpointRecord, commit string, parent *string, tree string, payload map[string]any) (*persistence.RepoCheckpointRecord, error) {
	p := ""
	if parent != nil {
		p = *parent
	}
	out, err := e.d.Repo.CompleteCheckpoint(ctx, rec.ID, persistence.CheckpointCreated, commit, p, tree, "")
	if err != nil {
		return nil, err
	}
	// The overseer moved HEAD: the checkpoint commit is now expected
	// (WBS 18.50).
	if e.d.Identities != nil {
		branch := ""
		if prev, err := e.d.Identities.LatestExpectedHead(ctx, st.rec.ID); err == nil {
			branch = prev.Branch
		}
		cid := out.ID
		if _, err := e.d.Identities.AppendExpectedHead(ctx, &persistence.ExpectedHeadRecord{ExecutionID: st.rec.ID, ProjectID: st.rec.ProjectID,
			Head: commit, Branch: branch, Source: persistence.HeadFromCheckpoint, CheckpointID: &cid, RecordedAt: e.d.Now().UTC()}); err != nil {
			return nil, err
		}
	}
	payload["commit"] = commit
	_ = e.event(ctx, nil, st, execution.EventCheckpointCreated, payload)
	if err := e.durableInvariants(ctx, st, "after", "checkpoint"); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Unit gates (P16-110).

// handleUnitGates marks the unit DONE only after its required validation
// and acceptance passed and the checkpoint policy is satisfied (WBS
// 16.53–16.58). A REQUIRED checkpoint failure keeps the unit non-DONE and
// requires recovery; BEST_EFFORT records the failure and proceeds.
func (e *Engine) handleUnitGates(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if st.unit == nil {
		return e.requireRecovery(ctx, st, step, "no work unit at the gates", nil)
	}
	uid := st.unit.ID
	if st.unit.Status == wbs.WorkUnitStatusDONE {
		return e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), advanceOpts{})
	}
	run, err := e.d.Repo.LatestValidationRun(ctx, nil, st.rec.ID, execution.ScopeUnit, uid)
	if err != nil || run.Status != string(execution.ValidationPass) || run.Acceptance == nil || run.Acceptance.Outcome != execution.AcceptancePass {
		return e.requireRecovery(ctx, st, step, "work unit gates not satisfied: no passing validation", map[string]any{"work_unit_id": uid.String()})
	}
	// The stored PASS must still carry a PASS result for every required
	// command (WBS 18.202): a run whose results were lost or altered
	// cannot complete the unit.
	spec := st.unit.Specification
	if spec == nil {
		spec = &wbs.WorkUnitSpecification{}
	}
	if missing := execution.MissingResults(execution.ResolveValidationCommands(spec, st.engine, execution.ScopeUnit), run.Records, execution.ValidationPass); len(missing) > 0 {
		return e.requireRecovery(ctx, st, step, "validation result is incomplete or corrupt: required command results are missing; the unit is not marked done",
			map[string]any{"condition": string(recovery.ArtifactCorrupt), "run_id": run.ID.String(), "missing": missing})
	}
	cp, err := e.checkpoint(ctx, st, step, execution.RepoCheckpointUnit, uid, &run.ID, st.engine.CheckpointPolicy, fmt.Sprintf("work unit %d %s", st.unit.Sequence, st.unit.Name))
	if err != nil {
		return execution.StepResult{}, err
	}
	if cp.Status == persistence.CheckpointFailed && execution.OnCheckpointFailure(st.engine.CheckpointPolicy) == execution.CheckpointBlock {
		reason, ev := e.checkpointFailure(ctx, st, "required checkpoint failed", cp.Error, map[string]any{"checkpoint_id": cp.ID.String(), "error": cp.Error})
		return e.requireRecovery(ctx, st, step, reason, ev)
	}
	cpID, runID := cp.ID, run.ID
	return e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), advanceOpts{
		Refs: persistence.StepRefs{CheckpointID: &cpID, ValidationRunID: &runID},
		Tx:   func(tx persistence.Transaction) error { return e.d.Repo.MarkUnitDone(ctx, tx, uid, st.rec.ID, runID) },
		Events: []pendingEvent{{Type: execution.EventUnitDone, Payload: map[string]any{"work_unit_id": uid.String(), "validation_run_id": runID.String(),
			"checkpoint_id": cpID.String(), "checkpoint_status": cp.Status}}}})
}

// ambiguousCheckpoint explains why found trailer commits cannot be adopted
// as this checkpoint ("" = adopt found[0]): more than one commit carries
// the trailer, or the commit does not sit directly on the HEAD the
// execution expected when the checkpoint started.
func (e *Engine) ambiguousCheckpoint(ctx context.Context, st *engineState, found []*gitcheckpoint.Commit) string {
	if len(found) > 1 {
		return fmt.Sprintf("%d commits carry the checkpoint trailer", len(found))
	}
	if e.d.Identities == nil {
		return ""
	}
	exp, err := e.d.Identities.LatestExpectedHead(ctx, st.rec.ID)
	if err != nil || exp.Head == "" {
		return "" // no recorded expectation (legacy execution): trailer only
	}
	parent := ""
	if found[0].Parent != nil {
		parent = *found[0].Parent
	}
	if parent != exp.Head {
		return fmt.Sprintf("the trailer commit %s has parent %s, expected %s", short(found[0].Commit), short(parent), short(exp.Head))
	}
	return ""
}

// validationDiskFull reports a record that could not run because the disk
// is full (WBS 18.54).
func validationDiskFull(records []execution.ValidationRecord) bool {
	for _, r := range records {
		if r.Outcome == execution.ValidationInfraError && resilience.MentionsDiskFull(r.InfraReason) {
			return true
		}
	}
	return false
}

// ErrValidationCapacity: the global validation concurrency limit is
// reached (WBS 18.93); the execution waits and retries.
var ErrValidationCapacity = errors.New("validation capacity reached")

// validationCapacity reports whether a validation slot is free now (a
// reservation is not held: the slot is taken for the commands' duration
// by validationSlot).
func (e *Engine) validationCapacity() bool {
	if e.validations == nil {
		return true
	}
	return len(e.validations) < cap(e.validations)
}

// validationSlot takes a validation slot for the commands' duration
// (waiting at most until ctx ends: the capacity check just before makes a
// wait rare and short).
func (e *Engine) validationSlot(ctx context.Context) (func(), error) {
	if e.validations == nil {
		return func() {}, nil
	}
	select {
	case e.validations <- struct{}{}:
		return func() { <-e.validations }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
