// P6-21 (WBS 6.27/6.28/6.29): transactional application of an
// ALREADY-selected transition (selection/evaluation is WBS 6.14 —
// consumed, not reimplemented). The op reads the runtime state, verifies
// the caller's expected version and that the transition belongs to the
// current state, then in ONE transaction updates the current state,
// increments the runtime version, and appends the transition history with
// the per-project monotonic sequence — all atomic, rolling back on any
// failure.
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

// TransitionApplicationService applies already-selected transitions
// transactionally (WBS 6.27).
type TransitionApplicationService struct {
	runtime persistence.RuntimeStateRepository
	history persistence.TransitionHistoryRepository
	// projects is the WBS 6.34 cancellation guard source (nil = no guard).
	projects persistence.ProjectRepository
	// historyOverride is the failure-injection seam (T6.27.5): when set,
	// the op resolves the history repository through it.
	historyOverride persistence.TransitionHistoryRepository
	txPool          *pgxpool.Pool
}

// NewTransitionApplicationService wires the service to the runtime-state
// and transition-history repositories.
func NewTransitionApplicationService(
	runtimeRepo persistence.RuntimeStateRepository,
	historyRepo persistence.TransitionHistoryRepository,
) *TransitionApplicationService {
	return &TransitionApplicationService{runtime: runtimeRepo, history: historyRepo}
}

// WithProjects wires the project-status guard (WBS 6.34): ApplyTransition
// reads the project's lifecycle status through it and rejects normal
// transitions on a CANCELLED project before any write. When unset, no
// lifecycle guard runs (unit scope).
func (s *TransitionApplicationService) WithProjects(projects persistence.ProjectRepository) *TransitionApplicationService {
	s.projects = projects
	return s
}

// WithTxPool wires the transaction pool so read-verify-write + history
// append are ONE transaction (WBS 6.27.4); with a nil pool the op runs
// sequentially (the versioned update is still the authoritative guard).
func (s *TransitionApplicationService) WithTxPool(pool *pgxpool.Pool) *TransitionApplicationService {
	s.txPool = pool
	return s
}

// WithHistoryRepo swaps the history repository (failure-injection seam for
// T6.27.5). A nil argument restores the primary repository.
func (s *TransitionApplicationService) WithHistoryRepo(repo persistence.TransitionHistoryRepository) *TransitionApplicationService {
	s.historyOverride = repo
	return s
}

func (s *TransitionApplicationService) resolveHistory() persistence.TransitionHistoryRepository {
	if s.historyOverride != nil {
		return s.historyOverride
	}
	return s.history
}

// ApplyTransitionInput is one already-selected transition to apply (WBS
// 6.27): the transition's endpoints + outcome (P6-14 selection output),
// optional phase/work-unit references for the history entry, and the
// caller's expected runtime version (the optimistic-concurrency guard,
// WBS 6.24).
type ApplyTransitionInput struct {
	FromStateID string
	ToStateID   string
	Outcome     string

	PhaseID    *string
	WorkUnitID *string

	ExpectedVersion int
}

// TransitionHistoryView is the service-level view of the recorded history
// entry (WBS 6.28).
type TransitionHistoryView struct {
	Seq            int
	FromStateID    string
	ToStateID      string
	Outcome        string
	RuntimeVersion int
	PhaseID        *string
	WorkUnitID     *string
	StateChangedAt time.Time
}

// AppliedTransition is the result of a successful application: the updated
// runtime state plus the history entry that was recorded atomically with it.
type AppliedTransition struct {
	State   *RuntimeState
	History *TransitionHistoryView
}

