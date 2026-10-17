package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/recovery"
	"overseer/internal/state"
	"overseer/internal/wbs"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// ---------------------------------------------------------------------------
// Context, prompt, integrity, session (P16-87 … P16-90).

// prepared is a step's invocation context.
type prepared struct {
	snapshotID uuid.UUID
	promptID   uuid.UUID
	roleKey    string
}

// prepare creates the immutable context snapshot (WBS 16.24) and the worker
// prompt with the template version captured in the execution snapshot
// (WBS 16.25), reusing the step's recorded artifacts on retry.
func (e *Engine) prepare(ctx context.Context, st *engineState, step *persistence.StepRecord) (*prepared, error) {
	p := &prepared{}
	if step.ContextSnapshotID != nil {
		p.snapshotID = *step.ContextSnapshotID
	} else {
		var extra []string
		if isRepairState(st) {
			extra = []string{"TEST_STATE"}
		}
		snap, err := e.d.ContextSnaps.CreateExecutionSnapshotIncluding(ctx, st.rec.ProjectID.String(), extra)
		if err != nil {
			return nil, fmt.Errorf("context snapshot: %w", err)
		}
		p.snapshotID = snap.ID
		if err := e.d.Repo.SetStepRefs(ctx, nil, step.ID, persistence.StepRefs{ContextSnapshotID: &p.snapshotID,
			Detail: map[string]any{"role": snap.RoleSnapshot.RoleKey}}); err != nil {
			return nil, err
		}
		step.ContextSnapshotID = &p.snapshotID
		p.roleKey = snap.RoleSnapshot.RoleKey
	}
	if p.roleKey == "" {
		if r, ok := step.Detail["role"].(string); ok {
			p.roleKey = r
		}
	}
	// The snapshot is verified before anything is composed from it (WBS
	// 18.199): a corrupt snapshot is never turned into a prompt.
	if err := e.d.ContextSnaps.VerifySnapshot(ctx, st.rec.ProjectID, p.snapshotID); errors.Is(err, ErrSnapshotCorrupt) {
		return nil, &repositoryGuardError{condition: recovery.ArtifactCorrupt, reason: "context snapshot is corrupt: " + err.Error() + "; no prompt composed",
			evidence: map[string]any{"condition": string(recovery.ArtifactCorrupt), "snapshot_id": p.snapshotID.String()}}
	} else if err != nil {
		return nil, err
	}
	if step.WorkerPromptID != nil {
		p.promptID = *step.WorkerPromptID
		return p, nil
	}
	key := ""
	if st.curRec != nil {
		key = st.curRec.PromptTemplate
	}
	var tmpl *execution.PromptTemplateSnapshot
	for i := range st.content.PromptTemplates {
		if st.content.PromptTemplates[i].Key == key {
			tmpl = &st.content.PromptTemplates[i]
		}
	}
	if tmpl == nil {
		return nil, fmt.Errorf("%w: state %s has no captured prompt template %q", ErrInvalidInput, st.cur.Key, key)
	}
	pr, err := e.d.Cache.PrepareForContext(ctx, st.rec.ProjectID, PrepareRequest{SnapshotID: p.snapshotID, TemplateID: tmpl.TemplateID, TemplateVersion: tmpl.Version})
	if err != nil {
		return nil, fmt.Errorf("prompt preparation: %w", err)
	}
	if pr == nil || pr.WorkerPrompt == nil {
		return nil, fmt.Errorf("prompt preparation produced no worker prompt")
	}
	p.promptID = pr.WorkerPrompt.ID
	if err := e.d.Repo.SetStepRefs(ctx, nil, step.ID, persistence.StepRefs{WorkerPromptID: &p.promptID,
		Detail: map[string]any{"cache_key": pr.CacheKey, "cache_hit": pr.Hit}}); err != nil {
		return nil, err
	}
	step.WorkerPromptID = &p.promptID
	return p, nil
}

