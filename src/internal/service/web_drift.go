package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/statefile"
)

// Drift resolution options (WBS 17.47).
const (
	DriftRestore = "RESTORE"
	DriftImport  = "IMPORT"
	DriftIgnore  = "IGNORE"
)

// ErrDerivedNotImportable is a 409: a DERIVED file cannot take filesystem
// content as authority (WBS 17.47).
var ErrDerivedNotImportable = fmt.Errorf("%w: derived state file cannot import filesystem content", ErrConflict)

// DriftView is the drift indicator of one state file (WBS 17.46).
type DriftView struct {
	StateFileID    string   `json:"state_file_id"`
	Status         string   `json:"status"`
	Path           string   `json:"path"`
	CurrentVersion int      `json:"current_version"`
	ContentHash    string   `json:"content_hash"`
	FSHash         string   `json:"filesystem_hash,omitempty"`
	SourceClass    string   `json:"source_class"`
	Ignored        bool     `json:"ignored"`
	IgnoredBy      string   `json:"ignored_by,omitempty"`
	Options        []string `json:"options"`
}

// DriftResolutionService resolves state-file drift where policy permits
// (WBS 17.46 / 17.47, P17-44): RESTORE re-materializes the managed version,
// IMPORT (non-DERIVED only) makes the filesystem content a new version,
// IGNORE acknowledges the current filesystem hash.
type DriftResolutionService struct {
	mat  *StateFileMaterializeService
	set  *SettingsService
	acks persistence.DriftAckRepository
}

// NewDriftResolutionService builds the service.
func NewDriftResolutionService(mat *StateFileMaterializeService, set *SettingsService, acks persistence.DriftAckRepository) *DriftResolutionService {
	return &DriftResolutionService{mat: mat, set: set, acks: acks}
}

func (s *DriftResolutionService) root(ctx context.Context, projectID string) (string, error) {
	st, err := s.set.GetSettings(ctx, projectID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(st.WorkingDirectory) == "" {
		return "", fmt.Errorf("%w: project has no working directory", ErrConflict)
	}
	return st.WorkingDirectory, nil
}

// sourceClass returns the file's type source class (project-scoped).
func (s *DriftResolutionService) sourceClass(ctx context.Context, projectID, fileID uuid.UUID) (statefile.SourceClass, *persistence.StateFileRecord, error) {
	rec, err := s.mat.files.Get(ctx, fileID)
	if err != nil {
		return "", nil, translate(err)
	}
	if rec.ProjectID != projectID {
		return "", nil, ErrNotFound
	}
	t, err := s.mat.types.Get(ctx, rec.TypeID)
	if err != nil {
		return "", nil, translate(err)
	}
	return statefile.SourceClass(t.Source), rec, nil
}

// Status returns the drift indicator and the options policy permits.
func (s *DriftResolutionService) Status(ctx context.Context, projectID, fileID uuid.UUID) (*DriftView, error) {
	class, _, err := s.sourceClass(ctx, projectID, fileID)
	if err != nil {
		return nil, err
	}
	root, err := s.root(ctx, projectID.String())
	if err != nil {
		return nil, err
	}
	d, err := s.mat.DetectDrift(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		return nil, err
	}
	v := &DriftView{StateFileID: fileID.String(), Status: string(d.Status), Path: d.Path, CurrentVersion: d.CurrentVersion,
		ContentHash: d.ContentHash, FSHash: d.FSHash, SourceClass: string(class), Options: []string{}}
	if d.Status == DriftDrifted && d.FSHash != "" {
		if a, err := s.acks.GetDriftAck(ctx, fileID, d.FSHash); err == nil {
			v.Ignored, v.IgnoredBy = true, a.AcknowledgedBy
		}
	}
	switch d.Status {
	case DriftDrifted:
		v.Options = append(v.Options, DriftRestore)
		if statefile.ControlledUpdateAllowed(class) {
			v.Options = append(v.Options, DriftImport)
		}
		if !v.Ignored {
			v.Options = append(v.Options, DriftIgnore)
		}
	case DriftMissing:
		v.Options = append(v.Options, DriftRestore)
	}
	return v, nil
}

// Resolve performs one option; it must be offered by Status.
func (s *DriftResolutionService) Resolve(ctx context.Context, projectID, fileID uuid.UUID, option, by, reason string) (*DriftView, error) {
	v, err := s.Status(ctx, projectID, fileID)
	if err != nil {
		return nil, err
	}
	if option == DriftImport && v.SourceClass == string(statefile.SourceDerived) {
		return nil, ErrDerivedNotImportable
	}
	offered := false
	for _, o := range v.Options {
		offered = offered || o == option
	}
	if !offered {
		return nil, fmt.Errorf("%w: %s not available for a %s file", ErrConflict, option, v.Status)
	}
	root, err := s.root(ctx, projectID.String())
	if err != nil {
		return nil, err
	}
	switch option {
	case DriftRestore:
		if err := s.mat.restoreOne(ctx, root, projectID, fileID); err != nil {
			return nil, err
		}
	case DriftImport:
		if _, err := s.mat.Import(ctx, root, projectID.String(), fileID.String()); err != nil {
			return nil, err
		}
	case DriftIgnore:
		if by == "" {
			by = "operator"
		}
		if _, err := s.acks.AcknowledgeDrift(ctx, &persistence.DriftAcknowledgement{ProjectID: projectID, StateFileID: fileID,
			FSHash: v.FSHash, AcknowledgedBy: by, Reason: reason, CreatedAt: time.Now().UTC()}); err != nil {
			return nil, translate(err)
		}
	}
	return s.Status(ctx, projectID, fileID)
}

// restoreOne re-materializes one file's persisted current version at its
// managed path (the managed version stays authoritative).
func (s *StateFileMaterializeService) restoreOne(ctx context.Context, root string, projectID, fileID uuid.UUID) error {
	rec, err := s.files.Get(ctx, fileID)
	if err != nil {
		return translate(err)
	}
	if rec.ProjectID != projectID {
		return ErrNotFound
	}
	path, _, err := s.resolveMaterializedPath(ctx, root, rec)
	if err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("%w: file has no materialization path", ErrConflict)
	}
	ver, err := s.versions.GetCurrentVersion(ctx, fileID)
	if err != nil {
		return translate(err)
	}
	if ver == nil {
		return fmt.Errorf("%w: file has no content version", ErrConflict)
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return fmt.Errorf("restore %s: %w", path, err)
	}
	if err := s.WriteFile(path, []byte(ver.Content)); err != nil {
		return fmt.Errorf("restore %s: %w", path, err)
	}
	if s.meta != nil {
		if err := s.meta.Upsert(ctx, &persistence.MaterializedStateFileRecord{StateFileID: fileID, Version: ver.Version,
			ContentHash: ver.ContentHash, Path: path, MaterializedAt: time.Now().UTC()}); err != nil {
			return translate(err)
		}
	}
	return nil
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}
