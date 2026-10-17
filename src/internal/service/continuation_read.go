package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/persistence"
	"overseer/internal/worker"
)

func (s *ContinuationService) get(ctx context.Context, projectID, id uuid.UUID) (*continuation.Continuation, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return nil, translate(err)
	}
	c, err := s.d.Repo.Get(ctx, projectID, id)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, ErrContinuationNotFound
	}
	return c, err
}

// --- queries (P14-62) ---------------------------------------------------------

// Continuation returns one continuation of the project.
func (s *ContinuationService) Continuation(ctx context.Context, projectID, id uuid.UUID) (*continuation.Continuation, error) {
	return s.get(ctx, projectID, id)
}

// Continuations lists the project's continuations (WBS 14.81).
func (s *ContinuationService) Continuations(ctx context.Context, f persistence.ContinuationFilter) ([]continuation.Continuation, int, error) {
	if _, err := s.d.Projects.Get(ctx, f.ProjectID.String()); err != nil {
		return nil, 0, translate(err)
	}
	return s.d.Repo.List(ctx, f)
}

// Current returns the project's active continuation (WBS 14.82);
// ErrContinuationNotFound when none is active.
func (s *ContinuationService) Current(ctx context.Context, projectID uuid.UUID) (*continuation.Continuation, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return nil, translate(err)
	}
	c, err := s.d.Repo.CurrentForProject(ctx, projectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, ErrContinuationNotFound
	}
	return c, err
}

// ContinuationHistory is the diagnostic history (WBS 14.83).
type ContinuationHistory struct {
	Continuation *continuation.Continuation
	Transitions  []persistence.ContinuationTransition
	// Attempts are the work unit's continuations in attempt order.
	Attempts []continuation.Continuation
	// Dispatches are the continuation's Phase 12 dispatches, oldest first.
	Dispatches []worker.WorkerDispatch
}

