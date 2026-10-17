package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/faults"
	"overseer/internal/persistence"
	"overseer/internal/worker"
)

// DispatchRequest asks the worker service to send one prepared worker
// prompt (WBS 12.9/12.30). Attempt 0 means attempt 1.
type DispatchRequest struct {
	ProjectID      uuid.UUID
	PhaseID        *uuid.UUID
	WorkUnitID     *uuid.UUID
	WorkerPromptID uuid.UUID
	RuntimeStateID uuid.UUID
	StateKey       string
	RoleKey        string
	Attempt        int
	// ContinuationID links a Phase 14 continuation dispatch (WBS 14.35).
	ContinuationID *uuid.UUID
}

// DispatchResult is the outcome of one dispatch. Err carries the
// normalized worker error (with delivery state and retry class) when the
// dispatch did not complete; the dispatch row is final either way.
type DispatchResult struct {
	Dispatch worker.WorkerDispatch
	Response *worker.WorkerResponseRecord
	Usage    *persistence.WorkerUsageObservation
	Err      *worker.WorkerError
}

type admitted struct {
	d      worker.WorkerDispatch
	sess   *persistence.WorkerSessionRecord
	lease  time.Duration
	prompt worker.WorkerRequest
}

// Dispatch sends a worker prompt through the project's active session
// (WBS 12.28-12.37). Admission (validation, integrity, session lease,
// PENDING row) happens before anything is transmitted; the worker is
// called exactly once; the outcome (response + COMPLETED, or FAILED /
// CANCELLED / UNKNOWN by delivery state) is persisted even when ctx is
// cancelled. Nothing is retried or resent here.
func (s *WorkerService) Dispatch(ctx context.Context, req DispatchRequest) (*DispatchResult, error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "worker service shutting down", nil)
	}
	s.inflight.Add(1)
	s.mu.Unlock()
	defer s.inflight.Done()
	// The call is tracked before its row exists, so ReconcileOrphan can
	// never mistake a dispatch being admitted for an orphan.
	id := uuid.New()
	s.trackActive(id, true)
	defer s.trackActive(id, false)
	a, err := s.admit(ctx, req, id)
	if err != nil {
		return nil, err
	}
	res, err := s.execute(ctx, a)
	switch {
	case err != nil:
		s.recordAvailability(ctx, req.ProjectID, worker.AsWorkerError(err))
	case res != nil:
		s.recordAvailability(ctx, req.ProjectID, res.Err)
	}
	return res, err
}

// admit is P12-61: integrity check, project-scoped active session, session
// lease, then the PENDING dispatch row.
func (s *WorkerService) admit(ctx context.Context, req DispatchRequest, id uuid.UUID) (*admitted, error) {
	if req.ProjectID == uuid.Nil || req.WorkerPromptID == uuid.Nil || req.RuntimeStateID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	if req.Attempt < 0 {
		return nil, ErrInvalidInput
	}
	if req.Attempt == 0 {
		req.Attempt = 1
	}
	wp, err := s.d.Prompts.GetWorkerPrompt(ctx, req.WorkerPromptID)
	if err != nil {
		return nil, translate(err)
	}
	if err := worker.VerifyPromptIntegrity(*wp); err != nil {
		return nil, errors.Join(ErrPromptIntegrity, err)
	}
	sess, err := s.d.Sessions.GetActiveForProject(ctx, req.ProjectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, ErrNoActiveSession
	} else if err != nil {
		return nil, translate(err)
	}
	if err := s.checkDirectory(ctx, sess); err != nil {
		return nil, err
	}
	st, _, err := s.WorkerSettings(ctx, req.ProjectID)
	if err != nil {
		return nil, err
	}
	now := s.d.Now()
	d := worker.WorkerDispatch{ID: id, ProjectID: req.ProjectID, SessionID: sess.ID, PhaseID: req.PhaseID, WorkUnitID: req.WorkUnitID,
		WorkerPromptID: wp.ID, PromptHash: wp.Hash, RuntimeStateID: req.RuntimeStateID, StateKey: req.StateKey, RoleKey: req.RoleKey,
		RequestID: uuid.New(), OperationID: worker.LogicalOperationID(req.RuntimeStateID, req.WorkUnitID, wp.ID), Attempt: req.Attempt,
		Status: worker.DispatchPending, Owner: s.d.Owner, CreatedAt: now, ContinuationID: req.ContinuationID}
	if cfg, err := s.workerConfig(ctx, req.ProjectID); err == nil {
		d.WorkerModel = cfg.Model
	}
	if err := s.acquireLease(ctx, sess, d.ID, st); err != nil {
		return nil, err
	}
	if err := s.d.Dispatches.Create(ctx, &d); err != nil {
		_, _ = s.d.Leases.Release(context.WithoutCancel(ctx), sess.ID, s.d.Owner)
		if errors.Is(err, persistence.ErrConflict) {
			return nil, ErrDuplicateDispatch
		}
		return nil, translate(err)
	}
	wreq := worker.WorkerRequest{RequestID: d.RequestID, ProjectID: d.ProjectID, PhaseID: d.PhaseID, WorkUnitID: d.WorkUnitID, Prompt: *wp,
		Metadata: worker.WorkerRequestMetadata{RuntimeStateID: d.RuntimeStateID, StateKey: d.StateKey, RoleKey: d.RoleKey, PromptHash: wp.Hash, Attempt: d.Attempt}}
	return &admitted{d: d, sess: sess, lease: st.Lease, prompt: wreq}, nil
}