// verifyPrompt re-checks the stored prompt's hash before dispatch
// (WBS 16.26): a modified artifact is never sent.
func (e *Engine) verifyPrompt(ctx context.Context, id uuid.UUID) error {
	wp, err := e.d.Prompts.GetWorkerPrompt(ctx, id)
	if err != nil {
		return err
	}
	return worker.VerifyPromptIntegrity(*wp)
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil && err2 == nil {
		return ea == eb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// ensureSession requires a connected, active session whose confirmed
// working directory is the execution's canonical CWD (WBS 16.27). It
// creates a session / changes directory when needed and never dispatches.
func (e *Engine) ensureSession(ctx context.Context, st *engineState, forceCD bool) (*persistence.WorkerSessionRecord, error) {
	cwd := st.cwd()
	changed := false
	sess, err := e.d.Worker.ActiveSession(ctx, st.rec.ProjectID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoActiveSession) || (err == nil && sess == nil) {
		if sess, err = e.d.Worker.NewSession(ctx, st.rec.ProjectID); err != nil {
			return nil, fmt.Errorf("new worker session: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	if sess.WorkingDirectory == nil || !samePath(*sess.WorkingDirectory, cwd) {
		if sess, err = e.d.Worker.ChangeDirectory(ctx, st.rec.ProjectID, cwd); err != nil {
			return nil, fmt.Errorf("worker working directory %s not confirmed: %w", cwd, err)
		}
		changed = true
	}
	rd, err := e.d.Worker.Readiness(ctx, st.rec.ProjectID)
	if err != nil {
		return nil, err
	}
	if !rd.Connected && rd.SessionActive {
		// This instance holds no transport for the persisted session (a
		// takeover from another instance, or a provider restart):
		// reconcile through Phase 12 — it never resends and never creates
		// a session. A session the provider no longer knows is LOST, and
		// a fresh session is created from PostgreSQL state (WBS 18.24 /
		// 18.25); chat history is never needed.
		res, rerr := e.d.Worker.Reconnect(ctx, st.rec.ProjectID)
		if providerUnreachable(rerr) {
			return nil, fmt.Errorf("reconnect worker session: %w", rerr)
		}
		if rerr == nil && res == worker.ReconcileLost {
			if sess, err = e.d.Worker.NewSession(ctx, st.rec.ProjectID); err != nil {
				return nil, fmt.Errorf("new worker session after loss: %w", err)
			}
			if sess.WorkingDirectory == nil || !samePath(*sess.WorkingDirectory, cwd) {
				if sess, err = e.d.Worker.ChangeDirectory(ctx, st.rec.ProjectID, cwd); err != nil {
					return nil, fmt.Errorf("worker working directory %s not confirmed: %w", cwd, err)
				}
				changed = true
			}
		}
		if rd, err = e.d.Worker.Readiness(ctx, st.rec.ProjectID); err != nil {
			return nil, err
		}
	}
	if forceCD && !changed && rd.Connected && rd.SessionActive {
		// Workflow commands always run after an explicit "change working
		// directory" prompt, so the agent works in the project, never in
		// its own root.
		if sess, err = e.d.Worker.ChangeDirectory(ctx, st.rec.ProjectID, cwd); err != nil {
			return nil, fmt.Errorf("worker working directory %s not confirmed: %w", cwd, err)
		}
	}
	if !rd.Connected || !rd.SessionActive || sess.WorkingDirectory == nil || !samePath(*sess.WorkingDirectory, cwd) {
		return nil, fmt.Errorf("worker session not ready (connected=%v active=%v cwd=%v, want %s)", rd.Connected, rd.SessionActive, ptrStr(sess.WorkingDirectory), cwd)
	}
	return sess, nil
}

// promptTooLarge reports whether prompt preparation stopped because no
// prompt that safely fits the worker context could be produced.
func promptTooLarge(err error) bool {
	return errors.Is(err, ErrPromptPreparationFailed) || errors.Is(err, ErrCanonicalExceedsContext) ||
		errors.Is(err, ErrCompressionTargetImpossible)
}

// providerUnreachable reports whether err is a transport failure that
// provably sent nothing (connection refused / timed out before the send).
func providerUnreachable(err error) bool {
	var we *worker.WorkerError
	return errors.As(err, &we) && we.Delivery == worker.DeliveryNotSent &&
		(we.Code == worker.CodeUnavailable || we.Code == worker.CodeTimeout)
}

func ptrStr(p *string) string {
	if p == nil {
		return "<none>"
	}
	return *p
}

// ---------------------------------------------------------------------------
// Dispatch (P16-91).

// dispatchOutcome is a terminal dispatch.
type dispatchOutcome struct {
	dispatch *worker.WorkerDispatch
	err      *worker.WorkerError
}

// findStepDispatch returns the (non-continuation) dispatch created for the
// step's prompt after the step's intent, if any — the reconciliation that
// keeps a crash between the dispatch row and the step record from causing
// a blind resend (WBS 16.28, 16.78).
func (e *Engine) findStepDispatch(ctx context.Context, st *engineState, step *persistence.StepRecord) (*worker.WorkerDispatch, error) {
	if step.DispatchID != nil {
		d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, *step.DispatchID)
		if err != nil {
			return nil, err
		}
		return d, nil
	}
	if step.WorkerPromptID == nil {
		return nil, nil
	}
	ds, _, err := e.d.Dispatches.List(ctx, persistence.DispatchFilter{ProjectID: st.rec.ProjectID, StateKey: step.StateKey, Limit: 20})
	if err != nil {
		return nil, err
	}
	for i := range ds {
		d := ds[i]
		if d.ContinuationID == nil && d.WorkerPromptID == *step.WorkerPromptID && d.RuntimeStateID == st.runtime.ID && !d.CreatedAt.Before(step.IntentAt) {
			return &d, nil
		}
	}
	return nil, nil
}

func dispatchTerminal(d *worker.WorkerDispatch) bool {
	switch d.Status {
	case worker.DispatchPending, worker.DispatchSending, worker.DispatchWaiting:
		return false
	}
	return true
}

func dispatchErr(d *worker.WorkerDispatch) *worker.WorkerError {
	if d.Status == worker.DispatchCompleted {
		return nil
	}
	del := worker.DeliveryUnknown
	if d.Delivery != nil {
		del = *d.Delivery
	}
	return &worker.WorkerError{Code: worker.WorkerErrorCode(d.ErrorCode), Message: d.ErrorMessage, Delivery: del}
}

// dispatchStep sends the step's prompt exactly once (P16-91): snapshot +
// prompt are recorded on the step, the prompt is verified, the session is
// made ready, the execution enters WAITING with a DISPATCH_INTENT event,
// then the generic Phase 12 worker dispatches (its own durable row is
// written before the send). A dispatch already recorded or found for the
// step is reconciled instead of re-sent; an unknown delivery is never
// resent. Returns nil outcome + a StepResult when the step stopped
// (recovery / waiting).
func (e *Engine) dispatchStep(ctx context.Context, st *engineState, step *persistence.StepRecord, req DispatchRequest) (*dispatchOutcome, *execution.StepResult, error) {
	if d, err := e.findStepDispatch(ctx, st, step); err != nil {
		return nil, nil, err
	} else if d != nil {
		if step.DispatchID == nil {
			id := d.ID
			if err := e.d.Repo.SetStepRefs(ctx, nil, step.ID, persistence.StepRefs{DispatchID: &id}); err != nil {
				return nil, nil, err
			}
			step.DispatchID = &id
		}
		if !dispatchTerminal(d) {
			// A call no longer running anywhere (e.g. the database was lost
			// after the send, WBS 18.34) is reconciled — PENDING → not
			// sent, otherwise DELIVERY_UNKNOWN — never left waiting forever
			// and never replayed.
			rd, rerr := e.d.Worker.ReconcileOrphan(ctx, *d)
			if rerr != nil {
				return nil, nil, rerr
			}
			if rd == nil {
				res, err := e.wait(ctx, st, step, "worker dispatch in flight", persistence.StepRefs{})
				return nil, &res, err
			}
			d = rd
		}
		we := dispatchErr(d)
		if !safeToResend(we) {
			return &dispatchOutcome{dispatch: d, err: we}, nil, nil
		}
		// Definitely not sent: a retry is safe (attempt + 1 below).
		req.Attempt = d.Attempt + 1
	}
	if reason := e.d.Gate.Blocked(); reason != "" {
		// New work is blocked process-wide (e.g. an unacknowledged audit
		// verification failure, decision U9): nothing is sent; the step
		// stays INTENT and resumes once the gate opens.
		res, err := e.wait(ctx, st, step, "new work blocked: "+reason, persistence.StepRefs{})
		return nil, &res, err
	}
	if err := e.guardRepository(ctx, st, "worker dispatch"); err != nil {
		return nil, nil, err
	}
	if err := e.durableInvariants(ctx, st, "before", "worker dispatch"); err != nil {
		return nil, nil, err
	}
	var p *prepared
	var err error
	if st.curRec != nil && execution.IsCommandTemplate(st.curRec.PromptTemplate) {
		p, err = e.prepareCommand(ctx, st, step)
	} else {
		p, err = e.prepare(ctx, st, step)
	}
	var guard *repositoryGuardError
	if errors.As(err, &guard) {
		return nil, nil, err
	}
	if err != nil {
		evidence := map[string]any{"error": err.Error()}
		reason := "prompt preparation failed"
		if promptTooLarge(err) {
			// The prompt cannot be made to fit safely (compression
			// unavailable / ineffective and the canonical prompt exceeds
			// the worker context): nothing is truncated or sent (WBS 18.30).
			reason = "prompt preparation failed: the prompt does not fit the worker context and was not sent"
			evidence["condition"] = string(recovery.PromptTooLarge)
		}
		res, rerr := e.requireRecovery(ctx, st, step, reason, evidence)
		return nil, &res, rerr
	}
	if err := e.verifyPrompt(ctx, p.promptID); err != nil {
		res, rerr := e.requireRecovery(ctx, st, step, "prompt integrity check failed: the prompt was not sent",
			map[string]any{"worker_prompt_id": p.promptID.String(), "error": err.Error()})
		return nil, &res, rerr
	}
	// Provider admission (WBS 18.26 / 18.92): an open circuit or full
	// dispatch capacity makes the execution wait — before any durable
	// intent, so nothing is sent and nothing is corrupted.
	release, aerr := e.d.Worker.Admit(ctx, st.rec.ProjectID)
	if aerr != nil {
		if errors.Is(aerr, ErrProviderUnavailable) || errors.Is(aerr, ErrWorkerCapacity) {
			w, werr := e.wait(ctx, st, step, aerr.Error(), persistence.StepRefs{})
			return nil, &w, werr
		}
		return nil, nil, aerr
	}
	defer release()
	if _, err := e.ensureSession(ctx, st, st.curRec != nil && execution.IsCommandTemplate(st.curRec.PromptTemplate)); err != nil {
		if providerUnreachable(err) {
			// The provider cannot be reached (WBS 18.26): an outage, not a
			// recovery condition — it counts against the circuit and the
			// execution waits; nothing was sent.
			e.d.Worker.recordAvailability(ctx, st.rec.ProjectID, worker.AsWorkerError(err))
			w, werr := e.wait(ctx, st, step, "worker provider unavailable: "+err.Error(), persistence.StepRefs{})
			return nil, &w, werr
		}
		res, rerr := e.requireRecovery(ctx, st, step, "worker session is not ready", map[string]any{"error": err.Error(), "cwd": st.cwd()})
		return nil, &res, rerr
	}
	baseline := e.repositoryObservation(ctx, st, "baseline")
	// Durable intent (WBS 16.28/16.29): WAITING + DISPATCH_INTENT committed
	// before the request.
	err = e.inTx(ctx, st, func(tx persistence.Transaction) error {
		if err := e.fence(ctx, tx, st); err != nil {
			return err
		}
		upd := persistence.PositionUpdate{SetWait: true, WaitReason: "worker dispatch in flight"}
		if st.rec.Status != execution.ExecutionWaiting {
			upd.Status = execution.ExecutionWaiting
		}
		rec, err := e.d.Repo.UpdatePosition(ctx, tx, st.rec.ID, st.rec.Version, upd)
		if err != nil {
			return err
		}
		st.rec = rec
		payload := map[string]any{"step_seq": step.Seq, "state": st.cur.Key, "worker_prompt_id": p.promptID.String(), "role": p.roleKey}
		// Repository baseline at intent time (WBS 18.20): evidence for a
		// later ambiguous delivery, never a causal claim.
		for k, v := range baseline {
			payload[k] = v
		}
		return e.event(ctx, tx, st, execution.EventDispatchIntent, payload)
	})
	if err != nil {
		return nil, nil, translate(err)
	}
	if err := e.hook(ctx, step.Action, "dispatch"); err != nil {
		return nil, nil, err
	}
	if err := e.fenceNow(ctx, st); err != nil {
		return nil, nil, err
	}
	req.ProjectID, req.WorkerPromptID, req.RuntimeStateID, req.StateKey = st.rec.ProjectID, p.promptID, st.runtime.ID, st.cur.Key
	if req.RoleKey == "" {
		req.RoleKey = p.roleKey
	}
	if req.Attempt == 0 {
		req.Attempt = step.Attempt
	}
	var res *DispatchResult
	for i := 0; i < 32; i++ {
		res, err = e.d.Worker.Dispatch(ctx, req)
		if !errors.Is(err, ErrDuplicateDispatch) {
			break
		}
		req.Attempt++
	}
	if err != nil {
		var we *worker.WorkerError
		switch {
		case errors.Is(err, ErrPromptIntegrity):
			r, rerr := e.requireRecovery(ctx, st, step, "prompt integrity check failed: the prompt was not sent", map[string]any{"error": err.Error()})
			return nil, &r, rerr
		case errors.As(err, &we) && we.Delivery == worker.DeliveryNotSent, errors.Is(err, ErrNoActiveSession):
			r, rerr := e.requireRecovery(ctx, st, step, "worker dispatch was not admitted", map[string]any{"error": err.Error()})
			return nil, &r, rerr
		}
		// The worker may have answered but persistence failed: the row
		// stays in flight and restart recovery marks it unknown.
		return nil, nil, err
	}
	d := res.Dispatch
	id := d.ID
	if err := e.d.Repo.SetStepRefs(ctx, nil, step.ID, persistence.StepRefs{DispatchID: &id}); err != nil {
		return nil, nil, err
	}
	step.DispatchID = &id
	e.workerRunEvidence(ctx, st, id, baseline)
	if err := e.durableInvariants(ctx, st, "after", "worker dispatch"); err != nil {
		return nil, nil, err
	}
	if safeToResend(res.Err) {
		// The transport proves the request never left (WBS 18.17), or the
		// provider explicitly rejected it because the session no longer
		// exists (WBS 18.24; the session is now LOST): the step stays
		// pending and the next step resends it (attempt + 1, in a fresh
		// session when needed) — exactly one eventual delivery.
		w, werr := e.wait(ctx, st, step, "worker provider unavailable: the request was not sent and will be resent", persistence.StepRefs{})
		return nil, &w, werr
	}
	return &dispatchOutcome{dispatch: &d, err: res.Err}, nil, nil
}

// forwardToken picks an action state's forward edge after its dispatch:
// the only enabled outgoing edge, else EVALUATE / SUBMIT / APPLY / PROCEED.
func forwardToken(m *state.StateMachineSnapshot, from string) string {
	var outs []string
	for _, t := range m.Transitions {
		if t.Enabled && t.From == from {
			outs = append(outs, t.Outcome)
		}
	}
	if len(outs) == 1 {
		return outs[0]
	}
	for _, tok := range []string{"EVALUATE", "SUBMIT", "APPLY", "PROCEED"} {
		for _, o := range outs {
			if state.MatchOutcome(o, tok) {
				return o
			}
		}
	}
	return "EVALUATE"
}

// pendingDispatch is the most recent dispatch the engine recorded (or a
// continuation dispatched) that no EVALUATE_RESULT / PLAN_VALIDATE step has
// consumed yet — adopted by the next action state instead of re-sent.
func (e *Engine) pendingDispatch(ctx context.Context, st *engineState) (*uuid.UUID, error) {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return nil, err
	}
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		if s.DispatchID == nil || s.Status != execution.StepApplied {
			continue
		}
		switch s.Action {
		case execution.ActionEvaluateResult, execution.ActionPlanValidate:
			return nil, nil
		}
		id := *s.DispatchID
		return &id, nil
	}
	return nil, nil
}

// handleDispatch drives IMPLEMENT / FIX / CONTINUE (and any custom action
// state): the claimed unit is EXECUTING, the prompt is dispatched once, and
// the forward edge leads to evaluation. A dispatch a continuation already
// made for the unit is adopted, never duplicated (PLAN Decision 9).
func (e *Engine) handleDispatch(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	tok := forwardToken(st.machine, st.cur.Key)
	if step.DispatchID == nil {
		if pend, err := e.pendingDispatch(ctx, st); err != nil {
			return execution.StepResult{}, err
		} else if pend != nil {
			if d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, *pend); err == nil && dispatchTerminal(d) &&
				(d.ContinuationID != nil || st.cur.Key != d.StateKey) {
				id := *pend
				return e.advance(ctx, st, step, tok, advanceOpts{Refs: persistence.StepRefs{DispatchID: &id, Detail: map[string]any{"adopted": true}}})
			}
		}
	}
	if st.unit != nil && st.unit.Status != wbs.WorkUnitStatusEXECUTING {
		if err := e.d.Repo.SetUnitStatus(ctx, nil, st.unit.ID, []wbs.WorkUnitStatus{wbs.WorkUnitStatusINCOMPLETE, wbs.WorkUnitStatusVALIDATING,
			wbs.WorkUnitStatusEXECUTING}, wbs.WorkUnitStatusEXECUTING); err != nil && !errors.Is(err, persistence.ErrConflict) {
			return execution.StepResult{}, err
		}
	}
	var unitID *uuid.UUID
	if st.unit != nil {
		id := st.unit.ID
		unitID = &id
	}
	req := DispatchRequest{PhaseID: st.runtime.PhaseID, WorkUnitID: unitID}
	fix, stopped, err := e.fixAttempt(ctx, st, step)
	if err != nil || stopped != nil {
		if stopped == nil {
			return execution.StepResult{}, err
		}
		return *stopped, err
	}
	if fix != nil {
		defer func() {
			if step.DispatchID != nil {
				_ = e.d.Repo.SetFixDispatch(ctx, nil, fix.ID, *step.DispatchID)
			}
		}()
	}
	out, stop, err := e.dispatchStep(ctx, st, step, req)
	if err != nil || stop != nil {
		if stop == nil {
			return execution.StepResult{}, err
		}
		return *stop, err
	}
	if err := e.hook(ctx, step.Action, "dispatched"); err != nil {
		return execution.StepResult{}, err
	}
	id := out.dispatch.ID
	return e.advance(ctx, st, step, tok, advanceOpts{Refs: persistence.StepRefs{DispatchID: &id}})
}

