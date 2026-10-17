package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
)

// LifecycleService applies project lifecycle transitions (WBS 3.6) and
// records the audit event (WBS 3.7). Transition validation is pure domain
// logic (project.CanTransitionTo); a rejected transition writes nothing.
type LifecycleService struct {
	projects persistence.ProjectRepository
	events   persistence.StatusEventRepository
	txPool   *pgxpool.Pool
	// hooks observe committed status changes (Phase 11: pause / cancel stop
	// precompute jobs). Hooks never fail the transition.
	hooks []func(ctx context.Context, projectID uuid.UUID, status project.ProjectStatus)
}

// WithStatusHook registers an observer of committed project status changes.
func (s *LifecycleService) WithStatusHook(fn func(ctx context.Context, projectID uuid.UUID, status project.ProjectStatus)) *LifecycleService {
	s.hooks = append(s.hooks, fn)
	return s
}

func (s *LifecycleService) notify(ctx context.Context, p *project.Project) {
	if p == nil {
		return
	}
	for _, h := range s.hooks {
		h(context.WithoutCancel(ctx), p.ID, p.Status)
	}
}

// NewLifecycleService wires the lifecycle service to the project and
// status-event repositories and the transaction pool.
func NewLifecycleService(
	projects persistence.ProjectRepository,
	events persistence.StatusEventRepository,
	txPool *pgxpool.Pool,
) *LifecycleService {
	return &LifecycleService{projects: projects, events: events, txPool: txPool}
}

// Transition moves the project to target if the state machine allows it
// and records the transition event atomically with the status update
// (WBS 3.6, 3.7). reason is the caller-supplied audit context. An invalid
// transition or a missing project is reported without any write
// (T3.6.12, T3.7.5).
func (s *LifecycleService) Transition(ctx context.Context, projectID, reason string, target project.ProjectStatus) (*project.Project, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := project.ParseStatus(target.String()); err != nil {
		return nil, ErrInvalidInput
	}
	// WBS 3.25: the read-validate-write cycle must be serialized per
	// project so two simultaneous requests cannot both observe the same
	// status and both write. Under the transaction seam this happens inside
	// one transaction protected by a per-project advisory lock; the bounded
	// retry recomputes on a conflict (belt-and-braces on the lock).
	return s.transitionCore(ctx, parsed, reason, target, func(p *project.Project) (bool, error) {
		// A state-machine rejection is an invalid operation (400), not an
		// idempotent no-op — return the error, not (false, nil).
		if !project.CanTransitionTo(p.Status, target) {
			return false, ErrInvalidInput
		}
		return true, nil
	})
}

// TransitionGuarded is Transition with an operation-specific guard evaluated
// INSIDE the serialized transition transaction (WBS 3.25). The guard sees the
// fresh project read taken under the per-project advisory lock, so concurrent
// requests cannot both observe the same status and both proceed: guard
// returning (true, nil) commits the transition; (false, nil) is an idempotent
// no-op (nil error, no event, the current project is returned); an error
// rejects the operation before any write.
func (s *LifecycleService) TransitionGuarded(ctx context.Context, projectID, reason string, target project.ProjectStatus, guard func(p *project.Project) (bool, error)) (*project.Project, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := project.ParseStatus(target.String()); err != nil {
		return nil, ErrInvalidInput
	}
	return s.transitionCore(ctx, parsed, reason, target, guard)
}

// transitionCore is the shared read-validate-write core (WBS 3.6/3.7/3.25).
// Under the transaction pool the whole cycle — fresh read, guard evaluation,
// state-machine check, status write, event record — runs in one transaction
// serialized by a per-project advisory lock, so two simultaneous requests
// produce a valid serialized sequence (exactly one committed transition, no
// corrupted state). Without a pool (unit scope) it degrades to sequential
// direct repository calls.
func (s *LifecycleService) transitionCore(ctx context.Context, parsed uuid.UUID, reason string, target project.ProjectStatus, guard func(p *project.Project) (bool, error)) (*project.Project, error) {
	attempt := func(pr persistence.ProjectRepository, ev persistence.StatusEventRepository) (*project.Project, error) {
		p, err := pr.Get(ctx, parsed.String())
		if err != nil {
			return nil, translate(err)
		}
		ok, gerr := guard(p)
		if gerr != nil {
			return nil, gerr
		}
		if !ok {
			// Idempotent no-op (e.g. pause on PAUSED): no event, no write.
			return p, nil
		}
		if !project.CanTransitionTo(p.Status, target) {
			return nil, ErrInvalidInput
		}
		event := &persistence.StatusEvent{
			ID:         uuid.New(),
			ProjectID:  parsed,
			FromStatus: p.Status.String(),
			ToStatus:   target.String(),
			Reason:     reason,
			CreatedAt:  time.Now().UTC(),
		}
		p.Status = target
		if err := pr.Update(ctx, p); err != nil {
			return nil, translate(err)
		}
		if err := ev.Record(ctx, event); err != nil {
			return nil, translate(err)
		}
		return p, nil
	}
	if s.txPool == nil {
		// Unit-scope fallback: sequential calls, no per-project lock
		// (single-goroutine test scope). Production always injects a pool.
		p, err := attempt(s.projects, s.events)
		if err == nil {
			s.notify(ctx, p)
		}
		return p, err
	}
	var out *project.Project
	err := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		// Per-project serialization (WBS 3.25): same-project transitions
		// queue on the advisory lock; other projects are unaffected.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", parsed); err != nil {
			return translate(err)
		}
		var aerr error
		out, aerr = attempt(s.projects.WithTx(tx), s.events.WithTx(tx))
		return aerr
	})
	if err != nil {
		return nil, err
	}
	s.notify(ctx, out)
	return out, nil
}