// ApplyTransition applies an already-selected transition transactionally
// (WBS 6.27, T6.27.1–T6.27.5):
//
//  1. read the project's runtime state (ErrNotFound when unprepared);
//  2. verify the caller's expected version (stale → ErrConflict BEFORE any
//     write, the P6-20 optimistic-concurrency pattern);
//  3. verify the transition belongs to the current state (from-state
//     mismatch → ErrInvalidInput BEFORE any write);
//  4. in ONE transaction: update the current state, increment the runtime
//     version, and append the history row with the next per-project
//     sequence number (T6.27.4); any failure rolls everything back
//     (T6.27.5).
func (s *TransitionApplicationService) ApplyTransition(ctx context.Context, projectID string, in *ApplyTransitionInput) (*AppliedTransition, error) {
	proj, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if in == nil {
		return nil, ErrInvalidInput
	}
	from, err := uuid.Parse(in.FromStateID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	to, err := uuid.Parse(in.ToStateID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if in.Outcome == "" || in.ExpectedVersion < 0 {
		return nil, ErrInvalidInput
	}
	// WBS 6.34: cancellation blocks normal transitions. The guard reads the
	// project's lifecycle status before any write; when the projects source
	// is wired, a CANCELLED project rejects the transition. (PAUSED does
	// not block — WBS 6.33 says pause leaves the position untouched and
	// resume continues it.)
	var phaseID, unitID *uuid.UUID
	if in.PhaseID != nil {
		p, err := uuid.Parse(*in.PhaseID)
		if err != nil {
			return nil, ErrInvalidInput
		}
		phaseID = &p
	}
	if in.WorkUnitID != nil {
		u, err := uuid.Parse(*in.WorkUnitID)
		if err != nil {
			return nil, ErrInvalidInput
		}
		unitID = &u
	}

	if s.txPool == nil {
		out, e := s.applyOnce(ctx, nil, proj, in, from, to, phaseID, unitID)
		if e != nil {
			return nil, translate(e)
		}
		return out, nil
	}
	var out *AppliedTransition
	e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var ee error
		out, ee = s.applyOnce(ctx, tx, proj, in, from, to, phaseID, unitID)
		return ee
	})
	if e != nil {
		return nil, translate(e)
	}
	return out, nil
}

