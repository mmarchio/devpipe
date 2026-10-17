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

// PhaseListSummaryEntry is one phase of the phase list summary (WBS 4.24):
// the phase's order, its lifecycle status, and its DERIVED work progress
// (completed/total/percentage, only DONE units count per WBS 4.19). The
// progress is always present; an unplanned phase reports 0/0/0.
type PhaseListSummaryEntry struct {
	Order    int
	Status   phase.PhaseStatus
	Progress PhaseReadModelProgress
}

// phaseSetPlanReader is the set-based plan read the summary needs (one
// statement for a whole phase set — the N+1 avoidance of WBS 4.24).
type phaseSetPlanReader interface {
	ListByPhaseSet(ctx context.Context, phaseIDs []uuid.UUID) ([]*wbs.ExecutionPlan, error)
}

// planSetUnitReader is the set-based work-unit read the summary needs (one
// statement for a whole plan set).
type planSetUnitReader interface {
	ListByPlanSet(ctx context.Context, planIDs []uuid.UUID) ([]*wbs.WorkUnit, error)
}

// PhaseListSummaryService assembles the phase list summary (WBS 4.24): the
// whole project roadmap — every phase in ascending order with its status
// and derived progress. It consumes the same repository family as the
// P4-19 phase read model, but reads in SETS (all phases, all plans of the
// phase set, all units of the active-plan set) so the statement count does
// not grow with the phase count. When a transaction pool is available the
// whole assembly runs in one transaction; without one the reads run
// sequentially. The summary is read-only: it never mutates phases,
// documents, plans, or work units.
type PhaseListSummaryService struct {
	phases   persistence.PhaseRepository
	projects persistence.ProjectRepository
	plans    phaseSetPlanReader
	units    planSetUnitReader
	txPool   *pgxpool.Pool
}

// NewPhaseListSummaryService wires the phase list summary service to its
// repositories and, when provided, the transaction pool used for
// transactionally consistent assembly.
func NewPhaseListSummaryService(
	phases persistence.PhaseRepository,
	projects persistence.ProjectRepository,
	plans *dbrepo.PlanRepository,
	units *dbrepo.WorkUnitRepository,
	txPool *pgxpool.Pool,
) *PhaseListSummaryService {
	return &PhaseListSummaryService{
		phases: phases, projects: projects, plans: plans, units: units, txPool: txPool,
	}
}

// PhaseListSummary loads the phase list summary (WBS 4.24) for a project.
// The project must exist (unknown/malformed id -> ErrNotFound/
// ErrInvalidInput). Every phase is returned in ascending phase order with
// its status and the progress derived from its active plan's work units
// (highest version, not SUPERSEDED, per WBS 2.19); a phase without an
// active plan reports 0/0/0. The reads are set-based — phases by project,
// plans by the phase set, units by the active-plan set — so the statement
// count stays bounded as the phase count grows (T4.24.5).
func (s *PhaseListSummaryService) PhaseListSummary(ctx context.Context, projectID string) ([]PhaseListSummaryEntry, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	do := func(
		projRepo persistence.ProjectRepository,
		phaseRepo persistence.PhaseRepository,
		planRepo phaseSetPlanReader,
		unitRepo planSetUnitReader,
	) ([]PhaseListSummaryEntry, error) {
		if _, err := projRepo.Get(ctx, parsed.String()); err != nil {
			return nil, translate(err)
		}
		phases, err := phaseRepo.ListByProject(ctx, parsed)
		if err != nil {
			return nil, translate(err)
		}
		out := make([]PhaseListSummaryEntry, 0, len(phases))
		for _, p := range phases {
			out = append(out, PhaseListSummaryEntry{Order: p.Order, Status: p.Status})
		}
		if s.plans == nil || len(phases) == 0 {
			return out, nil
		}
		phaseIDs := make([]uuid.UUID, len(phases))
		byPhase := make(map[uuid.UUID]int, len(phases))
		for i, p := range phases {
			phaseIDs[i] = p.ID
			byPhase[p.ID] = i
		}
		plans, err := s.plans.ListByPhaseSet(ctx, phaseIDs)
		if err != nil {
			return nil, translate(err)
		}
		plansByPhase := make(map[uuid.UUID][]*wbs.ExecutionPlan)
		for _, pl := range plans {
			plansByPhase[pl.PhaseID] = append(plansByPhase[pl.PhaseID], pl)
		}
		type activeRef struct {
			index int
			plan  *wbs.ExecutionPlan
		}
		var actives []activeRef
		for _, p := range phases {
			if a := wbs.ActivePlan(p.ID, plansByPhase[p.ID]); a != nil {
				actives = append(actives, activeRef{byPhase[p.ID], a})
			}
		}
		if len(actives) > 0 {
			planIDs := make([]uuid.UUID, len(actives))
			for i, a := range actives {
				planIDs[i] = a.plan.ID
			}
			units, err := s.units.ListByPlanSet(ctx, planIDs)
			if err != nil {
				return nil, translate(err)
			}
			unitsByPlan := make(map[uuid.UUID][]*wbs.WorkUnit)
			for _, u := range units {
				unitsByPlan[u.ExecutionPlanID] = append(unitsByPlan[u.ExecutionPlanID], u)
			}
			for _, a := range actives {
				wp := wbs.CalculateWorkProgress(unitsByPlan[a.plan.ID])
				out[a.index].Progress = PhaseReadModelProgress{
					Completed:  wp.Completed,
					Total:      wp.Total,
					Percentage: wp.Percentage,
				}
			}
		}
		return out, nil
	}
	if s.txPool == nil {
		return do(s.projects, s.phases, s.plans, s.units)
	}
	var out []PhaseListSummaryEntry
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var err error
		out, err = do(
			s.projects.WithTx(tx),
			s.phases.WithTx(tx),
			s.plansWithTx(s.plans, tx),
			s.unitsWithTx(s.units, tx),
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PhaseListSummaryService) plansWithTx(p phaseSetPlanReader, tx persistence.Transaction) phaseSetPlanReader {
	if r, ok := p.(*dbrepo.PlanRepository); ok {
		return r.WithTx(tx)
	}
	return p
}

func (s *PhaseListSummaryService) unitsWithTx(u planSetUnitReader, tx persistence.Transaction) planSetUnitReader {
	if r, ok := u.(*dbrepo.WorkUnitRepository); ok {
		return r.WithTx(tx)
	}
	return u
}
