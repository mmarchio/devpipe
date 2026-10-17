// P15-42 / P15-43 (WBS 15.43–15.49, 15.109): bootstrap state-file
// initialization. Every derived file is written through the Phase 7
// versioned content path, and a new version is appended only when the
// rendered content differs from the current version — re-running the stage
// (retry / restart) never duplicates logical files or versions.
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// derivedFileWriter writes derived state-file content idempotently.
type derivedFileWriter struct {
	crud *StateFileService
}

// typeByKey resolves an enabled state-file type by key: a project-scoped
// type shadows the global default.
func (w derivedFileWriter) typeByKey(ctx context.Context, projectID uuid.UUID, key string) (*persistence.StateFileTypeRecord, error) {
	proj, err := w.crud.Types().ListByScope(ctx, &projectID)
	if err != nil {
		return nil, err
	}
	for _, r := range proj {
		if r.Key == key {
			return r, nil
		}
	}
	global, err := w.crud.Types().ListByScope(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range global {
		if r.Key == key {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: state-file type %s", persistence.ErrNotFound, key)
}

// file returns (creating on first use) the project's file of the type at
// the given scope association.
func (w derivedFileWriter) file(ctx context.Context, projectID uuid.UUID, typ *persistence.StateFileTypeRecord, phaseID, unitID *uuid.UUID) (*persistence.StateFileRecord, error) {
	scope := typ.Scope
	switch statefile.StateFileScope(scope) {
	case statefile.ScopeProject:
		phaseID, unitID = nil, nil
	case statefile.ScopePhase:
		unitID = nil
	case statefile.ScopeWorkUnit:
		phaseID = nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		rec, err := w.crud.Files().GetByTypeAndScope(ctx, projectID, typ.ID, scope, phaseID, unitID)
		if err == nil {
			return rec, nil
		}
		if !errors.Is(err, persistence.ErrNotFound) {
			return nil, err
		}
		tid := typ.ID
		sf, err := statefile.NewStateFile(statefile.StateFileInput{ProjectID: &projectID, TypeID: &tid,
			Scope: statefile.StateFileScope(scope), PhaseID: phaseID, WorkUnitID: unitID})
		if err != nil {
			return nil, err
		}
		nr := &persistence.StateFileRecord{ID: sf.ID, ProjectID: projectID, TypeID: tid, Scope: scope,
			PhaseID: sf.PhaseID, WorkUnitID: sf.WorkUnitID, CurrentVersion: sf.CurrentVersion, CreatedAt: sf.CreatedAt, UpdatedAt: sf.UpdatedAt}
		if err := w.crud.Files().Create(ctx, nr); err != nil {
			if errors.Is(err, persistence.ErrConflict) {
				continue // a concurrent bootstrap created it
			}
			return nil, err
		}
		return nr, nil
	}
	return nil, ErrConflict
}

// write stores content as the file's current version unless it already is.
func (w derivedFileWriter) write(ctx context.Context, file *persistence.StateFileRecord, typeKey, content string) (execution.FileRef, error) {
	for attempt := 0; attempt < 5; attempt++ {
		cur, err := w.crud.GetCurrentVersion(ctx, file.ID.String())
		if err != nil {
			return execution.FileRef{}, err
		}
		if cur != nil && cur.Content == content {
			id, _ := uuid.Parse(cur.ID)
			return execution.FileRef{Type: typeKey, VersionID: id, ContentHash: cur.ContentHash}, nil
		}
		expected := 0
		if cur != nil {
			expected = cur.Version
		}
		v, err := w.crud.UpdateContent(ctx, file.ID.String(), content, expected)
		if err == nil {
			id, _ := uuid.Parse(v.ID)
			return execution.FileRef{Type: typeKey, VersionID: id, ContentHash: v.ContentHash}, nil
		}
		if !errors.Is(err, ErrConflict) {
			return execution.FileRef{}, err
		}
	}
	return execution.FileRef{}, ErrConflict
}

// Derive writes content into the (type, scope) file idempotently.
func (w derivedFileWriter) Derive(ctx context.Context, projectID uuid.UUID, typeKey string, phaseID, unitID *uuid.UUID, content string) (execution.FileRef, error) {
	typ, err := w.typeByKey(ctx, projectID, typeKey)
	if err != nil {
		return execution.FileRef{}, err
	}
	f, err := w.file(ctx, projectID, typ, phaseID, unitID)
	if err != nil {
		return execution.FileRef{}, err
	}
	return w.write(ctx, f, typeKey, content)
}

// Current returns the current version of an existing (type, scope) file
// without writing (ARCHITECTURE reuse, WBS 15.44/15.50); ok=false when the
// file or its content is absent.
func (w derivedFileWriter) Current(ctx context.Context, projectID uuid.UUID, typeKey string, phaseID, unitID *uuid.UUID) (execution.FileRef, bool, error) {
	typ, err := w.typeByKey(ctx, projectID, typeKey)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return execution.FileRef{}, false, nil
		}
		return execution.FileRef{}, false, err
	}
	switch statefile.StateFileScope(typ.Scope) {
	case statefile.ScopeProject:
		phaseID, unitID = nil, nil
	case statefile.ScopePhase:
		unitID = nil
	case statefile.ScopeWorkUnit:
		phaseID = nil
	}
	rec, err := w.crud.Files().GetByTypeAndScope(ctx, projectID, typ.ID, typ.Scope, phaseID, unitID)
	if errors.Is(err, persistence.ErrNotFound) {
		return execution.FileRef{}, false, nil
	}
	if err != nil {
		return execution.FileRef{}, false, err
	}
	cur, err := w.crud.GetCurrentVersion(ctx, rec.ID.String())
	if err != nil || cur == nil {
		return execution.FileRef{}, false, err
	}
	id, _ := uuid.Parse(cur.ID)
	return execution.FileRef{Type: typeKey, VersionID: id, ContentHash: cur.ContentHash}, true, nil
}

// DeriveActiveTaskForUnit writes ACTIVE_TASK for an explicit unit
// (P15-42, WBS 15.47): same rendering as DeriveActiveTask, idempotent.
func (s *ActiveTaskService) DeriveActiveTaskForUnit(ctx context.Context, projectID uuid.UUID, unit *wbs.WorkUnit) (execution.FileRef, error) {
	if unit == nil {
		return execution.FileRef{}, ErrInvalidInput
	}
	file, err := s.resolveActiveTaskFile(ctx, projectID, unit)
	if err != nil {
		return execution.FileRef{}, translate(err)
	}
	return derivedFileWriter{crud: s.crud}.write(ctx, file, activeTaskTypeKey, statefile.RenderActiveTask(unit))
}