// applyOnce performs one application attempt against the given transaction
// (nil for the sequential path).
func (s *TransitionApplicationService) applyOnce(ctx context.Context, tx persistence.Transaction, proj uuid.UUID, in *ApplyTransitionInput, from, to uuid.UUID, phaseID, unitID *uuid.UUID) (*AppliedTransition, error) {
	// WBS 6.34 cancellation guard: runs BEFORE any read of the runtime row
	// so a rejected transition writes nothing. Joins the caller's
	// transaction when one is open (fresh read under the same tx).
	if s.projects != nil {
		var pr persistence.ProjectRepository
		if tx == nil {
			pr = s.projects
		} else {
			pr = s.projects.WithTx(tx)
		}
		p, perr := pr.Get(ctx, proj.String())
		if perr != nil {
			return nil, translate(perr)
		}
		if p.Status == project.StatusCANCELLED {
			return nil, ErrInvalidInput
		}
	}
	var runtimeRepo persistence.RuntimeStateRepository
	if tx == nil {
		runtimeRepo = s.runtime
	} else {
		runtimeRepo = s.runtime.WithTx(tx)
	}
	existing, err := runtimeRepo.Get(ctx, proj)
	if err != nil {
		return nil, translate(err)
	}
	// Step 2: the optimistic-concurrency guard (WBS 6.24 / P6-20 pattern).
	if in.ExpectedVersion != existing.Version {
		return nil, ErrConflict
	}
	// Step 3: the transition must belong to the current state.
	if from != existing.CurrentStateID {
		return nil, ErrInvalidInput
	}

	// Retention semantics (WBS 6.31/6.32): an OMITTED (nil) association in
	// the apply input RETAINS the stored runtime-state association; an
	// explicitly provided value re-binds it. So the phase/unit can only move
	// via an explicit rebind (work-unit replacement after UNIT_DONE; phase
	// advancement) — a continuation/reset that omits them never changes them
	// and never implicitly clears them.
	if phaseID == nil {
		phaseID = existing.PhaseID
	}
	if unitID == nil {
		unitID = existing.WorkUnitID
	}

	newVersion := existing.Version + 1
	rec := &persistence.RuntimeStateRecord{
		ID: existing.ID, ProjectID: proj,
		StateMachineID: existing.StateMachineID, StateMachineVersion: existing.StateMachineVersion,
		CurrentStateID: to, PhaseID: phaseID, WorkUnitID: unitID,
		EnteredAt: time.Now().UTC(),
		Version:   existing.Version,
	}
	// Step 4a: update the current state + increment the version.
	if err := runtimeRepo.UpdateWithVersion(ctx, rec, existing.Version); err != nil {
		return nil, translate(err)
	}
	// Step 4b: append the history row with the next sequence (allocated
	// under the project-scoped advisory lock; joins this transaction).
	hrec := &persistence.TransitionHistoryRecord{
		ID: uuid.New(), ProjectID: proj,
		StateMachineID: existing.StateMachineID, StateMachineVersion: existing.StateMachineVersion,
		FromStateID: existing.CurrentStateID, ToStateID: to,
		Outcome: in.Outcome, PhaseID: phaseID, WorkUnitID: unitID,
		RuntimeVersion: newVersion,
		CreatedAt:      time.Now().UTC(),
	}
	historyRepo := s.resolveHistory()
	if tx != nil {
		historyRepo = historyRepo.WithTx(tx)
	}
	if err := historyRepo.Append(ctx, hrec, true); err != nil {
		return nil, translate(err)
	}
	return &AppliedTransition{
		State: s.toRuntimeView(existing, to, phaseID, unitID, newVersion),
		History: &TransitionHistoryView{
			Seq:            hrec.Seq,
			FromStateID:    existing.CurrentStateID.String(),
			ToStateID:      to.String(),
			Outcome:        in.Outcome,
			RuntimeVersion: newVersion,
			PhaseID:        phaseStr(phaseID),
			WorkUnitID:     phaseStr(unitID),
			StateChangedAt: hrec.CreatedAt,
		},
	}, nil
}

// phaseStr renders a retained (or explicit) uuid association as the string
// form the service view exposes (nil stays nil).
func phaseStr(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

func (s *TransitionApplicationService) toRuntimeView(existing *persistence.RuntimeStateRecord, to uuid.UUID, phaseID, unitID *uuid.UUID, version int) *RuntimeState {
	var p, u *string
	if phaseID != nil {
		v := phaseID.String()
		p = &v
	}
	if unitID != nil {
		v := unitID.String()
		u = &v
	}
	return &RuntimeState{
		ProjectID:           existing.ProjectID.String(),
		StateMachineID:      existing.StateMachineID.String(),
		StateMachineVersion: existing.StateMachineVersion,
		CurrentStateID:      to.String(),
		PhaseID:             p,
		WorkUnitID:          u,
		EnteredAt:           time.Now().UTC(),
		Version:             version,
	}
}

// ListHistory returns the project's transition history in seq order
// (WBS 6.28 reads).
func (s *TransitionApplicationService) ListHistory(ctx context.Context, projectID string) ([]*TransitionHistoryView, error) {
	proj, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	recs, err := s.resolveHistory().ListByProject(ctx, proj)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]*TransitionHistoryView, 0, len(recs))
	for _, r := range recs {
		var p, u *string
		if r.PhaseID != nil {
			v := r.PhaseID.String()
			p = &v
		}
		if r.WorkUnitID != nil {
			v := r.WorkUnitID.String()
			u = &v
		}
		out = append(out, &TransitionHistoryView{
			Seq:         r.Seq,
			FromStateID: r.FromStateID.String(),
			ToStateID:   r.ToStateID.String(),
			Outcome:     r.Outcome, RuntimeVersion: r.RuntimeVersion,
			PhaseID: p, WorkUnitID: u, StateChangedAt: r.CreatedAt,
		})
	}
	return out, nil
}
