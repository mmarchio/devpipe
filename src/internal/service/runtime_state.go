// P6-20 (WBS 6.23/6.24): runtime-state persistence with optimistic
// concurrency. The service persists the project's current logical
// workflow position (machine family + version binding, current state,
// optional phase/work-unit associations) and protects it with a
// versioned update so concurrent overseer processes cannot lose a
// transition. Pure persistence + concurrency guard: no transition
// evaluation (WBS 6.25+ territory).
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// RuntimeState is the service-level view of a project's current logical
// workflow position (WBS 6.23).
type RuntimeState struct {
	ProjectID           string
	StateMachineID      string
	StateMachineVersion int
	CurrentStateID      string
	PhaseID             *string
	WorkUnitID          *string
	EnteredAt           time.Time
	Version             int
}

// RuntimeStateService persists the project's runtime state and protects
// it under optimistic concurrency (WBS 6.23/6.24).
type RuntimeStateService struct {
	repo     persistence.RuntimeStateRepository
	machines persistence.StateMachineRepository
	txPool   *pgxpool.Pool
}

// NewRuntimeStateService wires the service to the runtime-state
// repository.
func NewRuntimeStateService(repo persistence.RuntimeStateRepository) *RuntimeStateService {
	return &RuntimeStateService{repo: repo}
}

// WithMachines wires the state-machine repository for the initial
// runtime-state op (WBS 6.30): Initialize resolves the machine's
// configured initial state and version. With it unwired, Initialize
// reports ErrInfrastructure (the op needs the machine surface).
func (s *RuntimeStateService) WithMachines(machines persistence.StateMachineRepository) *RuntimeStateService {
	s.machines = machines
	return s
}

// WithTxPool wires the transaction pool so the read-then-write cycle is
// one transaction (WBS 2.21); with a nil pool the op runs sequentially
// (the versioned update is still the authoritative guard).
func (s *RuntimeStateService) WithTxPool(pool *pgxpool.Pool) *RuntimeStateService {
	s.txPool = pool
	return s
}

// TransitionInput is one runtime-state transition: the new position and
// the caller's expected current version (0 = "no row yet / first write").
type TransitionInput struct {
	StateMachineID      string
	StateMachineVersion int
	CurrentStateID      string
	PhaseID             *string
	WorkUnitID          *string
	ExpectedVersion     int
}

// Transition persists a new logical workflow position for the project
// (WBS 6.23/6.24, T6.23.1–T6.23.5, T6.24.1–T6.24.5). It is a pure
// position write guarded by optimistic concurrency:
//
//   - ExpectedVersion 0 with no stored row: the row is created at
//     Version 1 (T6.23.1).
//   - ExpectedVersion == stored version: the position is written and the
//     version is incremented atomically (T6.24.1/T6.24.2).
//   - Any other ExpectedVersion: ErrConflict, the stored state untouched
//     (T6.24.3/T6.24.5).
//
// PhaseID / WorkUnitID are nullable associations (T6.23.3/T6.23.4); a
// nil pointer clears the association. The machine binding (id +
// version) is carried on every write and retained on read (T6.23.5).
func (s *RuntimeStateService) Transition(ctx context.Context, projectID string, in *TransitionInput) (*RuntimeState, error) {
	proj, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if in == nil {
		return nil, ErrInvalidInput
	}
	mid, err := uuid.Parse(in.StateMachineID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	cur, err := uuid.Parse(in.CurrentStateID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if in.StateMachineVersion < 1 || in.ExpectedVersion < 0 {
		return nil, ErrInvalidInput
	}
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
		return s.transitionOnce(ctx, nil, proj, in, mid, cur, phaseID, unitID)
	}
	var out *RuntimeState
	e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var ee error
		out, ee = s.transitionOnce(ctx, tx, proj, in, mid, cur, phaseID, unitID)
		return ee
	})
	if e != nil {
		return nil, translate(e)
	}
	return out, nil
}

func (s *RuntimeStateService) bind(tx persistence.Transaction) persistence.RuntimeStateRepository {
	if tx == nil {
		return s.repo
	}
	return s.repo.WithTx(tx)
}

