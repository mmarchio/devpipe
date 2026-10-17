// P6-30 (WBS 6.40): the runtime-state REST read model. GET
// /api/v1/projects/{project}/runtime-state composes the existing P6-20/
// P6-22 runtime-state read with the phase and work-unit associations
// stored on the runtime row (WBS 6.31/6.32 bindings) into one read-only
// response: current state, phase (order + name), work unit (id + name),
// and the runtime version. Read-only: the service exposes no mutation
// surface — the only writers remain the P6-20 Transition op and the
// P6-22 Initialize op.
package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// RuntimeStatePhase is the phase section of the runtime-state read (WBS
// 6.40): the order and name of the runtime row's associated phase. Nil
// when the row carries no phase association (a runtime state may exist
// before a phase is bound).
type RuntimeStatePhase struct {
	Order int
	Name  string
}

// RuntimeStateWorkUnit is the work-unit section of the runtime-state read
// (WBS 6.40): the id and name of the runtime row's associated work unit.
// Nil when the row carries no work-unit association.
type RuntimeStateWorkUnit struct {
	ID   string
	Name string
}

// RuntimeStateView is the assembled runtime-state read (WBS 6.40): the
// current state id, the phase / work-unit sections (nil when unbound or
// dangling/cross-project), and the row's optimistic-concurrency version
// (T6.40.4).
type RuntimeStateView struct {
	State          string
	Phase          *RuntimeStatePhase
	WorkUnit       *RuntimeStateWorkUnit
	RuntimeVersion int
}

// RuntimeStateAPIService assembles the runtime-state read for the REST
// surface (WBS 6.40). It reuses the P6-20 runtime-state repository as the
// single source of truth and resolves the phase / work-unit associations
// stored on that row — no new state, no new table, no mutation path.
type RuntimeStateAPIService struct {
	runtime persistence.RuntimeStateRepository
	phases  persistence.PhaseRepository
	plans   *dbrepo.PlanRepository
	units   *dbrepo.WorkUnitRepository
	txPool  *pgxpool.Pool
}

// NewRuntimeStateAPIService wires the runtime-state read service to its
// repositories.
func NewRuntimeStateAPIService(
	runtime persistence.RuntimeStateRepository,
	phases persistence.PhaseRepository,
	plans *dbrepo.PlanRepository,
	units *dbrepo.WorkUnitRepository,
) *RuntimeStateAPIService {
	return &RuntimeStateAPIService{
		runtime: runtime, phases: phases, plans: plans, units: units,
	}
}

// WithTxPool wires the transaction pool so the whole read assembles in ONE
// transaction (the P3-26/P4-18 read-model pattern); with a nil pool the
// reads run sequentially (the row read is still authoritative).
func (s *RuntimeStateAPIService) WithTxPool(pool *pgxpool.Pool) *RuntimeStateAPIService {
	s.txPool = pool
	return s
}

// Get returns the project's runtime-state view (WBS 6.40, T6.40.1–
// T6.40.4). Semantics:
//
//   - Malformed project id: ErrInvalidInput.
//   - No runtime row (Initialize never ran): ErrNotFound — the P6-22
//     Initialize op is the only writer (read-only surface, T6.40.5).
//   - The phase / work-unit sections are nil when the row carries no
//     association, and are resolved project-scoped: a phase or work unit
//     belonging to another project is not served (cross-project
//     isolation, pinned by TestP630CrossProjectIsolation). A dangling
//     association (the referenced row was deleted) also yields a nil
//     section — the read stays loadable (the P4-15 nil-section rule).
func (s *RuntimeStateAPIService) Get(ctx context.Context, projectID string) (*RuntimeStateView, error) {
	proj, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	do := func(
		runtimeRepo persistence.RuntimeStateRepository,
		phaseRepo persistence.PhaseRepository,
		planRepo *dbrepo.PlanRepository,
		unitRepo *dbrepo.WorkUnitRepository,
	) (*RuntimeStateView, error) {
		rec, err := runtimeRepo.Get(ctx, proj)
		if err != nil {
			return nil, translate(err)
		}
		view := &RuntimeStateView{
			State:          rec.CurrentStateID.String(),
			RuntimeVersion: rec.Version,
		}
		if rec.PhaseID != nil {
			p, err := phaseRepo.Get(ctx, *rec.PhaseID)
			switch {
			case err == nil:
				view.Phase = &RuntimeStatePhase{Order: p.Order, Name: p.Name}
			case !errors.Is(err, persistence.ErrNotFound) && p == nil:
				return nil, translate(err)
			}
			if view.Phase != nil && p.ProjectID != proj {
				view.Phase = nil // cross-project phase: not served
			}
		}
		if rec.WorkUnitID != nil {
			view.WorkUnit = s.unitSection(ctx, proj, *rec.WorkUnitID, unitRepo)
		}
		return view, nil
	}
	if s.txPool == nil {
		return do(s.runtime, s.phases, s.plans, s.units)
	}
	var out *RuntimeStateView
	e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var ee error
		out, ee = do(
			s.runtime.WithTx(tx),
			s.phases.WithTx(tx),
			s.plans.WithTx(tx),
			s.units.WithTx(tx),
		)
		return ee
	})
	if e != nil {
		return nil, e
	}
	return out, nil
}

// unitSection resolves the work-unit association project-scoped: the unit
// is read by id and its stored project (backfilled from the plan's phase)
// must be this project; a dangling or cross-project association yields a
// nil section.
func (s *RuntimeStateAPIService) unitSection(ctx context.Context, proj uuid.UUID, unitID uuid.UUID, unitRepo *dbrepo.WorkUnitRepository) *RuntimeStateWorkUnit {
	if unitRepo == nil {
		return nil
	}
	u, err := unitRepo.Get(ctx, unitID)
	if err != nil || u == nil || u.ProjectID != proj {
		return nil
	}
	return &RuntimeStateWorkUnit{ID: u.ID.String(), Name: u.Name}
}