// checkDirectory refuses a dispatch whose session has no confirmed
// project directory when the worker supports directory control (WBS
// 12.62: dispatch requiring the project CWD does not proceed). Workers
// without the capability are unaffected (WBS 12.97).
func (s *WorkerService) checkDirectory(ctx context.Context, sess *persistence.WorkerSessionRecord) error {
	ps, err := s.d.ProjectSettings.Get(ctx, sess.ProjectID.String())
	if err != nil {
		return translate(err)
	}
	if ps.WorkingDirectory == "" {
		return nil
	}
	pw, _ := s.worker(ctx, sess.ProjectID)
	if pw == nil {
		return nil
	}
	caps, err := pw.w.Capabilities(ctx)
	if err != nil || !caps.ChangeDirectory {
		return nil
	}
	if sess.WorkingDirectory == nil || *sess.WorkingDirectory != ps.WorkingDirectory {
		return ErrWorkingDirectoryNotReady
	}
	return nil
}

// acquireLease takes the session lease (WBS 12.57): REJECT fails at once
// when held; WAIT polls until the configured wait elapses.
func (s *WorkerService) acquireLease(ctx context.Context, sess *persistence.WorkerSessionRecord, dispatchID uuid.UUID, st worker.Settings) error {
	deadline := s.d.Now().Add(st.Wait)
	for {
		now := s.d.Now()
		ok, err := s.d.Leases.Acquire(ctx, persistence.WorkerSessionLease{SessionID: sess.ID, ProjectID: sess.ProjectID, Owner: s.d.Owner,
			DispatchID: &dispatchID, AcquiredAt: now, ExpiresAt: now.Add(st.Lease)})
		if err != nil {
			return translate(err)
		}
		if ok {
			return nil
		}
		if st.ConcurrentSend != worker.SendWait || !now.Before(deadline) {
			return ErrSessionBusy
		}
		select {
		case <-ctx.Done():
			return ErrSessionBusy
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// heartbeat renews the lease while a request is in flight (P12-63): a
// silent long request keeps its ownership; lost tracks a failed renewal.
func (s *WorkerService) heartbeat(sessionID uuid.UUID, lease time.Duration) (stop func() bool) {
	done := make(chan struct{})
	lost := make(chan bool, 1)
	go func() {
		t := time.NewTicker(lease / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				lost <- false
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), lease/3)
				ok, err := s.d.Leases.Renew(ctx, sessionID, s.d.Owner, s.d.Now().Add(lease))
				cancel()
				if err == nil && !ok {
					<-done
					lost <- true
					return
				}
			}
		}
	}()
	return func() bool { close(done); return <-lost }
}

