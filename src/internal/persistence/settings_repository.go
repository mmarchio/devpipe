package persistence

import (
	"context"

	"overseer/internal/project"
)

// SettingsRepository is the domain-facing persistence contract for
// per-project settings (WBS 3.2). PostgreSQL-specific behavior stays
// behind this interface.
type SettingsRepository interface {
	Create(ctx context.Context, s *project.Settings) error
	Get(ctx context.Context, projectID string) (*project.Settings, error)
	Update(ctx context.Context, s *project.Settings) error
	Delete(ctx context.Context, projectID string) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21, 3.2).
	WithTx(tx Transaction) SettingsRepository
}
