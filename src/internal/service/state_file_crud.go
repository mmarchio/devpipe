package service

import (
	"context"
	"errors"
	"fmt"
	"overseer/internal/bounds"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// DefaultMaxContentBytes is the hard size limit for a single state-file
// content version (WBS 7.27). It is a package constant — the limit source
// for this task (a state-file-level hard limit, not per-project config).
// Content above it is rejected; below and at the limit are accepted.
const DefaultMaxContentBytes = 1 << 20

// ErrContentLimitExceeded means the proposed content exceeds the hard size
// limit (WBS 7.27). The error message carries the offending file id and the
// byte limit, so the rejection identifies the state file (T7.27.4).
var ErrContentLimitExceeded = errors.New("service: state-file content exceeds size limit")

// StateFileService is the application-layer entry point for state-file CRUD
// (Phase 7): create file, read current/historical versions, and the
// controlled content update guarded by the source-classification mutation
// policy (WBS 7.16) and the size limits (WBS 7.27).
type StateFileService struct {
	types    persistence.StateFileTypeRepository
	files    persistence.StateFileRepository
	versions persistence.StateFileVersionRepository

	phases   persistence.PhaseRepository
	units    unitProjectLookuper
	projects persistence.ProjectRepository

	txPool *pgxpool.Pool
}

// NewStateFileService wires the state-file CRUD service to its repositories.
func NewStateFileService(
	types persistence.StateFileTypeRepository,
	files persistence.StateFileRepository,
	versions persistence.StateFileVersionRepository,
) *StateFileService {
	return &StateFileService{types: types, files: files, versions: versions}
}

// WithTxPool wires the transaction pool (additive; the single-write ops in
// this task commit directly, the pool is reserved for future multi-write
// ops).
func (s *StateFileService) WithTxPool(pool *pgxpool.Pool) *StateFileService {
	s.txPool = pool
	return s
}

// unitProjectLookuper resolves a work unit's owning project for the
// cross-project association guard (WBS 7.4 T7.4.6). *dbrepo.WorkUnitRepository
// satisfies it.
type unitProjectLookuper interface {
	Get(ctx context.Context, id uuid.UUID) (*wbs.WorkUnit, error)
}

// WithPhases wires the phase repository used to resolve a PHASE association's
// owning project (T7.4.5).
func (s *StateFileService) WithPhases(repo persistence.PhaseRepository) *StateFileService {
	s.phases = repo
	return s
}

// WithUnits wires the work-unit repository used to resolve a WORK_UNIT
// association's owning project (T7.4.6).
func (s *StateFileService) WithUnits(repo unitProjectLookuper) *StateFileService {
	s.units = repo
	return s
}

// WithProjects wires the project repository used to resolve the project id
// for the project-scoped REST surface (WBS 7.38). Unwired, the REST
// handlers pre-resolve the project themselves.
func (s *StateFileService) WithProjects(repo persistence.ProjectRepository) *StateFileService {
	s.projects = repo
	return s
}

// CreateFileInput carries the caller-supplied fields for CreateFile.
type CreateFileInput struct {
	ProjectID  string
	TypeID     string
	Scope      string
	PhaseID    *string
	WorkUnitID *string
}

// FileView is the API-facing state-file record.
type FileView struct {
	ID             string
	ProjectID      string
	TypeID         string
	Scope          string
	PhaseID        *string
	WorkUnitID     *string
	CurrentVersion int
}

// StateFileVersionView is the API-facing content-version record.
type StateFileVersionView struct {
	ID            string
	StateFileID   string
	Version       int
	Content       string
	ContentHash   string
	TokenEstimate int
}

func toFileView(r *persistence.StateFileRecord) FileView {
	v := FileView{
		ID:             r.ID.String(),
		ProjectID:      r.ProjectID.String(),
		TypeID:         r.TypeID.String(),
		Scope:          r.Scope,
		CurrentVersion: r.CurrentVersion,
	}
	if r.PhaseID != nil {
		s := r.PhaseID.String()
		v.PhaseID = &s
	}
	if r.WorkUnitID != nil {
		s := r.WorkUnitID.String()
		v.WorkUnitID = &s
	}
	return v
}

func toVersionView(r *persistence.StateFileVersionRecord) StateFileVersionView {
	return StateFileVersionView{
		ID:            r.ID.String(),
		StateFileID:   r.StateFileID.String(),
		Version:       r.Version,
		Content:       r.Content,
		ContentHash:   r.ContentHash,
		TokenEstimate: r.TokenEstimate,
	}
}

// checkAssociation enforces the domain-level cross-project association guard
// (WBS 7.4 T7.4.5/T7.4.6): a PHASE-scoped file's phase, and a WORK_UNIT-scoped
// file's work unit, must belong to the file's own project. The owner is
// resolved through the wired repositories; an unresolved owner is
// ErrNotFound, a foreign owner is ErrInvalidReference. An unwired
// repository degrades to no check (the DB FK still pins row existence).
func (s *StateFileService) checkAssociation(ctx context.Context, f *statefile.StateFile) error {
	switch f.Scope {
	case statefile.ScopePhase:
		if f.PhaseID == nil || s.phases == nil {
			return nil
		}
		ph, err := s.phases.Get(ctx, *f.PhaseID)
		if err != nil {
			return translate(err)
		}
		if ph.ProjectID != f.ProjectID {
			return ErrInvalidReference
		}
	case statefile.ScopeWorkUnit:
		if f.WorkUnitID == nil || s.units == nil {
			return nil
		}
		u, err := s.units.Get(ctx, *f.WorkUnitID)
		if err != nil {
			return translate(err)
		}
		if u.ProjectID != f.ProjectID {
			return ErrInvalidReference
		}
	}
	return nil
}

// Types exposes the type repository (additive; consumed by the P7-10
// active-task derivation to resolve the global ACTIVE_TASK type).
func (s *StateFileService) Types() persistence.StateFileTypeRepository { return s.types }

// Files exposes the file repository (additive; consumed by the P7-10
// active-task derivation to resolve/create the per-unit file).
func (s *StateFileService) Files() persistence.StateFileRepository { return s.files }

// CreateFile creates a logical state file (WBS 7.4): the domain guard runs
// first (type required, project required, scope association valid), then the
// row is persisted. Cross-project phase/work-unit associations are rejected
// by the database FK and surface as ErrInvalidReference (T7.4.5/T7.4.6); a
// duplicate (type, scope, association) surfaces as ErrConflict.
func (s *StateFileService) CreateFile(ctx context.Context, in CreateFileInput) (*FileView, error) {
	projectID, err := uuid.Parse(in.ProjectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	typeID, err := uuid.Parse(in.TypeID)
	if err != nil {
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
	sf, err := statefile.NewStateFile(statefile.StateFileInput{
		ProjectID:  &projectID,
		TypeID:     &typeID,
		Scope:      statefile.StateFileScope(in.Scope),
		PhaseID:    phaseID,
		WorkUnitID: unitID,
	})
	if err != nil {
		return nil, ErrInvalidInput
	}
	if err := s.checkAssociation(ctx, sf); err != nil {
		return nil, err
	}
	rec := &persistence.StateFileRecord{
		ID:             sf.ID,
		ProjectID:      sf.ProjectID,
		TypeID:         sf.TypeID,
		Scope:          string(sf.Scope),
		PhaseID:        sf.PhaseID,
		WorkUnitID:     sf.WorkUnitID,
		CurrentVersion: sf.CurrentVersion,
		CreatedAt:      sf.CreatedAt,
		UpdatedAt:      sf.UpdatedAt,
	}
	if err := s.files.Create(ctx, rec); err != nil {
		return nil, translate(err)
	}
	v := toFileView(rec)
	return &v, nil
}

// FileListEntry is the API-facing list row (WBS 7.38): the file identity,
// the type's key and source classification, scope, association, and the
// file's current content version.
type FileListEntry struct {
	ID             string
	TypeID         string
	TypeKey        string
	Source         string
	Scope          string
	PhaseID        *string
	WorkUnitID     *string
	CurrentVersion int
}

// ListProjectFiles lists the project's state files (WBS 7.38) with each
// file's type key and source classification. An unknown project is
// ErrNotFound (the project is resolved first, so a foreign project's
// files are never listed).
func (s *StateFileService) ListProjectFiles(ctx context.Context, projectID string) ([]*FileListEntry, error) {
	id, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrNotFound
	}
	if s.projects != nil {
		if _, perr := s.projects.Get(ctx, projectID); perr != nil {
			return nil, translate(perr)
		}
	}
	files, err := s.files.ListByProject(ctx, id)
	if err != nil {
		return nil, translate(err)
	}
	entries := make([]*FileListEntry, 0, len(files))
	for _, f := range files {
		typeRec, err := s.types.Get(ctx, f.TypeID)
		if err != nil {
			return nil, translate(err)
		}
		e := &FileListEntry{
			ID:             f.ID.String(),
			TypeID:         f.TypeID.String(),
			TypeKey:        typeRec.Key,
			Source:         typeRec.Source,
			Scope:          f.Scope,
			CurrentVersion: f.CurrentVersion,
		}
		if f.PhaseID != nil {
			str := f.PhaseID.String()
			e.PhaseID = &str
		}
		if f.WorkUnitID != nil {
			str := f.WorkUnitID.String()
			e.WorkUnitID = &str
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// GetFile returns one of the project's files by id (WBS 7.38) together
// with its type's key and source. A file id that belongs to a DIFFERENT
// project is ErrNotFound (project scoping is enforced, not just row
// existence), as is a malformed/unknown id.
func (s *StateFileService) GetFile(ctx context.Context, projectID, fileID string) (rec *persistence.StateFileRecord, typeKey, source string, err error) {
	id, perr := uuid.Parse(fileID)
	if perr != nil {
		return nil, "", "", ErrNotFound
	}
	rec, err = s.files.Get(ctx, id)
	if err != nil {
		return nil, "", "", translate(err)
	}
	recProject, perr := uuid.Parse(projectID)
	if perr != nil || rec.ProjectID != recProject {
		return nil, "", "", ErrNotFound
	}
	typeRec, terr := s.types.Get(ctx, rec.TypeID)
	if terr != nil {
		return nil, "", "", translate(terr)
	}
	return rec, typeRec.Key, typeRec.Source, nil
}

// GetFileVersion returns one stored version of a project's file
// (WBS 7.38); the project-scope check applies (a foreign file is
// ErrNotFound), and a missing version is ErrNotFound.
func (s *StateFileService) GetFileVersion(ctx context.Context, projectID, fileID string, version int) (*StateFileVersionView, error) {
	id, err := uuid.Parse(fileID)
	if err != nil {
		return nil, ErrNotFound
	}
	rec, err := s.files.Get(ctx, id)
	if err != nil {
		return nil, translate(err)
	}
	recProject, perr := uuid.Parse(projectID)
	if perr != nil || rec.ProjectID != recProject {
		return nil, ErrNotFound
	}
	r, err := s.versions.GetVersion(ctx, id, version)
	if err != nil {
		return nil, translate(err)
	}
	v := toVersionView(r)
	return &v, nil
}

// ListFileVersions lists a project's file's stored versions in ascending
// order (WBS 7.38) with the same project-scope rule as GetFileVersion.
func (s *StateFileService) ListFileVersions(ctx context.Context, projectID, fileID string) ([]*StateFileVersionView, error) {
	id, err := uuid.Parse(fileID)
	if err != nil {
		return nil, ErrNotFound
	}
	rec, err := s.files.Get(ctx, id)
	if err != nil {
		return nil, translate(err)
	}
	recProject, perr := uuid.Parse(projectID)
	if perr != nil || rec.ProjectID != recProject {
		return nil, ErrNotFound
	}
	rs, err := s.versions.ListVersions(ctx, id)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]*StateFileVersionView, 0, len(rs))
	for _, r := range rs {
		v := toVersionView(r)
		out = append(out, &v)
	}
	return out, nil
}

// GetCurrentVersion returns the file's current content version; (nil, nil)
// for a file without content (P7-06 contract).
func (s *StateFileService) GetCurrentVersion(ctx context.Context, id string) (*StateFileVersionView, error) {
	fileID, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrInvalidInput
	}
	r, err := s.versions.GetCurrentVersion(ctx, fileID)
	if err != nil {
		return nil, translate(err)
	}
	if r == nil {
		return nil, nil
	}
	v := toVersionView(r)
	return &v, nil
}

// GetVersion returns a stored content version (current or historical).
func (s *StateFileService) GetVersion(ctx context.Context, id string, version int) (*StateFileVersionView, error) {
	fileID, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrInvalidInput
	}
	r, err := s.versions.GetVersion(ctx, fileID, version)
	if err != nil {
		return nil, translate(err)
	}
	v := toVersionView(r)
	return &v, nil
}

// UpdateContent is the controlled content-update path (WBS 7.16/7.27). The
// source-classification policy is consumed verbatim from P7-04: a class
// without a controlled-update path (DERIVED) is refused BEFORE any write
// (T7.16.3); MANAGED/USER/EXTERNAL permit it (T7.16.4). The size limit
// applies to the exact content bytes (WBS 7.27): above the hard limit the
// update is refused with the offending file identified (T7.27.4) and the
// stored content is left untouched (T7.27.5). A permitted update appends the
// next version via the P7-06 repository (optimistic concurrency: a stale
// expectedVersion is ErrConflict).
func (s *StateFileService) UpdateContent(ctx context.Context, id, content string, expectedVersion int) (*StateFileVersionView, error) {
	fileID, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rec, err := s.files.Get(ctx, fileID)
	if err != nil {
		return nil, translate(err)
	}
	typeRec, err := s.types.Get(ctx, rec.TypeID)
	if err != nil {
		return nil, translate(err)
	}
	if !statefile.ControlledUpdateAllowed(statefile.SourceClass(typeRec.Source)) {
		return nil, ErrInvalidInput
	}
	if len(content) > DefaultMaxContentBytes {
		return nil, fmt.Errorf("%w: file %s (%d bytes > limit %d)", ErrContentLimitExceeded, rec.ID, len(content), DefaultMaxContentBytes)
	}
	// Text validity (UTF-8, no NUL; WBS 18.89) — the size limit above is
	// the Phase 7 one.
	if err := bounds.Check(bounds.StateFile, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	stored, err := s.versions.UpdateContent(ctx, fileID, content, expectedVersion)
	if err != nil {
		return nil, translate(err)
	}
	v := toVersionView(stored)
	return &v, nil
}

// SeedDefaultFileTypes seeds the six GLOBAL default state-file types as
// data (WBS 7.2): for each default, the global row is created when its key
// is absent from the global scope. It is idempotent — re-running (or a
// server restart) creates no duplicates and never mutates existing rows —
// and a project-scoped type with the same key does not satisfy the check,
// so it leaves the global default untouched (T7.2.1-T7.2.4).
func (s *StateFileService) SeedDefaultFileTypes(ctx context.Context) error {
	existing, err := s.types.ListByScope(ctx, nil)
	if err != nil {
		return fmt.Errorf("seed state-file types: list global scope: %w", err)
	}
	present := make(map[string]bool, len(existing))
	for _, r := range existing {
		present[r.Key] = true
	}
	for _, def := range statefile.DefaultFileTypes() {
		if present[def.Key] {
			continue
		}
		ft, err := statefile.NewFileType(statefile.StateFileTypeInput{
			Key:             def.Key,
			Name:            def.Name,
			DefaultFilename: def.DefaultFilename,
			Scope:           def.Scope,
		})
		if err != nil {
			return fmt.Errorf("seed state-file type %s: %w", def.Key, err)
		}
		rec := &persistence.StateFileTypeRecord{
			ID:              ft.ID,
			Key:             ft.Key,
			Name:            ft.Name,
			Description:     ft.Description,
			DefaultFilename: ft.DefaultFilename,
			Scope:           string(ft.Scope),
			Source:          string(ft.Source),
			Required:        ft.Required,
			Enabled:         ft.Enabled,
			CreatedAt:       ft.CreatedAt,
			UpdatedAt:       ft.UpdatedAt,
		}
		if err := s.types.Create(ctx, rec); err != nil {
			return fmt.Errorf("seed state-file type %s: %w", def.Key, err)
		}
	}
	return nil
}
