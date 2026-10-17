package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/worker"
)

// WorkerRecoveryReport summarizes startup recovery (WBS 12.43/12.44).
type WorkerRecoveryReport struct {
	Sessions   map[uuid.UUID]worker.Reconciliation
	Dispatches map[uuid.UUID]worker.DispatchStatus
	Leases     int
}

// Recover runs at startup (P12-67). Expired leases are released; every
// orphaned in-flight dispatch (no live lease) is reclassified — PENDING →
// FAILED (definitely not sent), SENDING/WAITING → UNKNOWN (delivery
// unknown, requires recovery) — and never resent; every ACTIVE session is
// reconciled with its worker (CONFIRMED / LOST / UNKNOWN when the worker is
// unreachable). No session is ever created.
func (s *WorkerService) Recover(ctx context.Context) (WorkerRecoveryReport, error) {
	rep := WorkerRecoveryReport{Sessions: map[uuid.UUID]worker.Reconciliation{}, Dispatches: map[uuid.UUID]worker.DispatchStatus{}}
	now := s.d.Now()
	n, err := s.d.Leases.ReleaseExpired(ctx, now)
	if err != nil {
		return rep, translate(err)
	}
	rep.Leases = n
	inflight, err := s.d.Dispatches.ListInFlight(ctx, nil)
	if err != nil {
		return rep, translate(err)
	}
	for _, d := range inflight {
		if l, err := s.d.Leases.Get(ctx, d.SessionID); err == nil && l.DispatchID != nil && *l.DispatchID == d.ID && l.ExpiresAt.After(now) {
			continue // another live instance still owns it
		} else if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return rep, translate(err)
		}
		dd := d
		s.finishOrphan(ctx, &dd, "overseer restarted")
		rep.Dispatches[d.ID] = dd.Status
	}
	active, err := s.d.Sessions.ListActive(ctx)
	if err != nil {
		return rep, translate(err)
	}
	for i := range active {
		rec := &active[i]
		pw, err := s.worker(ctx, rec.ProjectID)
		if pw == nil || err != nil {
			rep.Sessions[rec.ID] = worker.ReconcileUnknown
			continue
		}
		res, err := s.reconcile(ctx, pw, rec)
		if err != nil {
			res = worker.ReconcileUnknown
		}
		rep.Sessions[rec.ID] = res
	}
	return rep, nil
}

// finishOrphan reclassifies a non-terminal dispatch that no live call owns
// (P12-67 / WBS 18.34): PENDING was never transmitted (FAILED, definitely
// not sent); SENDING / WAITING may have reached the worker (UNKNOWN,
// delivery unknown — never resent automatically).
func (s *WorkerService) finishOrphan(ctx context.Context, d *worker.WorkerDispatch, why string) {
	var we *worker.WorkerError
	if d.Status == worker.DispatchPending {
		we = worker.NewError(worker.CodeUnavailable, worker.DeliveryNotSent, why+" before the request was sent", nil)
	} else {
		we = worker.NewError(worker.CodeDeliveryUnknown, worker.DeliveryUnknown, why+" before a response was persisted", nil)
	}
	sess := &persistence.WorkerSessionRecord{ID: d.SessionID, ProjectID: d.ProjectID}
	s.finishFailed(ctx, d, d.Status, we, sess)
}

func (s *WorkerService) trackActive(id uuid.UUID, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		s.active = map[uuid.UUID]struct{}{}
	}
	if on {
		s.active[id] = struct{}{}
	} else {
		delete(s.active, id)
	}
}

// ReconcileOrphan reclassifies a non-terminal dispatch whose call is no
// longer running anywhere (WBS 18.34): this process is not executing it,
// and no other instance holds a live lease for it (no lease, an expired
// one, or one this instance owns — e.g. the database was lost after the
// send and neither the response nor the lease release could be
// recorded). Returns the reconciled dispatch, or nil when it may still be
// in flight (the caller keeps waiting).
func (s *WorkerService) ReconcileOrphan(ctx context.Context, d worker.WorkerDispatch) (*worker.WorkerDispatch, error) {
	if d.Status.Terminal() {
		return &d, nil
	}
	s.mu.Lock()
	_, running := s.active[d.ID]
	s.mu.Unlock()
	if running {
		return nil, nil
	}
	now := s.d.Now()
	l, err := s.d.Leases.Get(ctx, d.SessionID)
	switch {
	case err == nil && l.DispatchID != nil && *l.DispatchID == d.ID && l.ExpiresAt.After(now) && l.Owner != s.d.Owner:
		return nil, nil // another live instance owns the call
	case err != nil && !errors.Is(err, persistence.ErrNotFound):
		return nil, translate(err)
	}
	if err == nil && l.DispatchID != nil && *l.DispatchID == d.ID {
		_, _ = s.d.Leases.Release(ctx, d.SessionID, l.Owner)
	}
	s.finishOrphan(ctx, &d, "the dispatch call ended")
	if !d.Status.Terminal() {
		return nil, ErrInfrastructure
	}
	return &d, nil
}