// ---------------------------------------------------------------------------
// Result evaluation (P16-92 / P16-93).

// evaluation is the engine's reading of a finished dispatch.
type evaluation struct {
	// operatorResolution records an applied delivery resolution.
	operatorResolution string
	token              execution.ResultToken
	result             *persistence.StoredWorkerResult
	parseErr           *workerresult.ParseError
	dispatch           *worker.WorkerDispatch
}

// evaluateDispatch reads the dispatch outcome through Phase 13 only (WBS
// 16.31): the WorkerResult status, never response prose.
func (e *Engine) evaluateDispatch(ctx context.Context, st *engineState, dispatchID uuid.UUID) (*evaluation, error) {
	d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, dispatchID)
	if err != nil {
		return nil, err
	}
	ev := &evaluation{dispatch: d}
	if we := dispatchErr(d); we != nil {
		ev.token = execution.MapDispatchOutcome(execution.DispatchOutcome{Error: we})
		return ev, nil
	}
	res, err := e.d.Results.ResultByDispatch(ctx, st.rec.ProjectID, dispatchID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, persistence.ErrNotFound) {
		out, perr := e.d.Results.ParseDispatch(ctx, st.rec.ProjectID, dispatchID)
		if perr != nil {
			return nil, perr
		}
		if out.Err != nil {
			ev.parseErr = out.Err
			ev.token = execution.TokenProtocolError
			return ev, nil
		}
		res = out.Result
	} else if err != nil {
		return nil, err
	}
	// A stored result whose content no longer matches its recorded
	// canonical hash is corrupt: nothing advances on it (WBS 18.201).
	if got := workerresult.NormalizedHash(res.Result); res.Result.NormalizedHash != "" && got != res.Result.NormalizedHash {
		return nil, &repositoryGuardError{condition: recovery.ArtifactCorrupt,
			reason: "stored worker result is corrupt (canonical hash mismatch): no workflow advancement until reconciled",
			evidence: map[string]any{"condition": string(recovery.ArtifactCorrupt), "dispatch_id": dispatchID.String(), "result_id": res.ID.String(),
				"recorded_hash": res.Result.NormalizedHash, "computed_hash": got}}
	}
	ev.result = res
	ev.token = execution.MapDispatchOutcome(execution.DispatchOutcome{Status: res.Result.Status})
	return ev, nil
}