// execute is P12-62: SENDING → WAITING → one Send → final status.
func (s *WorkerService) execute(ctx context.Context, a *admitted) (*DispatchResult, error) {
	bg := context.WithoutCancel(ctx)
	d := a.d
	res := &DispatchResult{}
	release := func() { _, _ = s.d.Leases.Release(bg, a.sess.ID, s.d.Owner) }
	fail := func(we *worker.WorkerError, from worker.DispatchStatus) (*DispatchResult, error) {
		s.finishFailed(bg, &d, from, we, a.sess)
		release()
		res.Dispatch, res.Err = d, we
		return res, nil
	}
	pw, err := s.worker(ctx, d.ProjectID)
	if pw == nil {
		we := worker.AsWorkerError(err)
		if !errors.As(err, new(*worker.WorkerError)) {
			we = worker.NewError(worker.CodeUnavailable, worker.DeliveryNotSent, err.Error(), err)
		}
		return fail(we, worker.DispatchPending)
	}
	if err != nil {
		return fail(worker.AsWorkerError(err), worker.DispatchPending)
	}
	if err := s.ensureSession(ctx, pw, a.sess); err != nil {
		return fail(worker.AsWorkerError(err), worker.DispatchPending)
	}
	started := s.d.Now()
	if ok, err := s.d.Dispatches.Transition(bg, d.ID, worker.DispatchPending, worker.DispatchSending, persistence.DispatchUpdate{StartedAt: &started, At: started}); err != nil || !ok {
		release()
		if err == nil {
			err = ErrConflict
		}
		return nil, translate(err)
	}
	d.Status, d.StartedAt = worker.DispatchSending, &started
	if ok, err := s.d.Dispatches.Transition(bg, d.ID, worker.DispatchSending, worker.DispatchWaiting, persistence.DispatchUpdate{At: s.d.Now()}); err != nil || !ok {
		// Not transmitted yet: the dispatch is definitely not sent.
		return fail(worker.NewError(worker.CodeInternal, worker.DeliveryNotSent, "dispatch could not be marked waiting", err), worker.DispatchSending)
	}
	d.Status = worker.DispatchWaiting
	id, sid := d.ID, a.sess.ID
	s.event(bg, d.ProjectID, &sid, &id, worker.EventRequestStarted, map[string]any{"attempt": d.Attempt, "state_key": d.StateKey,
		"role_key": d.RoleKey, "prompt_hash": d.PromptHash, "request_id": d.RequestID.String()})
	if s.d.Activity != nil {
		s.d.Activity.NotifyWorkerDispatched(bg, d.ProjectID)
		defer s.d.Activity.NotifyWorkerFinished(d.ProjectID)
	}
	// Fault point (WBS 18.143): the request has not left the overseer —
	// an injected failure is a definite not-sent; a simulated crash leaves
	// the row WAITING for restart reconciliation.
	if ferr := faults.Hit(ctx, faults.BeforeWorkerSend); ferr != nil {
		if errors.Is(ferr, faults.ErrCrash) {
			release()
			return nil, ferr
		}
		return fail(worker.NewError(worker.CodeUnavailable, worker.DeliveryNotSent, "send failed before transmission: "+ferr.Error(), ferr), worker.DispatchWaiting)
	}
	stop := s.heartbeat(a.sess.ID, a.lease)
	resp, tools, sendErr := s.send(ctx, pw, a.prompt)
	if sendErr == nil {
		// Fault point (WBS 18.144): transmitted, then the connection is
		// lost before the response arrives — delivery unknown.
		if ferr := faults.Hit(ctx, faults.AfterWorkerSend); ferr != nil {
			if errors.Is(ferr, faults.ErrCrash) {
				_ = stop()
				release()
				return nil, ferr
			}
			sendErr = worker.NewError(worker.CodeDeliveryUnknown, worker.DeliveryUnknown, "connection lost after transmission: "+ferr.Error(), ferr)
		}
	}
	s.recordTools(bg, d, tools)
	if lost := stop(); lost {
		s.d.Logger.WarnContext(ctx, "worker session lease lost during dispatch", "dispatch_id", d.ID.String(), "session_id", a.sess.ID.String())
	}
	if sendErr != nil && s.shuttingDown.Load() {
		// Our own shutdown cut the call short (WBS 12.67 / 18.126): the
		// outcome is unknown to us — the dispatch is finalized as
		// DELIVERY_UNKNOWN (never FAILED / CANCELLED) and reconciled
		// through the delivery-unknown recovery; it is never resent blindly.
		we := worker.NewError(worker.CodeDeliveryUnknown, worker.DeliveryUnknown, "worker call interrupted by overseer shutdown", sendErr)
		s.d.Logger.WarnContext(ctx, "worker call interrupted by shutdown; delivery unknown", "dispatch_id", d.ID.String(), "error", sendErr.Error())
		return fail(we, worker.DispatchWaiting)
	}
	if sendErr != nil {
		we := worker.AsWorkerError(sendErr)
		pw.recordErr(we, s.d.Now())
		r, err := fail(we, worker.DispatchWaiting)
		s.logDispatch(ctx, r.Dispatch, nil, we)
		return r, err
	}
	now := s.d.Now()
	if resp.RequestID != d.RequestID {
		// WBS 12.32.2: never attach a response to the wrong dispatch.
		we := worker.NewError(worker.CodeProtocol, worker.DeliveryReceived, "response correlation id does not match the dispatch", nil)
		return fail(we, worker.DispatchWaiting)
	}
	rec := worker.NewResponseRecord(d, resp, now)
	delivered := worker.DeliveryReceived
	// Fault point (WBS 18.145): the response arrived but cannot be
	// persisted — the row stays WAITING; nothing is fabricated.
	if ferr := faults.Hit(ctx, faults.AfterWorkerResponse); ferr != nil {
		release()
		s.d.Logger.ErrorContext(ctx, "worker response not persisted", "dispatch_id", d.ID.String(), "error", ferr.Error())
		return nil, translate(ferr)
	}
	ok, err := s.d.Responses.CompleteWithResponse(bg, &rec, persistence.DispatchUpdate{Delivery: &delivered, CompletedAt: &now, At: now})
	release()
	if err != nil || !ok {
		// The worker answered but the outcome could not be stored: the row
		// stays WAITING and restart recovery marks it UNKNOWN.
		s.d.Logger.ErrorContext(ctx, "worker response not persisted", "dispatch_id", d.ID.String(), "error", errString(err))
		if err == nil {
			err = ErrConflict
		}
		return nil, translate(err)
	}
	d.Status, d.Delivery, d.CompletedAt = worker.DispatchCompleted, &delivered, &now
	pw.recordOK(now)
	res.Dispatch, res.Response = d, &rec
	res.Usage = s.observeUsage(bg, a, resp)
	_ = s.d.Sessions.UpdateLastSeen(bg, d.ProjectID, a.sess.ID, now)
	s.event(bg, d.ProjectID, &sid, &id, worker.EventRequestCompleted, map[string]any{"attempt": d.Attempt, "content_hash": rec.ContentHash,
		"content_bytes": len(rec.Content), "duration_ms": now.Sub(started).Milliseconds()})
	s.logDispatch(ctx, d, res.Usage, nil)
	if s.completion != nil {
		s.completion.OnDispatchCompleted(bg, d.ProjectID, d.ID)
	}
	return res, nil
}

