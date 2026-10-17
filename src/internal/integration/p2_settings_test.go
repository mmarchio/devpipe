package integration

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/database"
)

// T2.3.1 — Migration creates projects table.
func TestP2MigrationCreatesProjectsTable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var exists bool
	if err := h.DBScan(ctx, "SELECT true FROM pg_tables WHERE tablename = 'projects'", &exists); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !exists {
		t.Fatal("projects table missing after migration")
	}
}

// T2.3.2 — Migration creates settings table.
func TestP2MigrationCreatesSettingsTable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var exists bool
	if err := h.DBScan(ctx, "SELECT true FROM pg_tables WHERE tablename = 'project_settings'", &exists); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !exists {
		t.Fatal("project_settings table missing after migration")
	}
}

// T2.3.3 — Settings require valid project (FK violation for nonexistent project).
func TestP2SettingsRequireValidProject(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, err := h.DBExec(ctx, insertSettingsSQL(uuid.New()))
	if err == nil {
		t.Fatal("insert with nonexistent project_id succeeded; FK missing")
	}
}

// T2.3.4 / T2.2.8 — Deleting a project removes its settings (cascade).
func TestP2ProjectDeleteCascadesSettings(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := h.DBExec(ctx, insertSettingsSQL(pid)); err != nil {
		t.Fatalf("insert settings: %v", err)
	}
	if _, err := h.DBExec(ctx, "DELETE FROM projects WHERE id = '"+pid.String()+"'"); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM project_settings WHERE project_id = '"+pid.String()+"'", &count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("orphan settings after project delete: %d", count)
	}
}

// T2.2.2 — A project cannot have multiple active settings records
// (project_id is the primary key of project_settings).
func TestP2SettingsUniquePerProject(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := h.DBExec(ctx, insertSettingsSQL(pid)); err != nil {
		t.Fatalf("first settings insert: %v", err)
	}
	if _, err := h.DBExec(ctx, insertSettingsSQL(pid)); err == nil {
		t.Fatal("second settings row for same project accepted; uniqueness missing")
	}
}

// T2.3.5 — Migration remains idempotent (re-running is a safe no-op).
func TestP2MigrationIdempotent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	// Re-run the full migration set against the already-migrated database.
	migs, err := database.LoadMigrations(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if err := database.Migrate(ctx, h.App.DB().Raw(), migs); err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}
	var rows int
	if err := h.DBScan(ctx, "SELECT count(*) FROM schema_migrations", &rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	want := len(migs)
	if rows != want {
		t.Fatalf("schema_migrations has %d rows after re-run, want %d (duplicate records)", rows, want)
	}
}

// --- helpers -------------------------------------------------------------

func insertProjectSQL(id uuid.UUID, name string) string {
	return "INSERT INTO projects (id, name, description, status) VALUES ('" + id.String() + "', '" + name + "', '', 'DRAFT')"
}

func insertSettingsSQL(projectID uuid.UUID) string {
	return `INSERT INTO project_settings
		(project_id, working_directory, context_window, context_threshold, max_retries, worker_timeout)
		VALUES ('` + projectID.String() + `', '/wd', 8192, 0.8, 0, 60000000)`
}