// handleEvaluate routes the evaluated dispatch (WBS 16.32–16.42):
// COMPLETE → validation (unit VALIDATING, never DONE); INCOMPLETE /
// CONTEXT_LIMIT → continuation decision (unit INCOMPLETE); FAILED → the
// configured edge; BLOCKED / PROTOCOL_ERROR / DELIVERY_UNKNOWN / transport
// failure without an edge → recovery with evidence. Planning dispatches
// are evaluated by PLAN_VALIDATE instead.
func (e *Engine) handleEvaluate(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	pend, err := e.pendingDispatch(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	if pend == nil {
		return e.requireRecovery(ctx, st, step, "no dispatch to evaluate", nil)
	}
	ev, err := e.evaluateDispatch(ctx, st, *pend)
	if err != nil {
		return execution.StepResult{}, err
	}
	// An explicit, reviewed operator resolution (Phase 18 Decision 8); a
	// request that failed after delivery is resolved the same way. A
	// reviewed resend that is itself unresolved-unknown carries its own
	// resolution: the chain is followed (bounded).
	for hop := 0; hop < 8 && (ev.token == execution.TokenDeliveryUnknown || ev.token == execution.TokenTransportFailed); hop++ {
		nev, stop, err := e.applyResolution(ctx, st, step, ev)
		if err != nil || stop != nil {
			if stop == nil {
				return execution.StepResult{}, err
			}
			return *stop, err
		}
		if nev == nil {
			break
		}
		same := nev.dispatch != nil && ev.dispatch != nil && nev.dispatch.ID == ev.dispatch.ID
		ev = nev
		if ev.dispatch != nil {
			id := ev.dispatch.ID
			pend = &id
		}
		if same {
			break
		}
	}
	refs := persistence.StepRefs{DispatchID: pend, Detail: map[string]any{"result": string(ev.token)}}
	if ev.operatorResolution != "" {
		refs.Detail["operator_resolution"] = ev.operatorResolution
	}
	evidence := map[string]any{"dispatch_id": pend.String(), "token": string(ev.token)}
	if ev.parseErr != nil {
		evidence["protocol_error"] = string(ev.parseErr.Code)
	}
	if ev.result != nil {
		evidence["summary"] = ev.result.Result.Summary
		evidence["blocking"] = ev.result.Result.Blocking
	}
	if ev.token == execution.TokenDeliveryUnknown || ev.token == execution.TokenTransportFailed {
		for k, v := range e.deliveryEvidence(ctx, st, *pend) {
			evidence[k] = v
		}
	}
	route, err := execution.RouteToken(st.machine, st.cur.Key, ev.token)
	if ev.token == execution.TokenTransportFailed && (err != nil || route.Transition == nil) {
		// The worker received the request and failed (e.g. a provider
		// 500): any partial work is unknown. Without a workflow route this
		// is the delivery-unknown recovery — an operator reviews and
		// resolves (resend after review / accept the repository state);
		// nothing is resent automatically.
		code := ""
		if ev.dispatch != nil {
			code = ev.dispatch.ErrorCode
		}
		evidence["condition"] = string(recovery.DeliveryUnknown)
		_ = e.d.Repo.SetStepRefs(ctx, nil, step.ID, refs)
		return e.requireRecovery(ctx, st, step, "worker delivery unknown: the request failed after delivery ("+code+"); partial work unknown; not resent", evidence)
	}
	if err != nil {
		return e.requireRecovery(ctx, st, step, err.Error(), evidence)
	}
	if ev.token == execution.TokenProtocolError && route.Transition != nil {
		// A machine may route protocol errors to a repair; a model that
		// keeps violating the protocol must not loop forever (WBS 18.70).
		if n := e.protocolErrors(ctx, st); n >= maxProtocolRepairs {
			evidence["condition"] = string(recovery.ProtocolViolation)
			evidence["protocol_errors"] = n + 1
			return e.requireRecovery(ctx, st, step, fmt.Sprintf(reasonProtocolPrefix+" (%d): repair attempts exhausted", n+1), evidence)
		}
	}
	if route.Transition == nil {
		reason := route.Recovery
		switch ev.token {
		case execution.TokenDeliveryUnknown:
			reason = "worker delivery unknown: the request is not resent"
		case execution.TokenBlocked:
			reason = "worker reported BLOCKED"
		case execution.TokenProtocolError:
			reason = "worker response violates the result protocol"
		}
		_ = e.d.Repo.SetStepRefs(ctx, nil, step.ID, refs)
		return e.requireRecovery(ctx, st, step, reason, evidence)
	}
	o := advanceOpts{Refs: refs, Outcome: string(ev.token)}
	if st.unit != nil {
		uid := st.unit.ID
		var to wbs.WorkUnitStatus
		switch ev.token {
		case execution.TokenComplete:
			to = wbs.WorkUnitStatusVALIDATING
		case execution.TokenIncomplete, execution.TokenContextLimit:
			to = wbs.WorkUnitStatusINCOMPLETE
		}
		if to != "" {
			o.Tx = func(tx persistence.Transaction) error {
				err := e.d.Repo.SetUnitStatus(ctx, tx, uid, []wbs.WorkUnitStatus{wbs.WorkUnitStatusEXECUTING, wbs.WorkUnitStatusINCOMPLETE,
					wbs.WorkUnitStatusVALIDATING}, to)
				if errors.Is(err, persistence.ErrConflict) {
					return nil
				}
				return err
			}
		}
	}
	return e.advance(ctx, st, step, string(route.Transition.Outcome), o)
}

// ---------------------------------------------------------------------------
// Continuation (P16-102 / P16-103).

// continuationFor finds (or asks Phase 14 to create) the continuation of
// an evaluated dispatch. The engine never invents a strategy (16.33).
func (e *Engine) continuationFor(ctx context.Context, st *engineState, src uuid.UUID) (*continuation.Continuation, error) {
	find := func() (*continuation.Continuation, error) {
		if st.unit == nil {
			return nil, nil
		}
		uid := st.unit.ID
		list, _, err := e.d.Continuations.Continuations(ctx, persistence.ContinuationFilter{ProjectID: st.rec.ProjectID, WorkUnitID: &uid, Limit: 200})
		if err != nil {
			return nil, err
		}
		for i := range list {
			if list[i].SourceDispatchID == src {
				c := list[i]
				return &c, nil
			}
		}
		return nil, nil
	}
	c, err := find()
	if err != nil || c != nil {
		return c, err
	}
	d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, src)
	if err != nil {
		return nil, err
	}
	if d.Status == worker.DispatchCompleted {
		if res, err := e.d.Results.ResultByDispatch(ctx, st.rec.ProjectID, src); err == nil {
			e.d.Continuations.OnParseOutcome(ctx, st.rec.ProjectID, src, res)
		}
	} else {
		e.d.Continuations.OnDispatchFailed(ctx, *d)
	}
	return find()
}

