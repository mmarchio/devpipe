package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// Step executes exactly one lifecycle transition of a continuation (WBS
// 14.61) and reports whether another step can follow immediately. Every
// external side effect is preceded by a persisted intent status and
// followed by a persisted confirmed status (WBS 14.55-14.58); a paused or
// cancelled project stops before the next side effect (WBS 14.63/14.64).
func (s *ContinuationService) Step(ctx context.Context, projectID, id uuid.UUID) (bool, error) {
	c, err := s.d.Repo.Get(ctx, projectID, id)
	if err != nil {
		return false, err
	}
	if c.Status.IsTerminal() {
		return false, nil
	}
	if h := s.stepHook(); h != nil && !h(c) {
		return false, nil
	}
	st, err := s.projectStatus(ctx, c.ProjectID)
	if err != nil {
		return false, err
	}
	switch st {
	case project.StatusCANCELLED:
		_, err := s.advance(ctx, c, continuation.StatusCancelled, "project cancelled", persistence.ContinuationMutation{})
		return false, wrapStep(c, err)
	case project.StatusRUNNING:
	default:
		return false, nil // paused / not running: position preserved
	}
	var next *continuation.Continuation
	switch c.Status {
	case continuation.StatusPending:
		next, err = s.advance(ctx, c, continuation.StatusDeciding, "decide", persistence.ContinuationMutation{})
	case continuation.StatusDeciding:
		next, err = s.decide(ctx, c)
	case continuation.StatusSameSession:
		next, err = s.validateSameSession(ctx, c)
	case continuation.StatusResetRequired:
		next, err = s.startReset(ctx, c)
	case continuation.StatusCreatingSession:
		next, err = s.createSession(ctx, c)
	case continuation.StatusRestoringContext:
		next, err = s.restoreContext(ctx, c)
	case continuation.StatusRestoringCWD:
		next, err = s.restoreCWD(ctx, c)
	case continuation.StatusReady:
		next, err = s.readyAndDispatch(ctx, c)
	case continuation.StatusDispatching, continuation.StatusWaiting:
		next, err = s.reconcileDispatch(ctx, c.ID, c.ProjectID)
	}
	if err != nil {
		return false, wrapStep(c, err)
	}
	if next == nil || next.Status.IsTerminal() || next.Status == continuation.StatusWaiting {
		return false, nil
	}
	return next.Status != c.Status || next.Version != c.Version, nil
}