// transitionOnce performs one transition attempt against the given
// transaction (nil for the sequential path): read the stored state,
// then create (first write) or versioned-update it.
func (s *RuntimeStateService) transitionOnce(ctx context.Context, tx persistence.Transaction, proj uuid.UUID, in *TransitionInput, mid, cur uuid.UUID, phaseID, unitID *uuid.UUID) (*RuntimeState, error) {
	repo := s.bind(tx)
	existing, err := repo.Get(ctx, proj)
	switch {
	case err == nil:
		// Row exists: the optimistic-concurrency guard applies.
		if in.ExpectedVersion != existing.Version {
			return nil, ErrConflict
		}
		rec := &persistence.RuntimeStateRecord{
			ID: existing.ID, ProjectID: proj,
			StateMachineID: mid, StateMachineVersion: in.StateMachineVersion,
			CurrentStateID: cur, PhaseID: phaseID, WorkUnitID: unitID,
			EnteredAt: time.Now().UTC(),
			Version:   existing.Version,
		}
		if err := repo.UpdateWithVersion(ctx, rec, existing.Version); err != nil {
			return nil, translate(err)
		}
		return s.toView(rec, existing.Version+1), nil
	case errors.Is(err, persistence.ErrNotFound):
		// No row yet: only a first write (ExpectedVersion 0) may create it.
		if in.ExpectedVersion != 0 {
			return nil, ErrConflict
		}
		rec := &persistence.RuntimeStateRecord{
			ID: uuid.New(), ProjectID: proj,
			StateMachineID: mid, StateMachineVersion: in.StateMachineVersion,
			CurrentStateID: cur, PhaseID: phaseID, WorkUnitID: unitID,
			EnteredAt: time.Now().UTC(), Version: 1,
		}
		if err := repo.Create(ctx, rec); err != nil {
			return nil, translate(err)
		}
		return s.toView(rec, 1), nil
	default:
		return nil, translate(err)
	}
}

// Get returns the project's current runtime state (WBS 6.23);
// ErrNotFound when the project has none yet.
func (s *RuntimeStateService) Get(ctx context.Context, projectID string) (*RuntimeState, error) {
	proj, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rec, err := s.repo.Get(ctx, proj)
	if err != nil {
		return nil, translate(err)
	}
	return s.toView(rec, rec.Version), nil
}

func (s *RuntimeStateService) toView(rec *persistence.RuntimeStateRecord, version int) *RuntimeState {
	var phaseID, unitID *string
	if rec.PhaseID != nil {
		p := rec.PhaseID.String()
		phaseID = &p
	}
	if rec.WorkUnitID != nil {
		u := rec.WorkUnitID.String()
		unitID = &u
	}
	return &RuntimeState{
		ProjectID:           rec.ProjectID.String(),
		StateMachineID:      rec.StateMachineID.String(),
		StateMachineVersion: rec.StateMachineVersion,
		CurrentStateID:      rec.CurrentStateID.String(),
		PhaseID:             phaseID,
		WorkUnitID:          unitID,
		EnteredAt:           rec.EnteredAt,
		Version:             version,
	}
}

// Initialize prepares the project for execution by initializing its
// runtime state to the machine's configured initial state (WBS 6.30,
// T6.30.1-T6.30.4): the machine is resolved, its configured initial
// state becomes the runtime state's current state, and the machine
// family + version binding is captured on the row (created at version 1
// via the P6-20 repository op — no parallel write path). No LLM
// invocation occurs (WBS 6.30).
//
// Semantics:
//   - Unknown machine id: ErrNotFound, no row written (T6.30.4).
//   - Machine with no configured initial state: ErrInvalidInput, no row
//     written (T6.30.4).
//   - Duplicate init (a runtime row already exists): idempotent NO-OP —
//     the existing row is returned unchanged and no write occurs
//     (T6.30.3). Chosen over re-init: a prepared project must not be
//     reset by a second prepare call.
func (s *RuntimeStateService) Initialize(ctx context.Context, projectID, machineID string) (*RuntimeState, error) {
	if s.machines == nil {
		return nil, ErrInfrastructure
	}
	proj, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	mid, err := uuid.Parse(machineID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	machine, err := s.machines.Get(ctx, mid)
	if err != nil {
		return nil, translate(err)
	}
	if machine.InitialStateID == nil {
		return nil, ErrInvalidInput
	}
	if s.txPool == nil {
		return s.initializeOnce(ctx, nil, proj, machine)
	}
	var out *RuntimeState
	e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var ee error
		out, ee = s.initializeOnce(ctx, tx, proj, machine)
		return ee
	})
	if e != nil {
		return nil, translate(e)
	}
	return out, nil
}

// initializeOnce performs one initialization attempt against the given
// transaction (nil for the sequential path): a duplicate is a no-op,
// otherwise the row is created at version 1 with the machine's
// configured initial state and the machine family + version binding.
func (s *RuntimeStateService) initializeOnce(ctx context.Context, tx persistence.Transaction, proj uuid.UUID, machine *persistence.MachineRecord) (*RuntimeState, error) {
	repo := s.bind(tx)
	existing, err := repo.Get(ctx, proj)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	if err == nil {
		// Already initialized: idempotent no-op (T6.30.3) — return the
		// stored row unchanged, no write.
		return s.toView(existing, existing.Version), nil
	}
	rec := &persistence.RuntimeStateRecord{
		ID: uuid.New(), ProjectID: proj,
		StateMachineID: machine.FamilyID, StateMachineVersion: machine.Version,
		CurrentStateID: *machine.InitialStateID,
		EnteredAt:      time.Now().UTC(), Version: 1,
	}
	if err := repo.Create(ctx, rec); err != nil {
		return nil, translate(err)
	}
	return s.toView(rec, 1), nil
}