// handleContinuation drives the Phase 14 continuation of the evaluated
// dispatch at CONTINUE_DECISION (and the SAME_SESSION / RESET_SESSION
// control states): the continuation decides the strategy, resets the
// session / restores the CWD / rebuilds context when it chooses to, and
// dispatches the continuation prompt; the engine maps the decided strategy
// onto the workflow edges and adopts the continuation's dispatch. Limits,
// no-progress, blocking and failures end in recovery (16.38/16.39).
func (e *Engine) handleContinuation(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if t, _ := execution.RouteToken(st.machine, st.cur.Key, "SAME"); t.Transition == nil {
		// SAME_SESSION / RESET_SESSION: the decision step already adopted
		// the continuation's dispatch — follow the CONTINUE edge with it.
		if prev := e.lastApplied(ctx, st, execution.ActionContinuation); prev != nil && prev.DispatchID != nil {
			return e.advance(ctx, st, step, "CONTINUE", advanceOpts{Refs: persistence.StepRefs{DispatchID: prev.DispatchID,
				ContinuationID: prev.ContinuationID}})
		}
	}
	src, err := e.continuationSource(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	if src == nil {
		return e.requireRecovery(ctx, st, step, "no incomplete dispatch to continue", nil)
	}
	c, err := e.continuationFor(ctx, st, *src)
	if err != nil {
		return execution.StepResult{}, err
	}
	if c == nil {
		return e.requireRecovery(ctx, st, step, "Phase 14 created no continuation for the incomplete dispatch", map[string]any{"dispatch_id": src.String()})
	}
	cid := c.ID
	for i := 0; i < 8 && !c.Status.IsTerminal() && c.Status != continuation.StatusWaiting; i++ {
		if _, err := e.d.Continuations.Run(ctx, st.rec.ProjectID, c.ID); err != nil {
			e.d.Logger.WarnContext(ctx, "continuation run", "continuation_id", c.ID.String(), "error", err.Error())
		}
		cs, _, err := e.d.Continuations.Continuations(ctx, persistence.ContinuationFilter{ProjectID: st.rec.ProjectID, WorkUnitID: &c.WorkUnitID, Limit: 200})
		if err != nil {
			return execution.StepResult{}, err
		}
		before := c.Status
		for j := range cs {
			if cs[j].ID == cid {
				cc := cs[j]
				c = &cc
			}
		}
		if c.Status == before && c.DispatchID == nil {
			break
		}
	}
	refs := persistence.StepRefs{ContinuationID: &cid, Detail: map[string]any{"continuation_status": string(c.Status), "strategy": string(c.Strategy)}}
	switch c.Status {
	case continuation.StatusBlocked, continuation.StatusFailed, continuation.StatusRecoveryRequired, continuation.StatusCancelled:
		_ = e.d.Repo.SetStepRefs(ctx, nil, step.ID, refs)
		ev := map[string]any{"continuation_id": cid.String(), "continuation_status": string(c.Status)}
		if c.Decision != nil {
			ev["decision_reason"] = string(c.Decision.Reason)
		}
		return e.requireRecovery(ctx, st, step, "continuation stopped: "+string(c.Status), ev)
	}
	if c.DispatchID == nil {
		return e.wait(ctx, st, step, "continuation in progress", refs)
	}
	d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, *c.DispatchID)
	if err != nil {
		return execution.StepResult{}, err
	}
	if !dispatchTerminal(d) {
		return e.wait(ctx, st, step, "continuation dispatch in flight", refs)
	}
	refs.DispatchID = c.DispatchID
	// Map the decided strategy onto the workflow (SAME / RESET), then the
	// CONTINUE edge.
	tok := "CONTINUE"
	switch st.cur.Key {
	case "SAME_SESSION", "RESET_SESSION":
	default:
		if t, _ := execution.RouteToken(st.machine, st.cur.Key, "SAME"); t.Transition != nil {
			tok = "SAME"
			if c.Strategy == continuation.ContinueFreshSession {
				tok = "RESET"
			}
		}
	}
	return e.advance(ctx, st, step, tok, advanceOpts{Refs: refs, Outcome: tok})
}

