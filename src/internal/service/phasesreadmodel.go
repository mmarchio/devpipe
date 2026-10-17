package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// PhaseReadModelPhase is the phase section of the phase read model (WBS
// 4.18): the phase's order and lifecycle status.
type PhaseReadModelPhase struct {
	Order  int
	Status phase.PhaseStatus
}

// PhaseReadModelWBS is the WBS section of the phase read model (WBS 4.18):
// the current version and the DERIVED stale flag (an active plan bound to
// an older WBS version). Stale is defined only against an active plan; a
// phase with a WBS but no plan is unplanned and reports false.
type PhaseReadModelWBS struct {
	Version int
	Stale   bool
}

// PhaseReadModelPlan is the execution-plan section of the phase read model
// (WBS 4.18): the active plan's version. Nil when the phase has no active
// plan.
type PhaseReadModelPlan struct {
	Version int
}

// PhaseReadModelProgress is the progress section of the phase read model
// (WBS 4.18): completed/total work units and the derived percentage (only
// DONE units count, per WBS 4.19). Always present: an unplanned phase
// reports 0/0/0.
type PhaseReadModelProgress struct {
	Completed  int
	Total      int
	Percentage float64
}

// PhaseReadModel is the comprehensive per-phase representation (WBS 4.18)
// combining phase metadata, the WBS section (nil when the phase has no WBS
// document), the active plan section (nil when unplanned), and the work
// progress. It is the source for the future project UI; the P4-20 phase
// list assembles one per phase in order.
type PhaseReadModel struct {
	Phase    *PhaseReadModelPhase
	WBS      *PhaseReadModelWBS
	Plan     *PhaseReadModelPlan
	Progress PhaseReadModelProgress
}

// PhaseReadModelService assembles the phase read model (WBS 4.18) for a
// specific phase. It reuses the same one-transaction / nil-pool sequential
// assembly pattern as the P3-26 snapshot and P4-18 WBS read model services.
type PhaseReadModelService struct {
	phases   persistence.PhaseRepository
	projects persistence.ProjectRepository
	versions persistence.WBSVersionRepository
	plans    *dbrepo.PlanRepository
	units    *dbrepo.WorkUnitRepository
	txPool   *pgxpool.Pool
}

// NewPhaseReadModelService wires the phase read-model service to its
// repositories and, when provided, the transaction pool used for
// transactionally consistent assembly.
func NewPhaseReadModelService(
	phases persistence.PhaseRepository,
	projects persistence.ProjectRepository,
	versions persistence.WBSVersionRepository,
	plans *dbrepo.PlanRepository,
	units *dbrepo.WorkUnitRepository,
	txPool *pgxpool.Pool,
) *PhaseReadModelService {
	return &PhaseReadModelService{
		phases: phases, projects: projects,
		versions: versions, plans: plans, units: units, txPool: txPool,
	}
}

// PhaseReadModel loads the phase read model (WBS 4.18) for a specific
// phase of a project. The phase must belong to the project (cross-project
// or unknown ids -> ErrNotFound, malformed ids -> ErrInvalidInput — the
// getScopedPhase contract). The WBS section is the phase's current version
// with the DERIVED stale flag (an active plan bound to an older version);
// a phase with no WBS yields a nil section. The plan section is the active
// plan (highest version, not SUPERSEDED); nil when unplanned. Progress is
// derived from the active plan's work units (only DONE counts); unplanned
// phases report 0/0/0. When a transaction pool is available the whole
// assembly runs in one transaction; without one the reads run
// sequentially. The read model is read-only: it never mutates phases,
// documents, plans, or work units.
func (s *PhaseReadModelService) PhaseReadModel(ctx context.Context, projectID, phaseID string) (*PhaseReadModel, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	fid, err := uuid.Parse(phaseID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	do := func(
		projRepo persistence.ProjectRepository,
		phaseRepo persistence.PhaseRepository,
		versRepo persistence.WBSVersionRepository,
		planRepo *dbrepo.PlanRepository,
		unitRepo *dbrepo.WorkUnitRepository,
	) (*PhaseReadModel, error) {
		if _, err := projRepo.Get(ctx, pid.String()); err != nil {
			return nil, translate(err)
		}
		p, err := phaseRepo.Get(ctx, fid)
		if err != nil {
			return nil, translate(err)
		}
		if p.ProjectID != pid {
			return nil, ErrNotFound
		}
		out := &PhaseReadModel{Phase: &PhaseReadModelPhase{Order: p.Order, Status: p.Status}}
		cur, err := versRepo.GetCurrentVersion(ctx, p.ID.String())
		if err != nil {
			return nil, translate(err)
		}
		if cur > 0 {
			out.WBS = &PhaseReadModelWBS{Version: cur}
		}
		if s.plans != nil {
			plans, err := planRepo.ListByPhase(ctx, p.ID)
			if err != nil {
				return nil, translate(err)
			}
			if active := wbs.ActivePlan(p.ID, plans); active != nil {
				out.Plan = &PhaseReadModelPlan{Version: active.Version}
				if out.WBS != nil {
					out.WBS.Stale = active.WBSVersion < cur
				}
				units, err := unitRepo.ListByPlan(ctx, active.ID)
				if err != nil {
					return nil, translate(err)
				}
				wp := wbs.CalculateWorkProgress(units)
				out.Progress = PhaseReadModelProgress{
					Completed:  wp.Completed,
					Total:      wp.Total,
					Percentage: wp.Percentage,
				}
			}
		}
		return out, nil
	}
	if s.txPool == nil {
		return do(s.projects, s.phases, s.versions, s.plans, s.units)
	}
	var out *PhaseReadModel
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var err error
		out, err = do(
			s.projects.WithTx(tx),
			s.phases.WithTx(tx),
			s.versions.WithTx(tx),
			s.plans.WithTx(tx),
			s.units.WithTx(tx),
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