// decide is WBS 14.4-14.15 + 14.36-14.43: authoritative inputs only.
func (s *ContinuationService) decide(ctx context.Context, c *continuation.Continuation) (*continuation.Continuation, error) {
	settings, _, err := s.d.Settings.Get(ctx, c.ProjectID)
	if err != nil {
		return nil, err
	}
	threshold, err := s.Threshold(ctx, c.ProjectID)
	if err != nil {
		return nil, err
	}
	rt, err := s.d.Runtime.Get(ctx, c.ProjectID)
	if err != nil {
		return nil, err
	}
	if rt.ID != c.RuntimeStateID || !sameUUID(rt.PhaseID, &c.PhaseID) || !sameUUID(rt.WorkUnitID, &c.WorkUnitID) {
		// WBS 14.66-14.68: never detached from the authoritative workflow.
		return s.recovery(ctx, c, continuation.RecoveryBindingChanged, "runtime position no longer matches the continuation", persistence.ContinuationMutation{})
	}
	src, err := s.d.Dispatches.Get(ctx, c.ProjectID, c.SourceDispatchID)
	if err != nil {
		return nil, err
	}
	reading, err := s.ReadingForDispatch(ctx, src)
	if err != nil {
		return nil, err
	}
	sessStatus, sessID := worker.SessionStatus(""), (*uuid.UUID)(nil)
	if sess, err := s.d.Sessions.GetActiveForProject(ctx, c.ProjectID); err == nil {
		sessStatus, sessID = sess.Status, &sess.ID
		if sess.ID != src.SessionID && c.Trigger != continuation.TriggerManualRetry {
			// The source session is no longer the active one: the
			// telemetry describes another conversation.
			reading = continuation.UnknownReading()
		}
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	var resultStatus workerresult.WorkerResultStatus
	if c.SourceResultID != nil {
		resultStatus = workerresult.StatusIncomplete
	}
	trigger := c.Trigger
	if trigger == continuation.TriggerManualRetry {
		trigger = continuation.TriggerResultIncomplete
		if c.SourceResultID == nil {
			trigger = continuation.TriggerContextLimit
		}
	}
	counts, err := s.d.Repo.CountsForWorkUnit(ctx, c.ProjectID, c.WorkUnitID)
	if err != nil {
		return nil, err
	}
	dc := continuation.DecisionContext{ResultStatus: resultStatus, Trigger: trigger, Reading: reading, Threshold: threshold,
		SessionStatus: sessStatus, Settings: settings, Override: c.OverrideStrategy, ContinuationAttempts: c.Attempt,
		SessionResets: counts.SuccessfulResets}
	d, err := s.d.Policy.Decide(dc)
	if errors.Is(err, continuation.ErrUnsafeOverride) {
		dc.Override = ""
		d, err = s.d.Policy.Decide(dc)
	}
	if err != nil {
		return nil, err
	}
	m := persistence.ContinuationMutation{Decision: &d}
	if r, hit := continuation.CheckLimits(c.Attempt, continuation.Counts{SuccessfulResets: counts.SuccessfulResets,
		NoProgressStreak: c.NoProgressStreak}, settings, d.Strategy); hit {
		return s.recovery(ctx, c, r, "", m)
	}
	if d.Strategy == continuation.ContinueSameSession {
		m.SessionID = sessID
		return s.advance(ctx, c, continuation.StatusSameSession, string(d.Reason), m)
	}
	return s.advance(ctx, c, continuation.StatusResetRequired, string(d.Reason), m)
}

func (s *ContinuationService) toFresh(ctx context.Context, c *continuation.Continuation, reason continuation.DecisionReason) (*continuation.Continuation, error) {
	d := continuation.Decision{Strategy: continuation.ContinueFreshSession, Reason: reason, Source: worker.SourceUnknown}
	if c.Decision != nil {
		d = *c.Decision
		d.Strategy, d.Reason = continuation.ContinueFreshSession, reason
	}
	settings, _, err := s.d.Settings.Get(ctx, c.ProjectID)
	if err != nil {
		return nil, err
	}
	counts, err := s.d.Repo.CountsForWorkUnit(ctx, c.ProjectID, c.WorkUnitID)
	if err != nil {
		return nil, err
	}
	m := persistence.ContinuationMutation{Decision: &d, ClearSession: true, ClearDispatch: true}
	if continuation.ResetLimitReached(counts.SuccessfulResets, settings) {
		return s.recovery(ctx, c, continuation.RecoveryResetLimit, "", m)
	}
	return s.advance(ctx, c, continuation.StatusResetRequired, string(reason), m)
}

func (s *ContinuationService) ensureSnapshot(ctx context.Context, c *continuation.Continuation) (*uuid.UUID, error) {
	if c.SnapshotID != nil {
		return c.SnapshotID, nil
	}
	if c.SourceSnapshotID == nil || s.d.SnapshotSvc == nil {
		return nil, ErrInfrastructure
	}
	snap, err := s.d.SnapshotSvc.CreateContinuationSnapshot(ctx, c.ProjectID, *c.SourceSnapshotID)
	if err != nil {
		return nil, err
	}
	id := snap.ID
	return &id, nil
}

// validateSameSession is WBS 14.19: the existing session must still be
// ACTIVE, the project's and usable; otherwise a fresh session is required.
func (s *ContinuationService) validateSameSession(ctx context.Context, c *continuation.Continuation) (*continuation.Continuation, error) {
	sess, rec, err := s.d.Worker.ConfirmSession(ctx, c.ProjectID)
	switch {
	case errors.Is(err, ErrNoActiveSession):
		return s.toFresh(ctx, c, continuation.ReasonSessionLost)
	case err != nil && rec != worker.ReconcileLost:
		return nil, err
	}
	if rec == worker.ReconcileLost || sess == nil || sess.Status != worker.SessionActive || sess.ProjectID != c.ProjectID ||
		(c.SessionID != nil && *c.SessionID != sess.ID) {
		return s.toFresh(ctx, c, continuation.ReasonSessionLost)
	}
	snap, err := s.ensureSnapshot(ctx, c)
	if errors.Is(err, ErrContinuationBinding) {
		return s.recovery(ctx, c, continuation.RecoveryBindingChanged, errMessage(err), persistence.ContinuationMutation{})
	}
	if err != nil {
		return nil, err
	}
	id := sess.ID
	return s.advance(ctx, c, continuation.StatusRestoringCWD, "session confirmed", persistence.ContinuationMutation{SessionID: &id, SnapshotID: snap})
}

// startReset persists the session-creation intent (WBS 14.56).
func (s *ContinuationService) startReset(ctx context.Context, c *continuation.Continuation) (*continuation.Continuation, error) {
	m := persistence.ContinuationMutation{ClearSession: true}
	if sess, err := s.d.Sessions.GetActiveForProject(ctx, c.ProjectID); err == nil {
		id := sess.ID
		m.OldSessionID = &id
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	return s.advance(ctx, c, continuation.StatusCreatingSession, "session reset started", m)
}

// createSession is WBS 14.24/14.25/14.39/14.96: exactly one NewSession per
// reset attempt; a session created after the persisted intent is adopted
// instead of creating another; a failure never falls back to the old
// session.
func (s *ContinuationService) createSession(ctx context.Context, c *continuation.Continuation) (*continuation.Continuation, error) {
	facts := continuation.Facts{Status: c.Status, IntentAt: c.UpdatedAt, OldSessionID: c.OldSessionID}
	if sess, err := s.d.Sessions.GetActiveForProject(ctx, c.ProjectID); err == nil {
		facts.ActiveSession = &continuation.SessionFact{ID: sess.ID, Status: sess.Status, CreatedAt: sess.CreatedAt, WorkingDirectory: sess.WorkingDirectory}
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	var newID uuid.UUID
	if p := continuation.PlanRecovery(facts); p.Action == continuation.ActionAdoptSession {
		newID = *p.SessionID
	} else {
		rec, err := s.d.Worker.NewSession(ctx, c.ProjectID)
		if err != nil {
			settings, _, serr := s.d.Settings.Get(ctx, c.ProjectID)
			if serr != nil {
				return nil, serr
			}
			failures := c.SessionCreateFailures + 1
			m := persistence.ContinuationMutation{SessionCreateFailures: &failures}
			if failures > settings.SessionCreateRetries {
				return s.recovery(ctx, c, continuation.RecoverySessionCreateFailed, errMessage(err), m)
			}
			msg := errMessage(err)
			m.FailureMessage = &msg
			return s.advance(ctx, c, continuation.StatusResetRequired, "session creation failed; retrying", m)
		}
		newID = rec.ID
	}
	now := s.d.Now()
	return s.advance(ctx, c, continuation.StatusRestoringContext, "new session confirmed",
		persistence.ContinuationMutation{NewSessionID: &newID, SessionConfirmedAt: &now, SessionID: &newID})
}

// restoreContext is WBS 14.26/14.27: a new execution-context snapshot is
// reconstructed from persisted state only.
func (s *ContinuationService) restoreContext(ctx context.Context, c *continuation.Continuation) (*continuation.Continuation, error) {
	snap, err := s.ensureSnapshot(ctx, c)
	if errors.Is(err, ErrContinuationBinding) {
		return s.recovery(ctx, c, continuation.RecoveryBindingChanged, errMessage(err), persistence.ContinuationMutation{})
	}
	if err != nil {
		return nil, err
	}
	return s.advance(ctx, c, continuation.StatusRestoringCWD, "execution context restored", persistence.ContinuationMutation{SnapshotID: snap})
}

// restoreCWD is WBS 14.20/14.21/14.57/14.97: the confirmed session CWD is
// compared with the project directory; a change is issued only when needed
// and READY is persisted only after confirmation.
func (s *ContinuationService) restoreCWD(ctx context.Context, c *continuation.Continuation) (*continuation.Continuation, error) {
	ps, err := s.d.ProjectSettings.Get(ctx, c.ProjectID.String())
	if err != nil {
		return nil, err
	}
	desired := ps.WorkingDirectory
	sess, err := s.d.Sessions.GetActiveForProject(ctx, c.ProjectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return s.recovery(ctx, c, continuation.RecoveryCWDRestoreFailed, "no active worker session", persistence.ContinuationMutation{})
	} else if err != nil {
		return nil, err
	}
	if cs := c.ConfirmedSession(); cs != nil && *cs != sess.ID {
		return s.recovery(ctx, c, continuation.RecoveryCWDRestoreFailed, "the confirmed continuation session is no longer active", persistence.ContinuationMutation{})
	}
	if sess.WorkingDirectory == nil || *sess.WorkingDirectory != desired {
		rec, err := s.d.Worker.ChangeDirectory(ctx, c.ProjectID, desired)
		if err != nil {
			return s.recovery(ctx, c, continuation.RecoveryCWDRestoreFailed, errMessage(err), persistence.ContinuationMutation{})
		}
		if rec.WorkingDirectory == nil || *rec.WorkingDirectory != desired {
			return s.recovery(ctx, c, continuation.RecoveryCWDRestoreFailed, "worker confirmed a different directory", persistence.ContinuationMutation{})
		}
	}
	now := s.d.Now()
	return s.advance(ctx, c, continuation.StatusReady, "working directory confirmed", persistence.ContinuationMutation{CWDConfirmedAt: &now})
}

// prepare composes the CONTINUE_WORK_UNIT prompt through the normal
// Phase 8-10 pipeline (WBS 14.18, 14.28-14.34).
func (s *ContinuationService) prepare(ctx context.Context, c *continuation.Continuation) (*uuid.UUID, error) {
	if c.WorkerPromptID != nil {
		return c.WorkerPromptID, nil
	}
	if c.SnapshotID == nil {
		return nil, ErrInfrastructure
	}
	pid := c.ProjectID
	tmpl, err := s.d.Templates.GetByKey(ctx, &pid, s.d.ContinueTemplateKey)
	if errors.Is(err, persistence.ErrNotFound) {
		tmpl, err = s.d.Templates.GetByKey(ctx, nil, s.d.ContinueTemplateKey)
	}
	if err != nil {
		return nil, err
	}
	ver, err := s.d.TemplateVers.GetCurrentVersion(ctx, tmpl.ID)
	if err != nil {
		return nil, err
	}
	mode := ContinuationSameSession
	if c.Strategy == continuation.ContinueFreshSession {
		mode = ContinuationResetSession
	}
	in := c.Captured.PromptInput(c.WorkUnitID, c.CreatedAt)
	res, err := s.d.Cache.PrepareForContext(ctx, c.ProjectID, PrepareRequest{SnapshotID: *c.SnapshotID, TemplateID: tmpl.ID,
		TemplateVersion: ver.Version, Continuation: &in, ContinuationMode: mode})
	if err != nil {
		return nil, err
	}
	if res.WorkerPrompt == nil {
		return nil, ErrInfrastructure
	}
	id := res.WorkerPrompt.ID
	return &id, nil
}

// readiness gathers WBS 14.126 facts.
func (s *ContinuationService) readiness(ctx context.Context, c *continuation.Continuation, sess *persistence.WorkerSessionRecord, promptReady bool) error {
	in := continuation.ReadinessInput{ContinuationPhaseID: c.PhaseID, ContinuationWorkUnitID: c.WorkUnitID,
		ContinuationRuntimeID: c.RuntimeStateID, StatusCaptured: !c.Captured.Empty() || c.StatusUnknown, PromptReady: promptReady,
		ProjectID: c.ProjectID}
	if st, err := s.projectStatus(ctx, c.ProjectID); err == nil {
		in.ProjectStatus = st
	}
	if rt, err := s.d.Runtime.Get(ctx, c.ProjectID); err == nil {
		in.CurrentRuntimeID, in.CurrentPhaseID, in.CurrentWorkUnitID = rt.ID, rt.PhaseID, rt.WorkUnitID
	}
	if c.SnapshotID != nil {
		if snap, err := s.d.Snapshots.Get(ctx, *c.SnapshotID); err == nil && len(snap.RoleSnapshotJSON) > 2 {
			in.RoleSnapshotValid = true
		}
	}
	if sess != nil {
		in.SessionProjectID, in.SessionStatus, in.SessionCWD = sess.ProjectID, sess.Status, sess.WorkingDirectory
	}
	if ps, err := s.d.ProjectSettings.Get(ctx, c.ProjectID.String()); err == nil {
		in.DesiredCWD = ps.WorkingDirectory
	}
	if s.d.Results != nil {
		if st, _, err := s.d.Results.ProtocolSettings(ctx, c.ProjectID); err == nil && st.Validate() == nil {
			in.ProtocolValid = true
		}
	} else {
		in.ProtocolValid = true
	}
	if settings, _, err := s.d.Settings.Get(ctx, c.ProjectID); err == nil && continuation.ContinuationLimitReached(c.Attempt, settings) {
		in.LimitExceeded = continuation.RecoveryContinuationLimit
	}
	return continuation.ValidateReadiness(in)
}

// readyAndDispatch is WBS 14.18/14.35/14.58/14.59/14.95/14.126: session and
// CWD are re-checked, the prompt is prepared, all readiness problems are
// checked, DISPATCHING is persisted, then exactly one dispatch is sent.
func (s *ContinuationService) readyAndDispatch(ctx context.Context, c *continuation.Continuation) (*continuation.Continuation, error) {
	if c.OverrideStrategy == continuation.ContinueFreshSession && c.Strategy == continuation.ContinueSameSession {
		return s.toFresh(ctx, c, continuation.ReasonOperatorOverride)
	}
	sess, rec, err := s.d.Worker.ConfirmSession(ctx, c.ProjectID)
	if errors.Is(err, ErrNoActiveSession) || rec == worker.ReconcileLost || (sess != nil && sess.Status != worker.SessionActive) ||
		(sess != nil && c.ConfirmedSession() != nil && *c.ConfirmedSession() != sess.ID) {
		return s.toFresh(ctx, c, continuation.ReasonSessionLost)
	}
	if err != nil {
		return nil, err
	}
	promptID, err := s.prepare(ctx, c)
	if err != nil {
		return nil, err
	}
	if c.WorkerPromptID == nil {
		if c, err = s.d.Repo.Update(ctx, c.ID, c.Version, persistence.ContinuationMutation{WorkerPromptID: promptID}, s.d.Now()); err != nil {
			return nil, err
		}
	}
	if err := s.readiness(ctx, c, sess, true); err != nil {
		var re *continuation.ReadinessError
		if !errors.As(err, &re) {
			return nil, err
		}
		switch {
		case re.Has(continuation.ProblemProject):
			return nil, nil
		case re.Has(continuation.ProblemPhase), re.Has(continuation.ProblemUnit), re.Has(continuation.ProblemRuntime):
			return s.recovery(ctx, c, continuation.RecoveryBindingChanged, err.Error(), persistence.ContinuationMutation{})
		case re.Has(continuation.ProblemLimits):
			return s.recovery(ctx, c, continuation.RecoveryContinuationLimit, err.Error(), persistence.ContinuationMutation{})
		case re.Has(continuation.ProblemCWD) && len(re.Problems) == 1:
			return s.advance(ctx, c, continuation.StatusRestoringCWD, "working directory changed", persistence.ContinuationMutation{ClearCWD: true})
		}
		return s.recovery(ctx, c, continuation.RecoveryDispatchRejected, err.Error(), persistence.ContinuationMutation{})
	}
	c, err = s.advance(ctx, c, continuation.StatusDispatching, "dispatch intent", persistence.ContinuationMutation{})
	if err != nil {
		return nil, err
	}
	return s.dispatch(ctx, c)
}

const maxDispatchAttemptProbe = 64

// dispatch sends the continuation through Phase 12 (WBS 14.35): same unit,
// phase, runtime state and role as the source; the dispatch row carries
// the continuation id. Admission failures (nothing sent) return to READY.
func (s *ContinuationService) dispatch(ctx context.Context, c *continuation.Continuation) (*continuation.Continuation, error) {
	if c.WorkerPromptID == nil {
		return nil, ErrInfrastructure
	}
	ph, wu, cid := c.PhaseID, c.WorkUnitID, c.ID
	req := DispatchRequest{ProjectID: c.ProjectID, PhaseID: &ph, WorkUnitID: &wu, WorkerPromptID: *c.WorkerPromptID,
		RuntimeStateID: c.RuntimeStateID, StateKey: c.StateKey, RoleKey: c.RoleKey, ContinuationID: &cid}
	var derr error
	for attempt := c.Attempt + 1; attempt < c.Attempt+1+maxDispatchAttemptProbe; attempt++ {
		req.Attempt = attempt
		_, derr = s.d.Worker.Dispatch(ctx, req)
		if !errors.Is(derr, ErrDuplicateDispatch) {
			break
		}
	}
	if derr != nil {
		var we *worker.WorkerError
		if !errors.As(derr, &we) || we.Delivery == worker.DeliveryNotSent {
			// Admission failure: nothing left the overseer (dispatch rows
			// for the continuation are reconciled first).
			if latest, err := s.latestDispatch(ctx, c); err == nil && latest == nil {
				cur, err := s.d.Repo.Get(ctx, c.ProjectID, c.ID)
				if err != nil {
					return nil, err
				}
				if cur.Status == continuation.StatusDispatching {
					if _, err := s.advance(ctx, cur, continuation.StatusReady, "dispatch not admitted: "+errMessage(derr), persistence.ContinuationMutation{}); err != nil {
						return nil, err
					}
				}
				return nil, derr
			}
		}
	}
	return s.reconcileDispatch(ctx, c.ID, c.ProjectID)
}

func (s *ContinuationService) latestDispatch(ctx context.Context, c *continuation.Continuation) (*worker.WorkerDispatch, error) {
	id := c.ID
	ds, _, err := s.d.Dispatches.List(ctx, persistence.DispatchFilter{ProjectID: c.ProjectID, ContinuationID: &id, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		return nil, nil
	}
	return &ds[0], nil
}

// reconcileDispatch follows the continuation's Phase 12 dispatch (WBS
// 14.58/14.59/14.98/14.99): it never assumes a dispatch was not sent,
// never creates a second dispatch while one exists, and consumes the
// parsed result exactly once.
func (s *ContinuationService) reconcileDispatch(ctx context.Context, id, projectID uuid.UUID) (*continuation.Continuation, error) {
	c, err := s.d.Repo.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	d, err := s.latestDispatch(ctx, c)
	if err != nil {
		return nil, err
	}
	if c.Status != continuation.StatusDispatching && c.Status != continuation.StatusWaiting {
		if d != nil && c.Status.IsTerminal() && d.Status == worker.DispatchCompleted {
			// A cancelled continuation still records the consumption of
			// its in-flight dispatch's result — never a next continuation.
			if res, err := s.d.Results.ResultByDispatch(ctx, projectID, d.ID); err == nil {
				return s.consume(ctx, c, d, res)
			}
		}
		return c, nil
	}
	if d != nil && c.Status == continuation.StatusDispatching && d.CreatedAt.Before(c.UpdatedAt) {
		// A dispatch from before this DISPATCHING intent (an earlier,
		// definitely-not-sent or fresh-session-recovered attempt).
		d = nil
	}
	facts := continuation.Facts{Status: c.Status}
	var res *persistence.StoredWorkerResult
	if d != nil {
		df := &continuation.DispatchFact{ID: d.ID, Status: d.Status, Delivery: d.Delivery, ErrorCode: d.ErrorCode}
		if d.Status == worker.DispatchCompleted {
			if r, err := s.d.Results.ResultByDispatch(ctx, projectID, d.ID); err == nil {
				res, df.Parsed = r, true
			} else if s.d.Attempts != nil {
				did := d.ID
				if as, _, err := s.d.Attempts.List(ctx, persistence.ParseAttemptFilter{ProjectID: projectID, DispatchID: &did}); err == nil && len(as) > 0 {
					df.Rejected = true
				}
			}
		}
		facts.Dispatch = df
	}
	plan := continuation.PlanRecovery(facts)
	// The request was transmitted (or its outcome is known): record WAITING
	// with the dispatch before acting on the outcome.
	if c.Status == continuation.StatusDispatching && d != nil && d.Status != worker.DispatchPending &&
		(plan.Action == continuation.ActionWait || plan.Action == continuation.ActionConsume || plan.Action == continuation.ActionParse ||
			(plan.Action == continuation.ActionRecovery && d.Status != worker.DispatchFailed && d.Status != worker.DispatchCancelled)) {
		did := d.ID
		if c, err = s.advance(ctx, c, continuation.StatusWaiting, "dispatch transmitted", persistence.ContinuationMutation{DispatchID: &did}); err != nil {
			return nil, err
		}
	}
	switch plan.Action {
	case continuation.ActionAdmitDispatch:
		if c.Status == continuation.StatusDispatching {
			return s.dispatch(ctx, c)
		}
		return c, nil
	case continuation.ActionWait:
		return c, nil
	case continuation.ActionParse:
		out, err := s.d.Results.ParseDispatch(ctx, projectID, d.ID)
		if err != nil {
			return nil, err
		}
		if out.Result == nil {
			return s.reconcileDispatch(ctx, id, projectID)
		}
		cur, err := s.d.Repo.Get(ctx, projectID, id)
		if err != nil {
			return nil, err
		}
		return s.consume(ctx, cur, d, out.Result)
	case continuation.ActionConsume:
		return s.consume(ctx, c, d, res)
	case continuation.ActionRecovery:
		m := persistence.ContinuationMutation{}
		if d != nil && c.DispatchID == nil {
			did := d.ID
			m.DispatchID = &did
		}
		return s.recovery(ctx, c, plan.Recovery, "", m)
	case continuation.ActionFresh:
		return s.toFresh(ctx, c, plan.Reason)
	case continuation.ActionRetryDispatch:
		return s.advance(ctx, c, continuation.StatusReady, "dispatch not sent; retry", persistence.ContinuationMutation{ClearDispatch: true})
	case continuation.ActionFail:
		msg := "worker dispatch failed"
		if d != nil {
			msg += ": " + d.ErrorCode
		}
		return s.advance(ctx, c, continuation.StatusFailed, msg, persistence.ContinuationMutation{FailureMessage: &msg})
	}
	return c, nil
}