// continuationSource is the dispatch the current continuation decision is
// about: the latest evaluated dispatch (by EVALUATE_RESULT).
func (e *Engine) continuationSource(ctx context.Context, st *engineState) (*uuid.UUID, error) {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return nil, err
	}
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		if s.Action == execution.ActionEvaluateResult && s.Status == execution.StepApplied && s.DispatchID != nil {
			id := *s.DispatchID
			return &id, nil
		}
	}
	return nil, nil
}

// lastApplied returns the most recent applied step of an action.
func (e *Engine) lastApplied(ctx context.Context, st *engineState, a execution.StepAction) *persistence.StepRecord {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return nil
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Action == a && steps[i].Status == execution.StepApplied {
			return steps[i]
		}
	}
	return nil
}

// isRepairState reports a dispatch whose prompt must carry the
// deterministic failure evidence (TEST_STATE): FIX-like and hardening
// repair states.
func isRepairState(st *engineState) bool {
	if isFixState(st) || st.cur.Key == "HARDENING_FIX" {
		return true
	}
	return st.curRec != nil && st.curRec.PromptTemplate == "HARDENING_FIX"
}

// repositoryObservation captures HEAD / branch / working-tree fingerprint
// under prefix (empty map when no inspector or no repository).
func (e *Engine) repositoryObservation(ctx context.Context, st *engineState, prefix string) map[string]any {
	out := map[string]any{}
	if e.d.Inspector == nil {
		return out
	}
	snap, err := e.d.Inspector.Inspect(ctx, st.cwd())
	if err != nil {
		out[prefix+"_error"] = err.Error()
		return out
	}
	if snap.HeadCommit != nil {
		out[prefix+"_head"] = *snap.HeadCommit
	}
	if snap.Branch != nil {
		out[prefix+"_branch"] = *snap.Branch
	}
	out[prefix+"_fingerprint"] = snap.Fingerprint
	out[prefix+"_dirty"] = snap.Dirty
	out[prefix+"_at"] = e.d.Now().UTC().Format(time.RFC3339Nano)
	return out
}

