package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/service"
)

// failingSettingsRepo is a settings repository whose Create fails with a
// caller-supplied error (T3.2.5 rollback path).
type failingSettingsRepo struct {
	createErr error
}

func (f *failingSettingsRepo) Create(ctx context.Context, s *project.Settings) error {
	return f.createErr
}
func (f *failingSettingsRepo) Get(ctx context.Context, projectID string) (*project.Settings, error) {
	return nil, persistence.ErrNotFound
}
func (f *failingSettingsRepo) Update(ctx context.Context, s *project.Settings) error {
	return persistence.ErrNotFound
}
func (f *failingSettingsRepo) Delete(ctx context.Context, projectID string) error {
	return persistence.ErrNotFound
}
func (f *failingSettingsRepo) WithTx(tx persistence.Transaction) persistence.SettingsRepository {
	return f
}

// newP3Svc builds a ProjectService over the harness database (P3-02).
func newP3Svc(t *testing.T) (*service.ProjectService, *Harness) {
	t.Helper()
	h := NewHarness(t)
	pool := h.App.DB().Raw()
	return service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	), h
}

// T3.2.1 — Valid project created through the service.
func TestT321ValidProjectCreated(t *testing.T) {
	svc, h := newP3Svc(t)
	ctx := context.Background()
	p, s, err := svc.CreateProject(ctx, "creation project", "desc")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p == nil || s == nil {
		t.Fatal("nil project or settings returned")
	}
	var count int
	if err := h.DBScan(ctx, "SELECT COUNT(*) FROM projects WHERE id = '"+p.ID.String()+"'", &count); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 1 {
		t.Fatalf("project row = %d, want 1", count)
	}
}

// T3.2.2 — Initial status DRAFT.
func TestT322InitialStatusDraft(t *testing.T) {
	svc, h := newP3Svc(t)
	ctx := context.Background()
	p, _, err := svc.CreateProject(ctx, "draft project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var status string
	if err := h.DBScan(ctx, "SELECT status FROM projects WHERE id = '"+p.ID.String()+"'", &status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "DRAFT" {
		t.Fatalf("status = %q, want DRAFT", status)
	}
}

// T3.2.3 — Settings created with the project: a settings row exists for the
// new project id carrying the service defaults.
func TestT323SettingsCreatedWithProject(t *testing.T) {
	svc, h := newP3Svc(t)
	ctx := context.Background()
	p, _, err := svc.CreateProject(ctx, "settings project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var (
		window    int
		threshold float64
	)
	query := "SELECT context_window, context_threshold FROM project_settings WHERE project_id = '" + p.ID.String() + "'"
	if err := h.DBScan(ctx, query, &window, &threshold); err != nil {
		t.Fatalf("no settings row for new project: %v", err)
	}
	if window != service.DefaultContextWindow {
		t.Fatalf("context_window = %d, want default %d", window, service.DefaultContextWindow)
	}
	if threshold != service.DefaultContextThreshold {
		t.Fatalf("context_threshold = %v, want default %v", threshold, service.DefaultContextThreshold)
	}
}

// T3.2.4 — Invalid project rejected: empty name leaves nothing persisted.
func TestT324InvalidProjectRejected(t *testing.T) {
	svc, h := newP3Svc(t)
	ctx := context.Background()
	if _, _, err := svc.CreateProject(ctx, "   ", ""); err == nil {
		t.Fatal("empty name accepted")
	}
	var count int
	if err := h.DBScan(ctx, "SELECT COUNT(*) FROM projects", &count); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 0 {
		t.Fatalf("invalid create persisted %d projects", count)
	}
}

// T3.2.5 — Settings failure rolls back project: a settings insert failure
// (injected via the repository seam) must leave NO project row behind.
func TestT325SettingsFailureRollsBackProject(t *testing.T) {
	svc, h := newP3Svc(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()

	conflict := &failingSettingsRepo{createErr: persistence.ErrConflict}
	svc = svc.WithSettingsRepo(conflict)

	p, _, err := svc.CreateProject(ctx, "rollback me", "")
	if err == nil {
		t.Fatalf("create succeeded; want settings failure (project id %s)", p.ID)
	}
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("create: got %v, want ErrConflict", err)
	}

	// The in-tx project insert must have been rolled back: no row for the
	// returned id, and no row at all in the fresh database.
	var count int
	if err := h.DBScan(ctx, "SELECT COUNT(*) FROM projects", &count); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 0 {
		t.Fatalf("project row survived a settings failure: %d rows (rollback missing)", count)
	}
	var sCount int
	if err := h.DBScan(ctx, "SELECT COUNT(*) FROM project_settings", &sCount); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sCount != 0 {
		t.Fatalf("settings row survived: %d rows", sCount)
	}
	_ = pool
}

// T3.2.6 — Created timestamps populated: both rows carry non-zero
// created_at/updated_at.
func TestT326TimestampsPopulated(t *testing.T) {
	svc, h := newP3Svc(t)
	ctx := context.Background()
	p, _, err := svc.CreateProject(ctx, "timestamps", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var (
		pCreated, pUpdated time.Time
	)
	if err := h.DBScan(ctx, "SELECT created_at, updated_at FROM projects WHERE id = '"+p.ID.String()+"'", &pCreated, &pUpdated); err != nil {
		t.Fatalf("scan project: %v", err)
	}
	if pCreated.IsZero() || pUpdated.IsZero() {
		t.Fatalf("project timestamps zero: %+v", p)
	}
	var (
		sCreated, sUpdated time.Time
	)
	if err := h.DBScan(ctx, "SELECT created_at, updated_at FROM project_settings WHERE project_id = '"+p.ID.String()+"'", &sCreated, &sUpdated); err != nil {
		t.Fatalf("scan settings: %v", err)
	}
	if sCreated.IsZero() || sUpdated.IsZero() {
		t.Fatal("settings timestamps zero")
	}
}
