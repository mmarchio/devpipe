package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// activeTaskTypeKey is the normalized key of the GLOBAL default
// ACTIVE_TASK type seeded at startup (P7-07).
const activeTaskTypeKey = "ACTIVE_TASK"

// ActiveTaskService implements the ACTIVE_TASK derivation and
// synchronization (WBS 7.11/7.12). The ACTIVE_TASK file content is
// DERIVED from the project's current work unit — it is never a competing
// source of truth: the DB work-unit row is authoritative, and the
// derivation writes through the P7-08 versioned content path, so the
// previous active-task content is retained historically (T7.12.2).
//
// Current-work-unit selection rule (stated per the task NOTES and pinned
// by the unit tests in internal/wbs and internal/integration): the FIRST
// unit, in ascending sequence order, of the project's current phase's
// active (non-SUPERSEDED, highest-version) plan whose status is not DONE
// (the WBS 2.19 CurrentPhase/ActivePlan composition plus
// wbs.CurrentWorkUnit). A project with no such unit has no current work
// unit (ErrNotFound — nothing is written).
type ActiveTaskService struct {
	crud   *StateFileService
	types  persistence.StateFileTypeRepository
	files  persistence.StateFileRepository
	phases persistence.PhaseRepository
	plans  activeTaskPlanLister
	units  activeTaskUnitLister
}

// activeTaskPlanLister lists a phase's execution plans.
// *dbrepo.PlanRepository satisfies it.
type activeTaskPlanLister interface {
	ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error)
}

// activeTaskUnitLister lists a plan's work units in ascending sequence
// order. *dbrepo.WorkUnitRepository satisfies it.
type activeTaskUnitLister interface {
	ListByPlan(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error)
}

// NewActiveTaskService wires the derivation to the P7-08 CRUD service (the
// content write path — its repositories are consumed through accessors) and
// the phase/plan/unit repositories the current-work-unit resolution reads.
func NewActiveTaskService(
	crud *StateFileService,
	phases persistence.PhaseRepository,
	plans activeTaskPlanLister,
	units activeTaskUnitLister,
) *ActiveTaskService {
	return &ActiveTaskService{
		crud:   crud,
		types:  crud.Types(),
		files:  crud.Files(),
		phases: phases,
		plans:  plans,
		units:  units,
	}
}

// DeriveActiveTask derives the project's ACTIVE_TASK content from its
// current work unit and writes it through the versioned content path
// (WBS 7.11/7.12): the (ACTIVE_TASK, WORK_UNIT, unit) file is created on
// first use (one file per derived unit — a unit change binds a NEW file,
// which is how the previous active-task content stays retained in
// history), the rendered content is appended as the next version, and the
// work-unit row is left byte-identical (T7.11.7 — the op is read-only over
// the unit). A project with no current work unit is ErrNotFound with
// nothing written.
//
// The append re-reads the current version on each attempt and retries on
// ErrConflict (the advisory-locked version append may lose a concurrent
// race — the bounded retry keeps the sequence contiguous, T7.12.1).
func (s *ActiveTaskService) DeriveActiveTask(ctx context.Context, projectID string) (*StateFileVersionView, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	unit, err := s.currentWorkUnit(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}

	file, err := s.resolveActiveTaskFile(ctx, pid, unit)
	if err != nil {
		return nil, translate(err)
	}
	content := statefile.RenderActiveTask(unit)

	const maxAttempts = 5
	for attempt := 0; ; attempt++ {
		cur, err := s.crud.GetCurrentVersion(ctx, file.ID.String())
		if err != nil {
			return nil, translate(err)
		}
		expected := 0
		if cur != nil {
			expected = cur.Version
		}
		v, err := s.crud.UpdateContent(ctx, file.ID.String(), content, expected)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrConflict) || attempt >= maxAttempts-1 {
			return nil, translate(err)
		}
	}
}

// currentWorkUnit resolves the project's current work unit under the
// stated selection rule: the current phase's active plan's first non-DONE
// unit in sequence order. No current phase, no active plan, or an all-DONE
// plan means no current work unit (persistence.ErrNotFound).
func (s *ActiveTaskService) currentWorkUnit(ctx context.Context, projectID uuid.UUID) (*wbs.WorkUnit, error) {
	phases, err := s.phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	cur := wbs.CurrentPhase(phases)
	if cur == nil {
		return nil, persistence.ErrNotFound
	}
	plans, err := s.plans.ListByPhase(ctx, cur.ID)
	if err != nil {
		return nil, err
	}
	active := wbs.ActivePlan(cur.ID, plans)
	if active == nil {
		return nil, persistence.ErrNotFound
	}
	units, err := s.units.ListByPlan(ctx, active.ID)
	if err != nil {
		return nil, err
	}
	unit := wbs.CurrentWorkUnit(units)
	if unit == nil {
		return nil, persistence.ErrNotFound
	}
	return unit, nil
}

// resolveActiveTaskFile returns the project's ACTIVE_TASK file bound to the
// given unit, creating it on first use. The global default type is looked
// up by key (P7-07 seeds it at startup); the file lookup is
// (type, WORK_UNIT, unit), so a derivation for a new unit binds a fresh
// file and the previous unit's file (and its content history) is untouched.
func (s *ActiveTaskService) resolveActiveTaskFile(ctx context.Context, projectID uuid.UUID, unit *wbs.WorkUnit) (*persistence.StateFileRecord, error) {
	typeID, err := s.activeTaskTypeID(ctx)
	if err != nil {
		return nil, err
	}
	unitID := unit.ID
	rec, err := s.files.GetByTypeAndScope(ctx, projectID, typeID, string(statefile.ScopeWorkUnit), nil, &unitID)
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	sf, err := statefile.NewStateFile(statefile.StateFileInput{
		ProjectID:  &projectID,
		TypeID:     &typeID,
		Scope:      statefile.ScopeWorkUnit,
		WorkUnitID: &unitID,
	})
	if err != nil {
		return nil, fmt.Errorf("active-task file: %w", err)
	}
	newRec := &persistence.StateFileRecord{
		ID:             sf.ID,
		ProjectID:      sf.ProjectID,
		TypeID:         sf.TypeID,
		Scope:          string(sf.Scope),
		WorkUnitID:     sf.WorkUnitID,
		CurrentVersion: sf.CurrentVersion,
		CreatedAt:      sf.CreatedAt,
		UpdatedAt:      sf.UpdatedAt,
	}
	if err := s.files.Create(ctx, newRec); err != nil {
		return nil, err
	}
	return newRec, nil
}

func (s *ActiveTaskService) activeTaskTypeID(ctx context.Context) (uuid.UUID, error) {
	recs, err := s.types.ListByScope(ctx, nil)
	if err != nil {
		return uuid.Nil, err
	}
	for _, r := range recs {
		if r.Key == activeTaskTypeKey {
			return r.ID, nil
		}
	}
	return uuid.Nil, persistence.ErrNotFound
}
