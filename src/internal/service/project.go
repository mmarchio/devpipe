package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
)

// Default project-creation settings (WBS 3.2: "configured/default values").
// These are the service-level defaults applied when a project is created;
// later phases (P3-03) make them updatable.
const (
	// DefaultContextWindow is the default context window size.
	DefaultContextWindow = 200000
	// DefaultContextThreshold is the default context-usage threshold.
	DefaultContextThreshold = 0.8
	// DefaultMaxRetries is the default worker retry count.
	DefaultMaxRetries = 0
	// DefaultWorkerTimeout is the default worker operation timeout.
	DefaultWorkerTimeout = 5 * time.Minute
)

// ProjectService is the application-level project service (WBS 3.1/3.2):
// it validates input against the domain, delegates storage to the
// repositories, and translates repository failures into stable
// service-level errors.
type ProjectService struct {
	projects persistence.ProjectRepository
	settings persistence.SettingsRepository
	// txPool is the transaction seam: operations that must be atomic
	// coordinate through it (WBS 3.2).
	txPool *pgxpool.Pool
	// contextThreshold is the default for new projects (0 = the
	// DefaultContextThreshold constant; configurable, WBS 18.2).
	contextThreshold float64
}

// WithDefaultContextThreshold sets the context threshold applied to new
// projects (0 keeps DefaultContextThreshold).
func (s *ProjectService) WithDefaultContextThreshold(t float64) *ProjectService {
	s.contextThreshold = t
	return s
}

func (s *ProjectService) defaultThreshold() float64 {
	if s.contextThreshold > 0 {
		return s.contextThreshold
	}
	return DefaultContextThreshold
}

// NewProjectService wires the service to the project and settings
// repositories and the transaction pool.
func NewProjectService(projects persistence.ProjectRepository, settings persistence.SettingsRepository, txPool *pgxpool.Pool) *ProjectService {
	return &ProjectService{projects: projects, settings: settings, txPool: txPool}
}

// WithSettingsRepo returns a copy of the service bound to a different
// settings repository (test seam for the rollback path, WBS 3.2).
func (s *ProjectService) WithSettingsRepo(settings persistence.SettingsRepository) *ProjectService {
	return &ProjectService{projects: s.projects, settings: settings, txPool: s.txPool, contextThreshold: s.contextThreshold}
}

// CreateProject is the single transactional project-creation operation
// (WBS 3.2): it validates the input, then creates a DRAFT project and its
// default ProjectSettings in one transaction. If settings creation fails
// the project is rolled back — no project is ever left without its
// required settings (T3.2.1–T3.2.6).
func (s *ProjectService) CreateProject(ctx context.Context, name, description string) (*project.Project, *project.Settings, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, ErrInvalidInput
	}
	p, err := project.New(name, description)
	if err != nil {
		return nil, nil, ErrInvalidInput
	}
	st, err := project.NewSettings(
		p.ID,
		"", // working directory is configured post-creation (P3-04)
		"", "",
		"", "",
		DefaultContextWindow,
		s.defaultThreshold(),
		false,
		DefaultMaxRetries,
		DefaultWorkerTimeout,
	)
	if err != nil {
		// Defaults must always satisfy domain validation; reaching here
		// is a programming error.
		return nil, nil, ErrInvalidInput
	}

	var created *project.Project
	if s.txPool == nil {
		// No transaction pool injected (unit scope): fall back to
		// sequential repository calls. Production always injects a pool,
		// where creation is atomic.
		if err := s.projects.Create(ctx, p); err != nil {
			return nil, nil, translate(err)
		}
		if s.settings != nil {
			if err := s.settings.Create(ctx, st); err != nil {
				return nil, nil, translate(err)
			}
		}
		created = p
		return created, st, nil
	}
	if err := s.RunInTx(ctx, func(tx persistence.Transaction) error {
		projRepo := s.projects.WithTx(tx)
		setRepo := s.settings.WithTx(tx)
		if err := projRepo.Create(ctx, p); err != nil {
			return translate(err)
		}
		if err := setRepo.Create(ctx, st); err != nil {
			return translate(err)
		}
		created = p
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return created, st, nil
}

// GetProject returns the project with the given id (T3.1.4).
func (s *ProjectService) GetProject(ctx context.Context, id string) (*project.Project, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrInvalidInput
	}
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	return p, nil
}