// maxEvidencePaths bounds the changed-path list in recovery evidence.
const maxEvidencePaths = 50

// deliveryEvidence is the repository / session / timing evidence of an
// unknown delivery (WBS 18.20): the baseline captured at intent, the
// current repository state, a bounded change list, the worker session and
// the dispatch timestamps. It never claims whether the worker caused any
// change (WBS 18.21 / 18.22).
func (e *Engine) deliveryEvidence(ctx context.Context, st *engineState, dispatchID uuid.UUID) map[string]any {
	out := map[string]any{"condition": "DELIVERY_UNKNOWN",
		"evidence_note": "repository observations only: they neither prove nor disprove that the worker received or processed the request"}
	if evs, err := e.d.Execs.ListEvents(ctx, st.rec.ProjectID, &st.rec.ID); err == nil {
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].Type == execution.EventDispatchIntent {
				for k, v := range evs[i].Payload {
					if strings.HasPrefix(k, "baseline_") {
						out[k] = v
					}
				}
				break
			}
		}
	}
	for k, v := range e.repositoryObservation(ctx, st, "current") {
		out[k] = v
	}
	if b, ok := out["baseline_fingerprint"].(string); ok {
		if c, ok := out["current_fingerprint"].(string); ok {
			out["repository_changed_since_intent"] = b != c
		}
	}
	if cp, ok := e.d.Inspector.(changedPathser); ok && e.d.Inspector != nil {
		if paths, err := cp.ChangedPaths(ctx, st.cwd()); err == nil {
			if len(paths) > maxEvidencePaths {
				out["changed_paths_truncated"] = len(paths)
				paths = paths[:maxEvidencePaths]
			}
			out["changed_paths"] = paths
		}
	}
	if d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, dispatchID); err == nil && d != nil {
		out["dispatch_created_at"] = d.CreatedAt.UTC().Format(time.RFC3339Nano)
		if d.StartedAt != nil {
			out["dispatch_started_at"] = d.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		if d.CompletedAt != nil {
			out["dispatch_completed_at"] = d.CompletedAt.UTC().Format(time.RFC3339Nano)
		}
		out["worker_session_id"] = d.SessionID.String()
		out["dispatch_attempt"] = d.Attempt
	}
	return out
}

