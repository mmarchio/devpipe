package service

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/project"
)

// CompletionService drives the RUNNING → COMPLETED lifecycle transition
// (WBS 3.12). Completion requires that every phase of the project has
// reached DONE; any pending, executing, or failed phase blocks it. The
// later orchestration service will call Complete automatically when the
// final phase finishes.
type CompletionService struct {
	projects  persistence.ProjectRepository
	phases    persistence.PhaseRepository
	lifecycle *LifecycleService
}

// NewCompletionService wires the completion service to its repositories and
// the lifecycle service it delegates the transition (and event persistence)
// to.
func NewCompletionService(
	projects persistence.ProjectRepository,
	phases persistence.PhaseRepository,
	lifecycle *LifecycleService,
) *CompletionService {
	return &CompletionService{projects: projects, phases: phases, lifecycle: lifecycle}
}

// Complete moves a RUNNING project to COMPLETED if and only if every phase
// is DONE (WBS 3.12, T3.12.1–T3.12.5). A non-RUNNING project or a project
// with any incomplete/failed phase is rejected without mutation.
func (s *CompletionService) Complete(ctx context.Context, projectID string) (*project.Project, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	if p.Status != project.StatusRUNNING {
		return nil, ErrInvalidInput
	}
	allDone, err := s.allPhasesDone(ctx, parsed)
	if err != nil {
		return nil, translate(err)
	}
	if !allDone {
		return nil, ErrInvalidInput
	}
	return s.lifecycle.Transition(ctx, projectID, "complete", project.StatusCOMPLETED)
}

// allPhasesDone reports whether every phase of the project has reached DONE.
func (s *CompletionService) allPhasesDone(ctx context.Context, projectID uuid.UUID) (bool, error) {
	phases, err := s.phases.ListByProject(ctx, projectID)
	if err != nil {
		return false, err
	}
	for _, ph := range phases {
		if ph.Status != phase.StatusDONE {
			return false, nil
		}
	}
	return true, nil
}
