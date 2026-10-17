package persistence

import (
	"context"

	"overseer/internal/project"
)

// ProjectListFilter is the domain-facing project listing filter (WBS 3.22).
// Zero values select all statuses; Limit/Offset are applied by the service
// layer before reaching the repository, so implementations can assume
// sane values.
type ProjectListFilter struct {
	Status project.ProjectStatus // "" = no status filter
	Limit  int                   // max rows to return (service-applied default)
	Offset int                   // rows to skip (deterministic pagination)
}

// ProjectRepository is the domain-facing persistence contract for projects
// (WBS 2.4). PostgreSQL-specific behavior stays behind this interface.
type ProjectRepository interface {
	Create(ctx context.Context, p *project.Project) error
	Get(ctx context.Context, id string) (*project.Project, error)
	Update(ctx context.Context, p *project.Project) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, filter ProjectListFilter) ([]*project.Project, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21, 3.2).
	WithTx(tx Transaction) ProjectRepository
	// UpdateWithVersion applies an optimistic-concurrency update (WBS
	// 3.24): it only writes when the stored version equals expectedVersion,
	// increments the version on success, returns ErrNotFound when the
	// project is missing and ErrConflict when the version is stale.
	UpdateWithVersion(ctx context.Context, p *project.Project, expectedVersion int) error
}