// DefaultListLimit and MaxListLimit bound listing pagination (WBS 3.22):
// a zero limit means "default", a negative or oversized limit is rejected
// as invalid input rather than passed to the repository.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ListProjects returns projects matching the given filter (T3.1.5, extended
// for WBS 3.22). The service applies the pagination bounds so the
// repository only ever sees sane values.
func (s *ProjectService) ListProjects(ctx context.Context, filter persistence.ProjectListFilter) ([]*project.Project, error) {
	if filter.Status != "" {
		if _, err := project.ParseStatus(string(filter.Status)); err != nil {
			return nil, ErrInvalidInput
		}
	}
	if filter.Offset < 0 {
		return nil, ErrInvalidInput
	}
	if filter.Limit <= 0 {
		filter.Limit = DefaultListLimit
	}
	if filter.Limit > MaxListLimit {
		return nil, ErrInvalidInput
	}
	out, err := s.projects.List(ctx, filter)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// UpdateProject validates and persists changed project fields (T3.1.6).
func (s *ProjectService) UpdateProject(ctx context.Context, p *project.Project) (*project.Project, error) {
	if p == nil || p.ID == uuid.Nil || strings.TrimSpace(p.Name) == "" {
		return nil, ErrInvalidInput
	}
	if _, err := project.ParseStatus(p.Status.String()); err != nil {
		return nil, ErrInvalidInput
	}
	if err := s.projects.Update(ctx, p); err != nil {
		return nil, translate(err)
	}
	return p, nil
}

// deletableStatuses is the deletion-eligibility policy (WBS 3.13): every
// status without a live or resumable execution is deletable — DRAFT, READY,
// CANCELLED and the terminal COMPLETED / FAILED. RUNNING and PAUSED are
// protected (cancel first) so an in-flight engine step or worker dispatch
// never loses its rows. Deletion cascades to every project-owned row; the
// audit log is kept (it is not owned by the project, WBS 17.103).
var deletableStatuses = map[project.ProjectStatus]bool{
	project.StatusDRAFT:     true,
	project.StatusREADY:     true,
	project.StatusCANCELLED: true,
	project.StatusCOMPLETED: true,
	project.StatusFAILED:    true,
}

// ProjectDeletable reports whether a project in status may be deleted.
func ProjectDeletable(status string) bool {
	return deletableStatuses[project.ProjectStatus(status)]
}

// UpdateProjectWithVersion persists changed project fields guarded by an
// optimistic-concurrency version (WBS 3.24, T3.24.1-T3.24.5). It writes only
// when the stored version equals expectedVersion and increments the version
// atomically. A stale expectedVersion is ErrConflict; a missing project is
// ErrNotFound. The returned project carries the incremented version.
func (s *ProjectService) UpdateProjectWithVersion(ctx context.Context, p *project.Project, expectedVersion int) (*project.Project, error) {
	if p == nil || p.ID == uuid.Nil || strings.TrimSpace(p.Name) == "" {
		return nil, ErrInvalidInput
	}
	if _, err := project.ParseStatus(p.Status.String()); err != nil {
		return nil, ErrInvalidInput
	}
	if err := s.projects.UpdateWithVersion(ctx, p, expectedVersion); err != nil {
		return nil, translate(err)
	}
	p.Version = expectedVersion + 1
	return p, nil
}

// DeleteProject removes the project and, by cascade, everything it owns if
// its status permits (WBS 3.13, T3.1.7, T3.13.1–T3.13.5). A protected status
// (RUNNING, PAUSED) is rejected with ErrConflict and nothing is deleted.
func (s *ProjectService) DeleteProject(ctx context.Context, id string) (*project.Project, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrInvalidInput
	}
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	if !deletableStatuses[p.Status] {
		return nil, ErrConflict
	}
	if err := s.projects.Delete(ctx, parsed.String()); err != nil {
		return nil, translate(err)
	}
	return &project.Project{ID: parsed}, nil
}

// RunInTx executes fn inside a database transaction, committing on nil and
// rolling back on error or panic (T3.1.8). This is the transaction
// coordination seam the service uses where atomicity is required.
func (s *ProjectService) RunInTx(ctx context.Context, fn func(tx persistence.Transaction) error) error {
	return dbrepo.Run(ctx, s.txPool, fn)
}
