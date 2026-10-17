package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
	"overseer/internal/worker"
)

// ValidateRepositoryState runs the current work unit's deterministic
// validation against the repository as recovery evidence (WBS 18.23): the
// run is recorded, but the execution stays RECOVERY_REQUIRED and the
// dispatch's delivery status is untouched.
func (e *Engine) ValidateRepositoryState(ctx context.Context, projectID, executionID uuid.UUID) (map[string]any, error) {
	m := e.lock(executionID)
	m.Lock()
	defer m.Unlock()
	st, err := e.load(ctx, executionID)
	if err != nil {
		return nil, translate(err)
	}
	if st.rec.ProjectID != projectID {
		return nil, ErrNotFound
	}
	if st.rec.Status != execution.ExecutionRecovery {
		return nil, fmt.Errorf("%w: the execution does not require recovery", ErrConflict)
	}
	if st.unit == nil {
		return nil, fmt.Errorf("%w: no work unit to validate", ErrConflict)
	}
	spec := st.unit.Specification
	if spec == nil {
		spec = &wbs.WorkUnitSpecification{}
	}
	cmds := execution.ResolveValidationCommands(spec, st.engine, execution.ScopeUnit)
	approvals, err := e.d.Repo.Approvals(ctx, nil, st.unit.ID)
	if err != nil {
		return nil, translate(err)
	}
	uid := st.unit.ID
	run, err := e.runValidation(ctx, st, nil, &persistence.ValidationRunRecord{Scope: execution.ScopeUnit, WorkUnitID: &uid,
		PhaseID: st.runtime.PhaseID, Label: "recovery evidence: " + st.unit.Name}, cmds, spec.AcceptanceChecks, approvals)
	if err != nil {
		return nil, err
	}
	return map[string]any{"validation_run_id": run.ID.String(), "outcome": run.Status, "evidence_only": true,
		"note": "the result is evidence; it does not change the dispatch's delivery status"}, nil
}

