package service

import (
	"time"

	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
)

// SettingsService provides application operations for retrieving and
// modifying project settings (WBS 3.3).
type SettingsService struct {
	settings persistence.SettingsRepository
	projects persistence.ProjectRepository
	txPool   *pgxpool.Pool
	// onUpdated runs after a successful settings update (the app kicks
	// the engine scheduler so enabling auto-continue takes effect now).
	onUpdated func(*project.Settings)
}

// SetUpdateHook installs the callback run after each settings update.
func (s *SettingsService) SetUpdateHook(h func(*project.Settings)) { s.onUpdated = h }

// NewSettingsService wires the settings service to its repositories and
// the transaction pool.
func NewSettingsService(
	settings persistence.SettingsRepository,
	projects persistence.ProjectRepository,
	txPool *pgxpool.Pool,
) *SettingsService {
	return &SettingsService{settings: settings, projects: projects, txPool: txPool}
}

// GetSettings returns the settings for the given project (T3.3.1).
func (s *SettingsService) GetSettings(ctx context.Context, projectID string) (*project.Settings, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	st, err := s.settings.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	return st, nil
}

// GetProjectVersion returns the project's optimistic-concurrency version
// (WBS 3.24) so the settings API can expose it to clients.
func (s *SettingsService) GetProjectVersion(ctx context.Context, projectID string) (int, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return 0, ErrInvalidInput
	}
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return 0, translate(err)
	}
	return p.Version, nil
}

// protectedChanged reports whether the desired settings change any
// execution-affecting field relative to the current ones. Per WBS 3.3
// Phase 3, these are the fields whose change is rejected while the project
// is RUNNING; provider/model strings and auto-continue are not
// execution-affecting in this phase.
func protectedChanged(current, desired *project.Settings) bool {
	return current.WorkingDirectory != desired.WorkingDirectory ||
		current.ContextWindow != desired.ContextWindow ||
		current.ContextThreshold != desired.ContextThreshold ||
		current.MaxRetries != desired.MaxRetries ||
		current.WorkerTimeout != desired.WorkerTimeout
}

// UpdateSettings validates and persists a new settings state (T3.3.2–T3.3.8).
// Domain validation is enforced through project.NewSettings; changes to
// protected (execution-affecting) fields of a RUNNING project are
// rejected; the update is executed atomically — the project status check
// and the settings write happen in one transaction.
// expectedVersion is the project version the client read before composing the
// update (WBS 3.24); a mismatch is ErrConflict and leaves both settings and
// the version untouched.
func (s *SettingsService) UpdateSettings(ctx context.Context, st *project.Settings, expectedVersion int) (*project.Settings, error) {
	if st == nil || st.ProjectID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	// Domain validation: the desired state must satisfy all field rules.
	valid, err := project.NewSettings(
		st.ProjectID,
		st.WorkingDirectory, st.WorkerProvider, st.WorkerModel,
		st.CompressionProvider, st.CompressionModel,
		st.ContextWindow, st.ContextThreshold,
		st.AutoContinue, st.MaxRetries, st.WorkerTimeout,
	)
	if err != nil {
		return nil, ErrInvalidInput
	}

	do := func(projRepo persistence.ProjectRepository, setRepo persistence.SettingsRepository) error {
		cur, err := setRepo.Get(ctx, st.ProjectID.String())
		if err != nil {
			return translate(err)
		}
		// Planning policy / granularity are written only by UpdatePlanning
		// (P17-22): a general settings update keeps them.
		valid.PlanningPolicy, valid.PlanningGranularity = cur.PlanningPolicy, cur.PlanningGranularity
		p, err := projRepo.Get(ctx, st.ProjectID.String())
		if err != nil {
			return translate(err)
		}
		if p.Status == project.StatusRUNNING && protectedChanged(cur, valid) {
			// WBS 3.15 (T3.15.5): a protected change while RUNNING is a
			// state conflict (409), distinct from invalid input (400).
			return ErrConflict
		}
		if p.Version != expectedVersion {
			// WBS 3.24 (T3.24.3/T3.24.5): a stale version is a conflict; the
			// settings write below never runs, so current data is preserved.
			return ErrConflict
		}
		if err := setRepo.Update(ctx, valid); err != nil {
			return translate(err)
		}
		// The project version guards settings as well (WBS 3.24): bump it
		// atomically with the settings write, re-checking the version so a
		// concurrent update cannot be lost.
		if err := projRepo.UpdateWithVersion(ctx, p, expectedVersion); err != nil {
			return translate(err)
		}
		p.Version = expectedVersion + 1
		return nil
	}

	if s.txPool == nil {
		// Unit-scope fallback: sequential reads and write. Production
		// always injects a pool, where the update is atomic.
		return valid, do(s.projects, s.settings)
	}
	if err := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		return do(s.projects.WithTx(tx), s.settings.WithTx(tx))
	}); err != nil {
		return nil, err
	}
	if s.onUpdated != nil {
		s.onUpdated(valid)
	}
	return valid, nil
}

// UpdatePlanning writes the project's planning policy and granularity
// (WBS 8.16, 17.61) without touching any other setting.
func (s *SettingsService) UpdatePlanning(ctx context.Context, projectID, policy, granularity string) error {
	cur, err := s.settings.Get(ctx, projectID)
	if err != nil {
		return translate(err)
	}
	cur.PlanningPolicy, cur.PlanningGranularity = policy, granularity
	cur.UpdatedAt = time.Now().UTC()
	if err := s.settings.Update(ctx, cur); err != nil {
		return translate(err)
	}
	return nil
}
