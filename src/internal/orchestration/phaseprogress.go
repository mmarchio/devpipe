package orchestration

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/phase"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// PhaseProgress is the dashboard-ready snapshot for one project (WBS 2.19).
// CurrentPhase is nil (and everything else zero) when the project is
// completed or has no phases; ActivePlan is nil when the current phase has
// no active plan; CurrentWorkUnit is nil when the active plan has no
// eligible work unit.
type PhaseProgress struct {
	CurrentPhase    *phase.Phase
	ActivePlan      *wbs.ExecutionPlan
	CurrentWorkUnit *wbs.WorkUnit
	Progress        wbs.WorkProgress
}

// IsZero reports whether the snapshot is empty (no current phase, no plan,
// no work unit, no units at all).
func (p PhaseProgress) IsZero() bool {
	return p.CurrentPhase == nil && p.ActivePlan == nil && p.CurrentWorkUnit == nil &&
		p.Progress.Total == 0 && p.Progress.Completed == 0 && p.Progress.Remaining == 0 && p.Progress.Percentage == 0
}

// PhaseProgressService reports a project's current phase progress (WBS
// 2.19): the current phase, its active execution plan, the current work
// unit, and deterministic progress counts (P2-18). It is the data source
// for the future dashboard; no rendering happens here.
type PhaseProgressService struct {
	projects interface {
		Get(ctx context.Context, id string) (*project.Project, error)
	}
	phases interface {
		ListByProject(ctx context.Context, projectID uuid.UUID) ([]*phase.Phase, error)
	}
	plans interface {
		ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error)
	}
	units interface {
		ListByPlan(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error)
		ListEligible(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error)
	}
}

// NewPhaseProgressService wires the service to its repositories.
func NewPhaseProgressService(
	projects interface {
		Get(ctx context.Context, id string) (*project.Project, error)
	},
	phases interface {
		ListByProject(ctx context.Context, projectID uuid.UUID) ([]*phase.Phase, error)
	},
	plans interface {
		ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error)
	},
	units interface {
		ListByPlan(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error)
		ListEligible(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error)
	},
) *PhaseProgressService {
	return &PhaseProgressService{projects: projects, phases: phases, plans: plans, units: units}
}

// CurrentPhaseProgress returns the project's current phase progress (WBS
// 2.19, T2.19.1–T2.19.5): the lowest non-DONE phase, its highest-version
// non-SUPERSEDED plan, that plan's next eligible work unit, and progress
// computed by the deterministic P2-18 calculation. Unknown projects yield
// the underlying persistence.ErrNotFound.
func (s *PhaseProgressService) CurrentPhaseProgress(ctx context.Context, projectID uuid.UUID) (*PhaseProgress, error) {
	if _, err := s.projects.Get(ctx, projectID.String()); err != nil {
		return nil, err
	}
	phases, err := s.phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	current := wbs.CurrentPhase(phases)
	if current == nil {
		// Completed (or phase-less) project: no current phase (T2.19.5).
		return &PhaseProgress{}, nil
	}
	out := &PhaseProgress{CurrentPhase: current}

	plans, err := s.plans.ListByPhase(ctx, current.ID)
	if err != nil {
		return nil, err
	}
	out.ActivePlan = wbs.ActivePlan(current.ID, plans)
	if out.ActivePlan == nil {
		return out, nil
	}

	units, err := s.units.ListByPlan(ctx, out.ActivePlan.ID)
	if err != nil {
		return nil, err
	}
	out.Progress = wbs.CalculateWorkProgress(units)

	eligible, err := s.units.ListEligible(ctx, out.ActivePlan.ID)
	if err != nil {
		return nil, err
	}
	if len(eligible) > 0 {
		out.CurrentWorkUnit = eligible[0]
	}
	return out, nil
}
