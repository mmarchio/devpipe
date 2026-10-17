package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// taskArchiveTypeKey is the normalized key of the GLOBAL default
// TASK_ARCHIVE type seeded at startup (P7-07).
const taskArchiveTypeKey = "TASK_ARCHIVE"

// TaskArchiveService implements the TASK_ARCHIVE derivation (WBS 7.13).
// The TASK_ARCHIVE content is DERIVED from the project's completed work
// units (status DONE) — it is never a competing source of truth: the DB
// work-unit rows are authoritative (T7.13.5), and the derivation writes
// through the P7-08 versioned content path, so every rebuild is retained
// historically (T7.13.4).
//
// Completion rule: a work unit is "completed" when its status is DONE —
// the P7-10 selection rule's complement. Ordering rule: ascending
// completed_at, tie-broken by (sequence, id) (stated in the statefile
// renderer and pinned by the unit and integration tests).
type TaskArchiveService struct {
	crud   *StateFileService
	types  persistence.StateFileTypeRepository
	files  persistence.StateFileRepository
	phases persistence.PhaseRepository
	plans  activeTaskPlanLister
	units  activeTaskUnitLister
}

// NewTaskArchiveService wires the derivation to the P7-08 CRUD service (the
// content write path) and the phase/plan/unit repositories the project's
// completed units are read through — the same repository surface the
// ACTIVE_TASK derivation (P7-10) uses.
func NewTaskArchiveService(
	crud *StateFileService,
	phases persistence.PhaseRepository,
	plans activeTaskPlanLister,
	units activeTaskUnitLister,
) *TaskArchiveService {
	return &TaskArchiveService{
		crud:   crud,
		types:  crud.Types(),
		files:  crud.Files(),
		phases: phases,
		plans:  plans,
		units:  units,
	}
}

// DeriveTaskArchive derives the project's TASK_ARCHIVE content from its
// completed work units and writes it through the versioned content path
// (WBS 7.13): the project-scoped (TASK_ARCHIVE, PROJECT, nil associations)
// file is created on first use — one archive file per project — and the
// rendered content is appended as the next version. The work-unit rows are
// left byte-identical (the derivation is read-only over them, T7.13.5).
//
// A project with no completed units still derives the stable empty-archive
// document (header only), so the archive file exists and rebuilds are
// comparable.
//
// The append re-reads the current version on each attempt and retries on
// ErrConflict (the advisory-locked version append may lose a concurrent
// race — the bounded retry keeps the sequence contiguous).
func (s *TaskArchiveService) DeriveTaskArchive(ctx context.Context, projectID string) (*StateFileVersionView, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	units, err := s.completedUnits(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}

	file, err := s.resolveTaskArchiveFile(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	content := statefile.RenderTaskArchive(units)

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

// completedUnits lists the project's work units (every phase, every plan)
// and filters to the completed (status DONE) ones — the DB records are the
// source of the derived content (T7.13.5).
func (s *TaskArchiveService) completedUnits(ctx context.Context, projectID uuid.UUID) ([]*wbs.WorkUnit, error) {
	phases, err := s.phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var out []*wbs.WorkUnit
	for _, p := range phases {
		plans, err := s.plans.ListByPhase(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, pl := range plans {
			units, err := s.units.ListByPlan(ctx, pl.ID)
			if err != nil {
				return nil, err
			}
			for _, u := range units {
				if u.Status == wbs.WorkUnitStatusDONE {
					out = append(out, u)
				}
			}
		}
	}
	return out, nil
}

// resolveTaskArchiveFile returns the project's TASK_ARCHIVE file, creating
// it on first use. The global default type is looked up by key (P7-07
// seeds it at startup); the file is PROJECT-scoped with no associations —
// one archive file per project (the P7-07 TASK_ARCHIVE default scope).
func (s *TaskArchiveService) resolveTaskArchiveFile(ctx context.Context, projectID uuid.UUID) (*persistence.StateFileRecord, error) {
	typeID, err := s.taskArchiveTypeID(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.files.GetByTypeAndScope(ctx, projectID, typeID, string(statefile.ScopeProject), nil, nil)
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	sf, err := statefile.NewStateFile(statefile.StateFileInput{
		ProjectID: &projectID,
		TypeID:    &typeID,
		Scope:     statefile.ScopeProject,
	})
	if err != nil {
		return nil, fmt.Errorf("task-archive file: %w", err)
	}
	newRec := &persistence.StateFileRecord{
		ID:             sf.ID,
		ProjectID:      sf.ProjectID,
		TypeID:         sf.TypeID,
		Scope:          string(sf.Scope),
		CurrentVersion: sf.CurrentVersion,
		CreatedAt:      sf.CreatedAt,
		UpdatedAt:      sf.UpdatedAt,
	}
	if err := s.files.Create(ctx, newRec); err != nil {
		return nil, err
	}
	return newRec, nil
}

func (s *TaskArchiveService) taskArchiveTypeID(ctx context.Context) (uuid.UUID, error) {
	recs, err := s.types.ListByScope(ctx, nil)
	if err != nil {
		return uuid.Nil, err
	}
	for _, r := range recs {
		if strings.EqualFold(r.Key, taskArchiveTypeKey) {
			return r.ID, nil
		}
	}
	return uuid.Nil, persistence.ErrNotFound
}
