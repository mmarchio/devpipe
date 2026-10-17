package dbrepo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/project"
)

// settingsColumns is the canonical projection of the project_settings
// table.
const settingsColumns = `project_id, working_directory, worker_provider, worker_model, compression_provider, compression_model, context_window, context_threshold, auto_continue, max_retries, worker_timeout, planning_policy, planning_granularity, created_at, updated_at`

// SettingsRepository implements persistence.SettingsRepository on
// PostgreSQL.
type SettingsRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.SettingsRepository = (*SettingsRepository)(nil)

// NewSettingsRepository binds the repository to an existing pool.
func NewSettingsRepository(pool *pgxpool.Pool) *SettingsRepository {
	return &SettingsRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy of the repository that executes its operations on
// the given transaction (WBS 2.21).
func (r *SettingsRepository) WithTx(tx persistence.Transaction) persistence.SettingsRepository {
	return &SettingsRepository{exec: txExecutor{tx}}
}

// Create inserts the settings for a project (T3.2.3).
func (r *SettingsRepository) Create(ctx context.Context, s *project.Settings) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO project_settings (project_id, working_directory, worker_provider, worker_model, compression_provider, compression_model, context_window, context_threshold, auto_continue, max_retries, worker_timeout, planning_policy, planning_granularity, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		s.ProjectID, s.WorkingDirectory, s.WorkerProvider, s.WorkerModel,
		s.CompressionProvider, s.CompressionModel, s.ContextWindow,
		s.ContextThreshold, s.AutoContinue, s.MaxRetries,
		int64(s.WorkerTimeout), s.PlanningPolicy, s.PlanningGranularity,
		s.CreatedAt.UTC(), s.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create settings", err)
	}
	return nil
}

// Get returns the settings for the given project (WBS 3.3 groundwork).
func (r *SettingsRepository) Get(ctx context.Context, projectID string) (*project.Settings, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+settingsColumns+` FROM project_settings WHERE project_id = $1`, projectID)
	return scanSettings(row)
}

// Update persists changed settings and refreshes UpdatedAt (WBS 3.3
// groundwork).
func (r *SettingsRepository) Update(ctx context.Context, s *project.Settings) error {
	s.UpdatedAt = time.Now().UTC()
	if _, err := r.exec.Exec(ctx,
		`UPDATE project_settings SET working_directory = $2, worker_provider = $3, worker_model = $4, compression_provider = $5, compression_model = $6, context_window = $7, context_threshold = $8, auto_continue = $9, max_retries = $10, worker_timeout = $11, planning_policy = $12, planning_granularity = $13, updated_at = $14
		 WHERE project_id = $1`,
		s.ProjectID, s.WorkingDirectory, s.WorkerProvider, s.WorkerModel,
		s.CompressionProvider, s.CompressionModel, s.ContextWindow,
		s.ContextThreshold, s.AutoContinue, s.MaxRetries,
		int64(s.WorkerTimeout), s.PlanningPolicy, s.PlanningGranularity,
		s.UpdatedAt); err != nil {
		return TranslateDBError("update settings", err)
	}
	return nil
}

// Delete removes the settings for the given project (WBS 3.3 groundwork).
func (r *SettingsRepository) Delete(ctx context.Context, projectID string) error {
	tag, err := r.exec.Exec(ctx, `DELETE FROM project_settings WHERE project_id = $1`, projectID)
	if err != nil {
		return TranslateDBError("delete settings", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

func scanSettings(row interface{ Scan(dest ...any) error }) (*project.Settings, error) {
	var (
		s         project.Settings
		timeoutNS int64
	)
	if err := row.Scan(&s.ProjectID, &s.WorkingDirectory, &s.WorkerProvider,
		&s.WorkerModel, &s.CompressionProvider, &s.CompressionModel,
		&s.ContextWindow, &s.ContextThreshold, &s.AutoContinue,
		&s.MaxRetries, &timeoutNS, &s.PlanningPolicy, &s.PlanningGranularity,
		&s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, TranslateDBError("scan settings", err)
	}
	s.WorkerTimeout = time.Duration(timeoutNS)
	return &s, nil
}