// History returns transitions (sequence order), the unit's attempts and
// the continuation's dispatches.
func (s *ContinuationService) History(ctx context.Context, projectID, id uuid.UUID) (*ContinuationHistory, error) {
	c, err := s.get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	tr, err := s.d.Repo.Transitions(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	att, err := s.d.Repo.ListByWorkUnit(ctx, projectID, c.WorkUnitID)
	if err != nil {
		return nil, err
	}
	cid := c.ID
	ds, _, err := s.d.Dispatches.List(ctx, persistence.DispatchFilter{ProjectID: projectID, ContinuationID: &cid, Limit: 500})
	if err != nil {
		return nil, translate(err)
	}
	for i, j := 0, len(ds)-1; i < j; i, j = i+1, j-1 {
		ds[i], ds[j] = ds[j], ds[i]
	}
	return &ContinuationHistory{Continuation: c, Transitions: tr, Attempts: att, Dispatches: ds}, nil
}

// State renders the derived CONTINUATION_STATE document (WBS 14.73).
func (s *ContinuationService) State(ctx context.Context, projectID, id uuid.UUID) (string, error) {
	c, err := s.get(ctx, projectID, id)
	if err != nil {
		return "", err
	}
	return continuation.RenderContinuationState(c), nil
}

// View builds the progress read model (WBS 14.77-14.80) from the persisted
// continuation, confirmed session statuses and the unit.
func (s *ContinuationService) View(ctx context.Context, c *continuation.Continuation) (continuation.ProgressView, error) {
	settings, _, err := s.d.Settings.Get(ctx, c.ProjectID)
	if err != nil {
		return continuation.ProgressView{}, err
	}
	in := continuation.ViewInput{Continuation: c, Settings: settings}
	if s.d.Units != nil {
		if u, err := s.d.Units.Get(ctx, c.WorkUnitID); err == nil {
			in.TaskLabel, in.TaskStatus = u.Name, string(u.Status)
		}
	}
	status := func(id *uuid.UUID) worker.SessionStatus {
		if id == nil {
			return ""
		}
		if rec, err := s.d.Sessions.Get(ctx, c.ProjectID, *id); err == nil {
			return rec.Status
		}
		return ""
	}
	in.OldSessionSt, in.NewSessionSt = status(c.OldSessionID), status(c.NewSessionID)
	return continuation.BuildProgressView(in), nil
}

// CurrentView is the current continuation plus its progress view.
func (s *ContinuationService) CurrentView(ctx context.Context, projectID uuid.UUID) (*continuation.Continuation, *continuation.ProgressView, error) {
	c, err := s.Current(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	v, err := s.View(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	return c, &v, nil
}

// Metrics derives continuation metrics from persisted rows (WBS 14.88).
func (s *ContinuationService) Metrics(ctx context.Context, projectID uuid.UUID) (continuation.Metrics, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return continuation.Metrics{}, translate(err)
	}
	rows, err := s.d.Repo.ListAll(ctx, projectID)
	if err != nil {
		return continuation.Metrics{}, err
	}
	return continuation.Aggregate(rows), nil
}

// --- manual operations (P14-60, P14-61) ---------------------------------------

// Retry creates a new continuation attempt for a RECOVERY_REQUIRED or
// FAILED continuation after the safety checks (WBS 14.84): a
// delivery-unknown continuation needs an explicit reconciliation
// acknowledgement and is never blindly resent. The work unit is unchanged.
func (s *ContinuationService) Retry(ctx context.Context, projectID, id uuid.UUID, reconciled bool) (*continuation.Continuation, error) {
	c, err := s.get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if err := continuation.CanRetry(c, reconciled); err != nil {
		return nil, errors.Join(ErrConflict, err)
	}
	if active, err := s.d.Repo.GetCurrentForWorkUnit(ctx, projectID, c.WorkUnitID); err == nil && active != nil {
		return nil, fmt.Errorf("%w: work unit already has an active continuation", ErrConflict)
	}
	src, err := s.d.Dispatches.Get(ctx, projectID, c.SourceDispatchID)
	if err != nil {
		return nil, translate(err)
	}
	var res *persistence.StoredWorkerResult
	if c.SourceResultID != nil {
		if res, err = s.d.Results.Result(ctx, projectID, *c.SourceResultID); err != nil {
			return nil, err
		}
	}
	id0 := c.ID
	n, err := s.newFromSource(ctx, src, continuation.TriggerManualRetry, res, c.Captured.Clone(), c.StatusUnknown, &id0)
	if errors.Is(err, ErrContinuationBinding) {
		return nil, errors.Join(ErrConflict, err)
	}
	if err != nil {
		return nil, err
	}
	// The operator restarts the progress run.
	n.NoProgressStreak = 1
	if err := s.d.Repo.Create(ctx, n, nil); err != nil {
		return nil, translate(err)
	}
	s.event(ctx, n, continuation.EventCreated, map[string]any{"trigger": string(n.Trigger), "retry_of": c.ID.String(),
		"reconciliation_acknowledged": reconciled})
	s.log(n, "continuation retry created", "retry_of", c.ID.String())
	s.Kick(projectID)
	return n, nil
}

// Override records an operator strategy override (WBS 14.85/14.86). A
// fresh session is only created when execution resumes; an unsafe
// same-session override is rejected without any dispatch.
func (s *ContinuationService) Override(ctx context.Context, projectID, id uuid.UUID, strategy continuation.Strategy) (*continuation.Continuation, error) {
	c, err := s.get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	sessStatus := worker.SessionStatus("")
	if sess, err := s.d.Sessions.GetActiveForProject(ctx, projectID); err == nil {
		sessStatus = sess.Status
	}
	if err := continuation.ValidateOverride(strategy, c, sessStatus); err != nil {
		if errors.Is(err, continuation.ErrInvalidStrategy) {
			return nil, errors.Join(ErrInvalidInput, err)
		}
		return nil, errors.Join(ErrConflict, err)
	}
	if strategy == continuation.ContinueSameSession && c.Status != continuation.StatusPending && c.Status != continuation.StatusDeciding {
		// A same-session override applies only before the decision.
		return nil, errors.Join(ErrConflict, continuation.ErrOverrideNotAllowed)
	}
	m := persistence.ContinuationMutation{OverrideStrategy: &strategy}
	var out *continuation.Continuation
	if strategy == continuation.ContinueFreshSession && c.Strategy == continuation.ContinueSameSession &&
		(c.Status == continuation.StatusSameSession || c.Status == continuation.StatusReady) {
		d := *c.Decision
		d.Strategy, d.Reason = continuation.ContinueFreshSession, continuation.ReasonOperatorOverride
		m.Decision, m.ClearDispatch = &d, true
		out, err = s.advance(ctx, c, continuation.StatusResetRequired, string(d.Reason), m)
	} else {
		// Applied when execution resumes (decision step, or READY for a
		// continuation still restoring its directory).
		out, err = s.d.Repo.Update(ctx, c.ID, c.Version, m, s.d.Now())
	}
	if err != nil {
		return nil, err
	}
	s.event(ctx, out, continuation.EventDecided, map[string]any{"override": string(strategy), "operator": true})
	s.log(out, "continuation override recorded", "override", string(strategy))
	s.Kick(projectID)
	return out, nil
}
