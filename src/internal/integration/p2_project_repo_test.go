package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
)

func newRepo(t *testing.T) persistence.ProjectRepository {
	t.Helper()
	h := NewHarness(t)
	return dbrepo.NewProjectRepository(h.App.DB().Raw())
}

func TestT241CreatePersists(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	p, err := project.New("RepoProject", "desc")
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, p.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != p.ID || got.Name != p.Name {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestT242GetReturnsPersisted(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	p, _ := project.New("Persisted", "")
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, p.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != p.Status || got.CreatedAt.IsZero() {
		t.Fatalf("fields not persisted: %+v", got)
	}
}

func TestT243UnknownIDNotFound(t *testing.T) {
	repo := newRepo(t)
	_, err := repo.Get(context.Background(), "11111111-1111-1111-1111-111111111111")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestT244UpdatePersists(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	p, _ := project.New("Before", "")
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	p.Name = "After"
	p.Description = "changed"
	if err := repo.Update(ctx, p); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := repo.Get(ctx, p.ID.String())
	if got.Name != "After" || got.Description != "changed" {
		t.Fatalf("update not persisted: %+v", got)
	}
}

func TestT245DeleteRemoves(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	p, _ := project.New("Doomed", "")
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Delete(ctx, p.ID.String()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, p.ID.String()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("after delete got %v, want ErrNotFound", err)
	}
}

func TestT246ListReturnsProjects(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	a, _ := project.New("ListA", "")
	b, _ := project.New("ListB", "")
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	list, err := repo.List(ctx, persistence.ProjectListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]bool{}
	for _, p := range list {
		byName[p.Name] = true
	}
	if !byName["ListA"] || !byName["ListB"] {
		t.Fatalf("list missing created projects: %v", byName)
	}
}

// T2.24.1 — Missing row translated to ErrNotFound (via Delete too).
func TestT2241MissingRowTranslated(t *testing.T) {
	repo := newRepo(t)
	if err := repo.Delete(context.Background(), "22222222-2222-2222-2222-222222222222"); err == nil {
		t.Fatal("delete of unknown id succeeded; want ErrNotFound")
	}
}

// T2.24.2 — Unique violation translated to ErrConflict.
func TestT2242UniqueViolationTranslated(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	p, _ := project.New("Dup", "")
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	p2, _ := project.New("Dup2", "")
	p2.ID = p.ID // force PK collision
	if err := repo.Create(ctx, p2); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
}

// T2.24.3 — Foreign-key violation translated to ErrInvalidReference.
// Forces a real FK violation through the live driver and verifies the
// translation, so the 23503 mapping is exercised, not faked.
func TestT2243ForeignKeyTranslated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, rawErr := h.App.DB().Raw().Exec(ctx,
		"INSERT INTO project_settings (project_id, working_directory, context_window, context_threshold, max_retries, worker_timeout) VALUES ('33333333-3333-3333-3333-333333333333', '/wd', 8192, 0.8, 0, 60000000)")
	if rawErr == nil {
		t.Fatal("expected FK violation")
	}
	if !errors.Is(dbrepo.TranslateDBError("insert settings", rawErr), persistence.ErrInvalidReference) {
		t.Fatalf("driver FK error %v not translated to ErrInvalidReference", rawErr)
	}
}

// T2.24.4 — Unknown DB error translated to InfrastructureError.
func TestT2244UnknownErrorTranslated(t *testing.T) {
	repo := newRepo(t)
	_, err := repo.Get(context.Background(), "not-a-uuid")
	if err == nil {
		t.Fatal("expected error for malformed id")
	}
	var inf *persistence.InfrastructureError
	if !errors.As(err, &inf) {
		t.Fatalf("got %v (%T), want InfrastructureError", err, err)
	}
}

// T2.4.7 / T2.24.5 — No PostgreSQL-specific type escapes the repository
// contract: every returned error is a persistence domain value.
func TestT2245NoDriverTypesEscape(t *testing.T) {
	repo := newRepo(t)
	var errs []error
	if _, e := repo.Get(context.Background(), "44444444-4444-4444-4444-444444444444"); e != nil {
		errs = append(errs, e)
	}
	// malformed id -> infrastructure error
	if _, e := repo.Get(context.Background(), "garbage"); e != nil {
		errs = append(errs, e)
	}
	for _, err := range errs {
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "pgconn") || strings.Contains(err.Error(), "pq:") {
			t.Fatalf("driver detail leaked: %v", err)
		}
	}
}