// Start moves a READY project to RUNNING (WBS 3.8). In this phase start is
// lifecycle-only: it changes project status and records the "start" event.
// It does not invoke the planner, call Hermes, or begin autonomous
// execution — later orchestration observes RUNNING and starts from there
// (T3.8.1, T3.8.5, T3.8.6). The READY-only guard runs inside the serialized
// transition (WBS 3.25).
func (s *LifecycleService) Start(ctx context.Context, projectID string) (*project.Project, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	// Fast 404 path; the authoritative status guard runs inside the
	// serialized transition (WBS 3.25).
	if _, err := s.projects.Get(ctx, parsed.String()); err != nil {
		return nil, translate(err)
	}
	return s.TransitionGuarded(ctx, projectID, "start", project.StatusRUNNING, func(p *project.Project) (bool, error) {
		// The state machine allows PAUSED -> RUNNING (T3.6.4), but a PAUSED
		// project resumes through Resume, not Start (WBS 3.8, T3.8.3).
		if p.Status != project.StatusREADY {
			return false, ErrInvalidInput
		}
		return true, nil
	})
}

// Pause moves a RUNNING project to PAUSED (WBS 3.9). There is no active
// worker to interrupt in this phase; later orchestration uses PAUSED to
// stop dispatching new work (T3.9.1). A project that is already PAUSED is
// accepted as an idempotent no-op (T3.9.4): no error, no event, no
// timestamp change. Any other status is rejected.
func (s *LifecycleService) Pause(ctx context.Context, projectID string) (*project.Project, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := s.projects.Get(ctx, parsed.String()); err != nil {
		return nil, translate(err)
	}
	return s.TransitionGuarded(ctx, projectID, "pause", project.StatusPAUSED, func(p *project.Project) (bool, error) {
		if p.Status == project.StatusPAUSED {
			// Idempotent no-op (T3.9.4): no error, no event, no write.
			return false, nil
		}
		if p.Status != project.StatusRUNNING {
			return false, ErrInvalidInput
		}
		return true, nil
	})
}

// Resume moves a PAUSED project back to RUNNING (WBS 3.10). Only PAUSED
// projects resume; every other status is rejected. The event reason is
// "resume" so the audit trail distinguishes resume from start (T3.10.1).
func (s *LifecycleService) Resume(ctx context.Context, projectID string) (*project.Project, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := s.projects.Get(ctx, parsed.String()); err != nil {
		return nil, translate(err)
	}
	return s.TransitionGuarded(ctx, projectID, "resume", project.StatusRUNNING, func(p *project.Project) (bool, error) {
		if p.Status != project.StatusPAUSED {
			return false, ErrInvalidInput
		}
		return true, nil
	})
}

// Cancel moves a RUNNING or PAUSED project to CANCELLED (WBS 3.11).
// Cancellation is a terminal lifecycle state; it deletes nothing — WBS
// documents, execution plans, completed work units, and other historical
// data are preserved (T3.11.3–T3.11.5). Any other status is rejected.
func (s *LifecycleService) Cancel(ctx context.Context, projectID string) (*project.Project, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := s.projects.Get(ctx, parsed.String()); err != nil {
		return nil, translate(err)
	}
	return s.TransitionGuarded(ctx, projectID, "cancel", project.StatusCANCELLED, func(p *project.Project) (bool, error) {
		if p.Status == project.StatusRUNNING || p.Status == project.StatusPAUSED {
			return true, nil
		}
		return false, ErrInvalidInput
	})
}

// TransitionTx performs the guarded lifecycle transition inside the
// caller's transaction (P15-41, WBS 15.155): the status write and status
// event commit or roll back with the caller's other writes. Hooks are NOT
// fired here — the returned notify function must be called after the
// caller commits (a rolled-back transition notifies nobody).
func (s *LifecycleService) TransitionTx(ctx context.Context, tx persistence.Transaction, projectID uuid.UUID, reason string, target project.ProjectStatus, guard func(p *project.Project) (bool, error)) (*project.Project, func(), error) {
	if tx == nil {
		return nil, nil, ErrInfrastructure
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", projectID); err != nil {
		return nil, nil, translate(err)
	}
	pr, ev := s.projects.WithTx(tx), s.events.WithTx(tx)
	p, err := pr.Get(ctx, projectID.String())
	if err != nil {
		return nil, nil, translate(err)
	}
	if guard != nil {
		ok, gerr := guard(p)
		if gerr != nil {
			return nil, nil, gerr
		}
		if !ok {
			return p, func() {}, nil
		}
	}
	if !project.CanTransitionTo(p.Status, target) {
		return nil, nil, ErrInvalidInput
	}
	event := &persistence.StatusEvent{
		ID: uuid.New(), ProjectID: projectID, FromStatus: p.Status.String(), ToStatus: target.String(),
		Reason: reason, CreatedAt: time.Now().UTC(),
	}
	p.Status = target
	if err := pr.Update(ctx, p); err != nil {
		return nil, nil, translate(err)
	}
	if err := ev.Record(ctx, event); err != nil {
		return nil, nil, translate(err)
	}
	out := *p
	return p, func() { s.notify(ctx, &out) }, nil
}