// safeToResend reports a failed dispatch that provably did not run: the
// transport never sent it, or the provider answered that the session no
// longer exists (an explicit rejection, not an ambiguous delivery).
func safeToResend(we *worker.WorkerError) bool {
	if we == nil {
		return false
	}
	if we.Delivery == worker.DeliveryNotSent {
		return true
	}
	return we.Code == worker.CodeSessionLost && we.Delivery == worker.DeliveryReceived
}

// workerRunEvidence records the repository before / after a worker run
// (WBS 18.62): HEAD, branch and working-tree fingerprint at intent and
// after the call. A HEAD or branch change surfaces here and the next
// repository guard stops new work (WBS 18.50); file changes are expected
// from the worker and are evidence only — never a claim of who made them.
func (e *Engine) workerRunEvidence(ctx context.Context, st *engineState, dispatchID uuid.UUID, before map[string]any) {
	if e.d.Inspector == nil {
		return
	}
	payload := map[string]any{"dispatch_id": dispatchID.String(),
		"evidence_note": "repository observations only; they do not establish who made a change"}
	for k, v := range before {
		payload[k] = v
	}
	after := e.repositoryObservation(ctx, st, "after")
	for k, v := range after {
		payload[k] = v
	}
	payload["head_changed"] = before["baseline_head"] != after["after_head"]
	payload["branch_changed"] = before["baseline_branch"] != after["after_branch"]
	payload["working_tree_changed"] = before["baseline_fingerprint"] != after["after_fingerprint"]
	if payload["working_tree_changed"] == true {
		// Which paths the run left changed (bounded) — edits outside what
		// the unit asked for are visible to the operator (WBS 18.220).
		if cp, ok := e.d.Inspector.(changedPathser); ok {
			if paths, err := cp.ChangedPaths(ctx, st.cwd()); err == nil {
				if len(paths) > maxEvidencePaths {
					payload["changed_paths_truncated"] = len(paths)
					paths = paths[:maxEvidencePaths]
				}
				payload["changed_paths"] = paths
			}
		}
	}
	if err := e.event(ctx, nil, st, execution.EventWorkerRunEvidence, payload); err != nil {
		e.d.Logger.WarnContext(ctx, "worker run evidence not recorded", "execution_id", st.rec.ID.String(), "error", err.Error())
	}
}

// maxProtocolRepairs bounds protocol-repair attempts per work unit (WBS
// 18.70; decision register): after the original response and this many
// repairs, the execution stops with PROTOCOL_VIOLATION.
const maxProtocolRepairs = 2

// protocolErrors counts the execution's earlier evaluations of the current
// unit that ended in a protocol error.
func (e *Engine) protocolErrors(ctx context.Context, st *engineState) int {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return 0
	}
	reset, _ := e.limitResetAt(ctx, st, LimitProtocolRepairs)
	n := 0
	for _, s := range steps {
		if s.Action != execution.ActionEvaluateResult || s.Status != execution.StepApplied || !s.IntentAt.After(reset) {
			continue
		}
		if st.unit != nil && (s.WorkUnitID == nil || *s.WorkUnitID != st.unit.ID) {
			continue
		}
		if r, _ := s.Detail["result"].(string); r == string(execution.TokenProtocolError) {
			n++
		}
	}
	return n
}