// send transmits the prompt once: streamed when the worker advertises
// streaming (tool activity captured, WBS 12.45-12.47), otherwise Send.
// The assembled streamed response is what gets persisted (WBS 12.45.3).
func (s *WorkerService) send(ctx context.Context, pw *projectWorker, req worker.WorkerRequest) (worker.WorkerResponse, []worker.ToolActivity, error) {
	sw, ok := pw.w.(worker.StreamingWorker)
	if caps, err := pw.w.Capabilities(ctx); !ok || err != nil || !caps.Streaming {
		resp, err := pw.w.Send(ctx, req)
		return resp, nil, err
	}
	st, err := sw.Stream(ctx, req)
	if err != nil {
		return worker.WorkerResponse{}, nil, err
	}
	defer st.Close()
	var asm worker.StreamAssembler
	var tools []worker.ToolActivity
	for {
		ev, err := st.Next(ctx)
		if err != nil {
			return worker.WorkerResponse{}, tools, err
		}
		if ev.Tool != nil {
			tools = append(tools, *ev.Tool)
		}
		if err := asm.Add(ev); err != nil {
			return worker.WorkerResponse{}, tools, worker.NewError(worker.CodeProtocol, worker.DeliveryReceived, err.Error(), err)
		}
		if ev.Kind == worker.StreamCompleted || ev.Kind == worker.StreamError {
			break
		}
	}
	resp, _, err := asm.Result()
	return resp, tools, err
}