// applyResolution applies an operator resolution of an unknown delivery
// during evaluation (PLAN P18 Decision 8). ACCEPT_REPOSITORY_STATE routes
// the unit to deterministic validation (worker COMPLETE is not assumed —
// validation decides). RESEND_AFTER_REVIEW sends the request once more
// (attempt + 1) and evaluates that dispatch instead. Returns the
// (possibly replaced) evaluation; nil means "no resolution applies".
func (e *Engine) applyResolution(ctx context.Context, st *engineState, step *persistence.StepRecord, ev *evaluation) (*evaluation, *execution.StepResult, error) {
	if e.d.Recovery == nil || ev.dispatch == nil {
		return nil, nil, nil
	}
	res, err := e.d.Recovery.GetResolution(ctx, ev.dispatch.ID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	switch res.Resolution {
	case execution.ResolutionAcceptRepositoryState:
		_, _ = e.d.Recovery.ConsumeResolution(ctx, nil, ev.dispatch.ID)
		out := *ev
		out.token = execution.TokenComplete
		out.operatorResolution = res.Resolution
		return &out, nil, nil
	case execution.ResolutionResendAfterReview:
		return e.resendAfterReview(ctx, st, step, ev.dispatch, res)
	}
	return nil, nil, nil
}

// resendAfterReview performs the reviewed resend exactly once: the
// resolution is consumed together with a durable resend intent (fenced),
// then one new attempt of the same prompt is sent. A crash after the
// intent never resends again: the consumed resolution routes to the
// newer attempt. A provably unsent resend releases the resolution.
func (e *Engine) resendAfterReview(ctx context.Context, st *engineState, step *persistence.StepRecord, old *worker.WorkerDispatch,
	res *persistence.DispatchResolutionRecord) (*evaluation, *execution.StepResult, error) {
	newer := func() (*worker.WorkerDispatch, error) {
		atts, err := e.d.Dispatches.ListAttempts(ctx, st.rec.ProjectID, old.OperationID)
		if err != nil {
			return nil, err
		}
		var best *worker.WorkerDispatch
		for i := range atts {
			if atts[i].Attempt > old.Attempt && (best == nil || atts[i].Attempt > best.Attempt) {
				best = &atts[i]
			}
		}
		return best, nil
	}
	if res.ConsumedAt != nil {
		n, err := newer()
		if err != nil {
			return nil, nil, err
		}
		if n == nil {
			r, rerr := e.requireRecovery(ctx, st, step, "worker delivery unknown: the reviewed resend's outcome is unknown", map[string]any{
				"condition": "DELIVERY_UNKNOWN", "dispatch_id": old.ID.String(), "resolution": res.Resolution})
			return nil, &r, rerr
		}
		ev, err := e.evaluateDispatch(ctx, st, n.ID)
		return ev, nil, err
	}
	err := e.inTx(ctx, st, func(tx persistence.Transaction) error {
		if err := e.fence(ctx, tx, st); err != nil {
			return err
		}
		ok, err := e.d.Recovery.ConsumeResolution(ctx, tx, old.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: resolution already applied", ErrConflict)
		}
		return e.event(ctx, tx, st, execution.EventDispatchIntent, map[string]any{"step_seq": step.Seq, "state": st.cur.Key,
			"resend_of": old.ID.String(), "resolution": res.Resolution, "resolution_id": res.ID.String(), "attempt": old.Attempt + 1})
	})
	if err != nil {
		return nil, nil, translate(err)
	}
	if err := e.fenceNow(ctx, st); err != nil {
		return nil, nil, err
	}
	req := DispatchRequest{ProjectID: st.rec.ProjectID, PhaseID: old.PhaseID, WorkUnitID: old.WorkUnitID,
		WorkerPromptID: old.WorkerPromptID, RuntimeStateID: old.RuntimeStateID, StateKey: old.StateKey, RoleKey: old.RoleKey, Attempt: old.Attempt + 1}
	var out *DispatchResult
	for i := 0; i < 32; i++ {
		out, err = e.d.Worker.Dispatch(ctx, req)
		if !errors.Is(err, ErrDuplicateDispatch) {
			break
		}
		req.Attempt++ // an earlier, provably unsent attempt holds this number
	}
	if err != nil {
		var we *worker.WorkerError
		if errors.As(err, &we) && we.Delivery == worker.DeliveryNotSent {
			_ = e.d.Recovery.ReleaseResolution(ctx, old.ID)
		}
		return nil, nil, err
	}
	if out.Err != nil && out.Err.Delivery == worker.DeliveryNotSent {
		_ = e.d.Recovery.ReleaseResolution(ctx, old.ID)
		w, werr := e.wait(ctx, st, step, "worker provider unavailable: the reviewed resend was not sent and will be retried", persistence.StepRefs{})
		return nil, &w, werr
	}
	ev, err := e.evaluateDispatch(ctx, st, out.Dispatch.ID)
	return ev, nil, err
}

// AcceptRepositoryHead records the repository's current HEAD and branch
// as expected (WBS 18.50 / 18.51): the operator's reviewed acceptance of
// an external change, appended to the expected-HEAD history with actor
// and reason. Only the same repository can be accepted — a different
// root commit / top level stays an identity mismatch. Repeating it for an
// already accepted position changes nothing. The caller (the recovery
// action service) handles confirmation, idempotency and resuming.
func (e *Engine) AcceptRepositoryHead(ctx context.Context, projectID, executionID uuid.UUID, by, note string) (map[string]any, error) {
	if e.d.Identifier == nil || e.d.Identities == nil {
		return nil, fmt.Errorf("%w: repository HEAD tracking is not available", ErrConflict)
	}
	m := e.lock(executionID)
	m.Lock()
	defer m.Unlock()
	st, err := e.load(ctx, executionID)
	if err != nil {
		return nil, translate(err)
	}
	if st.rec.ProjectID != projectID {
		return nil, ErrNotFound
	}
	got, err := e.d.Identifier.Identify(ctx, st.cwd())
	if err != nil {
		return nil, fmt.Errorf("%w: the repository cannot be read: %v", ErrConflict, err)
	}
	rec, err := e.d.Identities.Get(ctx, executionID)
	if err != nil {
		return nil, translate(err)
	}
	want := rec.Identity
	want.Branch = got.Branch
	if want.IsGit && !got.IsGit {
		return nil, fmt.Errorf("%w: the working directory is no longer a git repository", ErrConflict)
	}
	if identity, _ := execution.IdentityMismatch(want, got); identity != "" {
		return nil, fmt.Errorf("%w: %s — a different repository cannot be accepted", ErrConflict, identity)
	}
	prev, err := e.d.Identities.LatestExpectedHead(ctx, executionID)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	out := map[string]any{"accepted_head": got.Head, "accepted_branch": got.Branch}
	if prev != nil {
		out["previous_head"], out["previous_branch"] = prev.Head, prev.Branch
		if prev.Head == got.Head && prev.Branch == got.Branch {
			out["unchanged"] = true
			return out, nil
		}
	}
	if _, err := e.d.Identities.AppendExpectedHead(ctx, &persistence.ExpectedHeadRecord{ExecutionID: executionID, ProjectID: projectID,
		Head: got.Head, Branch: got.Branch, Source: persistence.HeadFromOperatorAccept, Actor: nonEmpty(by, "operator"), Reason: note,
		RecordedAt: e.d.Now().UTC()}); err != nil {
		return nil, translate(err)
	}
	payload := map[string]any{"actor": nonEmpty(by, "operator"), "reason": note}
	for k, v := range out {
		payload[k] = v
	}
	if err := e.event(ctx, nil, st, execution.EventRepositoryHeadAccepted, payload); err != nil {
		return nil, translate(err)
	}
	return out, nil
}