// recordTools persists non-authoritative tool telemetry; it never drives
// work-unit status (WBS 12.47.3).
func (s *WorkerService) recordTools(ctx context.Context, d worker.WorkerDispatch, tools []worker.ToolActivity) {
	if s.d.Events == nil {
		return
	}
	for _, t := range tools {
		rec := &persistence.WorkerToolEventRecord{ID: uuid.New(), ProjectID: d.ProjectID, DispatchID: d.ID, Activity: t, CreatedAt: s.d.Now()}
		if err := s.d.Events.AppendTool(ctx, rec); err != nil {
			s.d.Logger.WarnContext(ctx, "worker tool activity not recorded", "dispatch_id", d.ID.String(), "error", err.Error())
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// finishFailed records a non-completed outcome by delivery state (PLAN
// Decision 5) and the matching audit event.
func (s *WorkerService) finishFailed(ctx context.Context, d *worker.WorkerDispatch, from worker.DispatchStatus, we *worker.WorkerError, sess *persistence.WorkerSessionRecord) {
	to := worker.OutcomeFor(we)
	if from == worker.DispatchPending && to == worker.DispatchUnknown {
		// Nothing left the overseer before SENDING.
		to, we.Delivery = worker.DispatchFailed, worker.DeliveryNotSent
	}
	now := s.d.Now()
	rc := we.RetryClass()
	u := persistence.DispatchUpdate{Delivery: &we.Delivery, RetryClass: &rc, ErrorCode: string(we.Code), ErrorMessage: we.Message,
		RetryAfter: we.RetryAfter, Diagnostic: we.Diagnostic, CompletedAt: &now, At: now}
	if we.ProviderCode != nil {
		u.ProviderCode = *we.ProviderCode
	}
	if ok, err := s.d.Dispatches.Transition(ctx, d.ID, from, to, u); err != nil || !ok {
		s.d.Logger.ErrorContext(ctx, "worker dispatch outcome not persisted", "dispatch_id", d.ID.String(), "status", string(to), "error", errString(err))
	} else {
		d.Status, d.Delivery, d.RetryClass, d.ErrorCode, d.ErrorMessage, d.CompletedAt = to, &we.Delivery, &rc, string(we.Code), we.Message, &now
		d.RetryAfter = we.RetryAfter
	}
	if we.Code == worker.CodeSessionLost {
		s.markLost(ctx, sess, "provider reports the session no longer exists")
	}
	typ := worker.EventRequestFailed
	switch to {
	case worker.DispatchUnknown:
		typ = worker.EventDeliveryUnknown
	case worker.DispatchCancelled:
		typ = worker.EventRequestCancelled
	}
	id, sid := d.ID, d.SessionID
	p := map[string]any{"code": string(we.Code), "delivery": string(we.Delivery), "retry_class": string(rc), "attempt": d.Attempt}
	if we.RetryAfter != nil {
		p["retry_after_ms"] = we.RetryAfter.Milliseconds()
	}
	s.event(ctx, d.ProjectID, &sid, &id, typ, p)
	if s.failure != nil {
		s.failure.OnDispatchFailed(context.WithoutCancel(ctx), *d)
	}
}

// observeUsage is P12-64: one observation per completed dispatch using the
// telemetry precedence; the worker-reported context is preferred, an
// estimate is labelled as such. No decision is taken on the value.
func (s *WorkerService) observeUsage(ctx context.Context, a *admitted, resp worker.WorkerResponse) *persistence.WorkerUsageObservation {
	var actual, provider *worker.WorkerUsage
	if resp.Usage.HasContext() && !resp.Usage.Estimated {
		u := resp.Usage
		actual = &u
	} else if resp.Usage.InputTokens != nil && !resp.Usage.Estimated {
		u := resp.Usage
		provider = &u
	}
	window := int64(0)
	if pw := s.existing(a.d.ProjectID); pw != nil {
		window = int64(pw.cfg.ContextWindow)
	}
	est := &worker.WorkerUsage{InputTokens: worker.Int64(int64(a.prompt.Prompt.EstimatedTokens)), OutputTokens: worker.Int64(int64(len(resp.Content) / 4))}
	est.ContextTokens = worker.Int64(*est.InputTokens + *est.OutputTokens)
	if window > 0 {
		est.ContextWindow = worker.Int64(window)
	}
	u, src := worker.SelectUsage(actual, provider, est)
	if u.ContextWindow == nil && window > 0 {
		u.ContextWindow = worker.Int64(window)
	}
	id := a.d.ID
	o := &persistence.WorkerUsageObservation{ID: uuid.New(), ProjectID: a.d.ProjectID, SessionID: a.sess.ID, DispatchID: &id, Usage: u, Source: src, ObservedAt: s.d.Now()}
	if err := s.d.Responses.InsertObservation(ctx, o); err != nil {
		s.d.Logger.WarnContext(ctx, "worker usage not recorded", "dispatch_id", id.String(), "error", err.Error())
		return nil
	}
	_ = s.d.Sessions.UpdateUsage(ctx, a.d.ProjectID, a.sess.ID, u)
	p := map[string]any{"source": string(src), "estimated": u.Estimated}
	if ut, err := u.Utilization(); err == nil && ut.Known {
		p["context_tokens"], p["context_window"], p["percentage"] = ut.Tokens, ut.Window, ut.Percent
	}
	sid := a.sess.ID
	s.event(ctx, a.d.ProjectID, &sid, &id, worker.EventContextObserved, p)
	return o
}

// logDispatch is P12-66: correlation fields only — never prompt or
// response content, never credentials.
func (s *WorkerService) logDispatch(ctx context.Context, d worker.WorkerDispatch, o *persistence.WorkerUsageObservation, we *worker.WorkerError) {
	args := []any{"component", "worker", "project_id", d.ProjectID.String(), "dispatch_id", d.ID.String(), "session_id", d.SessionID.String(),
		"runtime_state_id", d.RuntimeStateID.String(), "request_id", d.RequestID.String(), "state", d.StateKey, "role", d.RoleKey,
		"attempt", d.Attempt, "result", string(d.Status)}
	if pw := s.existing(d.ProjectID); pw != nil {
		args = append(args, "provider", string(pw.provider))
	}
	if d.PhaseID != nil {
		args = append(args, "phase_id", d.PhaseID.String())
	}
	if d.WorkUnitID != nil {
		args = append(args, "work_unit_id", d.WorkUnitID.String())
	}
	if d.StartedAt != nil && d.CompletedAt != nil {
		args = append(args, "duration_ms", d.CompletedAt.Sub(*d.StartedAt).Milliseconds())
	}
	if o != nil {
		if o.Usage.InputTokens != nil {
			args = append(args, "input_tokens", *o.Usage.InputTokens)
		}
		if o.Usage.OutputTokens != nil {
			args = append(args, "output_tokens", *o.Usage.OutputTokens)
		}
		if o.Usage.ContextTokens != nil {
			args = append(args, "context_tokens", *o.Usage.ContextTokens)
		}
		args = append(args, "usage_source", string(o.Source))
	}
	if we != nil {
		args = append(args, "error_code", string(we.Code), "delivery", string(we.Delivery))
		s.d.Logger.WarnContext(ctx, "worker dispatch finished", args...)
		return
	}
	s.d.Logger.InfoContext(ctx, "worker dispatch finished", args...)
}

// Dispatches lists a project's dispatches (WBS 12.83).
func (s *WorkerService) Dispatches(ctx context.Context, f persistence.DispatchFilter) ([]worker.WorkerDispatch, int, error) {
	out, total, err := s.d.Dispatches.List(ctx, f)
	if err != nil {
		return nil, 0, translate(err)
	}
	return out, total, nil
}

// DispatchDetail is a dispatch with its attempt history and raw response
// reference.
type DispatchDetail struct {
	Dispatch worker.WorkerDispatch
	Attempts []worker.WorkerDispatch
	Response *worker.WorkerResponseRecord
	Tools    []persistence.WorkerToolEventRecord
}

// GetDispatch returns one dispatch with its attempt history (WBS 12.83).
func (s *WorkerService) GetDispatch(ctx context.Context, projectID, id uuid.UUID) (*DispatchDetail, error) {
	d, err := s.d.Dispatches.Get(ctx, projectID, id)
	if err != nil {
		return nil, translate(err)
	}
	att, err := s.d.Dispatches.ListAttempts(ctx, projectID, d.OperationID)
	if err != nil {
		return nil, translate(err)
	}
	out := &DispatchDetail{Dispatch: *d, Attempts: att}
	if r, err := s.d.Responses.GetByDispatch(ctx, projectID, id); err == nil {
		out.Response = r
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	if s.d.Events != nil {
		if t, err := s.d.Events.ListTools(ctx, projectID, id); err == nil {
			out.Tools = t
		}
	}
	return out, nil
}

// DispatchPrompt returns the worker prompt a dispatch sent.
func (s *WorkerService) DispatchPrompt(ctx context.Context, projectID, dispatchID uuid.UUID) (*compression.WorkerPrompt, error) {
	d, err := s.d.Dispatches.Get(ctx, projectID, dispatchID)
	if err != nil {
		return nil, translate(err)
	}
	wp, err := s.d.Prompts.GetWorkerPrompt(ctx, d.WorkerPromptID)
	if err != nil {
		return nil, translate(err)
	}
	return wp, nil
}

// Response returns the raw response of a dispatch for Phase 13 (WBS
// 12.70: "raw response available for Phase 13").
func (s *WorkerService) Response(ctx context.Context, projectID, dispatchID uuid.UUID) (*worker.WorkerResponseRecord, error) {
	r, err := s.d.Responses.GetByDispatch(ctx, projectID, dispatchID)
	if err != nil {
		return nil, translate(err)
	}
	return r, nil
}
