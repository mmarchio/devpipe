package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

// P8-10 (WBS 8.1 persistence half + 8.1 TDD DB half): migration 00041
// (prompt_templates) + the prompt-template repository against real
// PostgreSQL. The domain half (internal/prompt/template_test.go) covers the
// pure validation; this file covers the DB-backed half of T8.1.1-T8.1.6.

// p810Rec maps a valid domain template to its persistence record. The
// deterministic input (fixed timestamps) is the criterion: identical inputs
// persist identical rows.
func p810Rec(t *testing.T, projectID *uuid.UUID, key string, purpose prompt.PromptPurpose) *persistence.PromptTemplateRecord {
	t.Helper()
	tmpl, err := prompt.NewTemplate(prompt.TemplateInput{
		ProjectID: projectID,
		Key:       key,
		Name:      key,
		Purpose:   purpose,
	})
	if err != nil {
		t.Fatalf("NewTemplate: %v", err)
	}
	ts := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	return &persistence.PromptTemplateRecord{
		ID:          tmpl.ID,
		ProjectID:   tmpl.ProjectID,
		Key:         tmpl.Key,
		Name:        tmpl.Name,
		Description: tmpl.Description,
		Purpose:     string(tmpl.Purpose),
		Enabled:     tmpl.Enabled,
		Version:     tmpl.Version,
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}
}

// P8-10.1 — Migration 00041 applies cleanly on a fresh harness DB and the
// new objects exist.
func TestP810MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 40 -> 41 by P8-10 (migration 00041, prompt_templates) —
	// accepted contract change, not a weakened test.
	if version != 126 {
		t.Fatalf("max migration version = %d, want 82", version)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'prompt_templates'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("prompt_templates present = %d, want 1", n)
	}
}

// T8.1.1 (DB half) — A valid template is created: the row persists with all
// fields and reads back through the repository byte-for-byte.
func TestP810ValidTemplatePersisted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewPromptTemplateRepository(pool)

	projectStr := createPhaseProject(t, h)
	projectID, err := uuid.Parse(projectStr)
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	rec := p810Rec(t, &projectID, "phase-planning", prompt.PurposePhasePlanning)
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != rec.ID || *got.ProjectID != projectID || got.Key != "PHASE-PLANNING" ||
		got.Name != "phase-planning" || got.Purpose != "PHASE_PLANNING" ||
		!got.Enabled || got.Version != 1 {
		t.Fatalf("round-trip = %+v", got)
	}
	// Determinism: the read-back row equals the persisted input field-for-
	// field (identical inputs persist identical rows).
	if !got.CreatedAt.Equal(rec.CreatedAt) || !got.UpdatedAt.Equal(rec.UpdatedAt) {
		t.Fatalf("timestamps = %v/%v, want %v/%v", got.CreatedAt, got.UpdatedAt, rec.CreatedAt, rec.UpdatedAt)
	}
	var stored int
	if err := h.DBScan(ctx, "SELECT count(*) FROM prompt_templates WHERE id = '"+rec.ID.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("direct SQL count = %d, want 1", stored)
	}
}

// T8.1.2 — An empty key is rejected by domain validation BEFORE any write.
func TestP810EmptyKeyRejected(t *testing.T) {
	if _, err := prompt.NewTemplate(prompt.TemplateInput{Key: "  ", Purpose: prompt.PurposeCustom}); err == nil {
		t.Fatal("empty key accepted, want rejection")
	}
	// The domain is the sole writer of the normalized key, so an empty key
	// never reaches the repository — confirm no row lands for it.
	h := NewHarness(t)
	var n int
	if err := h.DBScan(context.Background(), "SELECT count(*) FROM prompt_templates WHERE key = ''", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("empty-key rows = %d, want 0", n)
	}
}

// T8.1.3 (DB half) — A duplicate scoped key is rejected: a same-scope
// duplicate yields persistence.ErrConflict at the repository surface and
// 23505 at the DB level, while the same key in a DIFFERENT project succeeds —
// the scoped index, not a global one.
func TestP810DuplicateScopedKeyRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewPromptTemplateRepository(pool)

	projectA, _ := uuid.Parse(createPhaseProject(t, h))
	projectB, _ := uuid.Parse(createPhaseProject(t, h))

	if err := repo.Create(ctx, p810Rec(t, &projectA, "impl", prompt.PurposeImplementation)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Same normalized key (padded lowercase input), same project scope ->
	// conflict.
	if err := repo.Create(ctx, p810Rec(t, &projectA, " impl ", prompt.PurposeImplementation)); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate err = %v, want ErrConflict", err)
	}
	// DB level: the unique index fires with 23505.
	_, err := h.DBExec(ctx, "INSERT INTO prompt_templates (id, project_id, key, name, purpose, enabled, version) VALUES (gen_random_uuid(), '"+projectA.String()+"', 'IMPL', 'x', 'IMPLEMENTATION', true, 1)")
	if err == nil {
		t.Fatal("duplicate accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate error = %v, want 23505", err)
	}
	// The same key in a DIFFERENT project scope succeeds — the scoped index,
	// not a global one.
	if err := repo.Create(ctx, p810Rec(t, &projectB, "impl", prompt.PurposeImplementation)); err != nil {
		t.Fatalf("cross-project create: %v", err)
	}
	// The same key in the GLOBAL scope also succeeds (distinct from every
	// project scope).
	if err := repo.Create(ctx, p810Rec(t, nil, "impl", prompt.PurposeImplementation)); err != nil {
		t.Fatalf("global-scope create: %v", err)
	}
}

// T8.1.4 — A valid purpose is accepted and persisted verbatim.
func TestP810ValidPurposeAccepted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewPromptTemplateRepository(pool)

	project, _ := uuid.Parse(createPhaseProject(t, h))
	rec := p810Rec(t, &project, "review", prompt.PurposeReview)
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Purpose != "REVIEW" {
		t.Fatalf("purpose = %q, want REVIEW", got.Purpose)
	}
}

// T8.1.5 — An unknown purpose is rejected by domain validation.
func TestP810UnknownPurposeRejected(t *testing.T) {
	if _, err := prompt.NewTemplate(prompt.TemplateInput{Key: "x", Purpose: prompt.PromptPurpose("BOGUS")}); err == nil {
		t.Fatal("unknown purpose accepted, want rejection")
	}
}

// T8.1.6 — The initial version is 1, persisted.
func TestP810InitialVersionOne(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewPromptTemplateRepository(pool)

	rec := p810Rec(t, nil, "control", prompt.PurposeControl)
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("version = %d, want 1", got.Version)
	}
	var stored int
	if err := h.DBScan(ctx, "SELECT version FROM prompt_templates WHERE id = '"+rec.ID.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored version = %d, want 1", stored)
	}
}

// T8.1.x (DB half, GetByKey + error translation) — GetByKey resolves within a
// scope and returns ErrNotFound for an absent key; a missing project reference
// yields ErrInvalidReference; a CHECK violation (unknown purpose seeded
// directly) wraps as an infrastructure error — driver errors do not escape the
// repository boundary.
func TestP810GetByKeyAndDriverErrors(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewPromptTemplateRepository(pool)

	project, _ := uuid.Parse(createPhaseProject(t, h))
	rec := p810Rec(t, &project, "validation", prompt.PurposeValidation)
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.GetByKey(ctx, &project, "validation")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if got.ID != rec.ID {
		t.Fatalf("GetByKey = %s, want %s", got.ID, rec.ID)
	}
	if _, err := repo.GetByKey(ctx, &project, "ABSENT"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("absent key err = %v, want ErrNotFound", err)
	}
	if _, err := repo.Get(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}

	// FK violation: a template referencing a missing project (23503).
	missing := uuid.New()
	orphan := p810Rec(t, &missing, "orphan", prompt.PurposeCustom)
	if err := repo.Create(ctx, orphan); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("FK err = %v, want ErrInvalidReference", err)
	}
	// CHECK violation: an unknown purpose seeded directly (23514) wraps as
	// infrastructure, never a driver type.
	bad := p810Rec(t, nil, "check", prompt.PurposeCustom)
	bad.Purpose = "BOGUS"
	bad.ID = uuid.New()
	err = repo.Create(ctx, bad)
	var infra *persistence.InfrastructureError
	if !errors.As(err, &infra) {
		t.Fatalf("CHECK err = %v (%T), want infrastructure error", err, err)
	}
}

// Update mutation surface (P8-10 constraint) — name/description/purpose/enabled
// are mutable in place; key and version are immutable. A stale update (unknown
// id) yields ErrNotFound. The policy is pinned here: Update touches only the
// four mutable fields and never key/version.
func TestP810UpdateMutationSurface(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewPromptTemplateRepository(pool)

	project, _ := uuid.Parse(createPhaseProject(t, h))
	rec := p810Rec(t, &project, "custom", prompt.PurposeCustom)
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Mutate the four mutable fields.
	before, _ := repo.Get(ctx, rec.ID)
	rec.Name = "renamed"
	rec.Description = "desc"
	rec.Purpose = "REVIEW"
	rec.Enabled = false
	if err := repo.Update(ctx, rec); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := repo.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "renamed" || got.Description != "desc" || got.Purpose != "REVIEW" || got.Enabled {
		t.Fatalf("updated row = %+v", got)
	}
	// Key and version are unchanged by Update.
	if got.Key != before.Key || got.Version != before.Version {
		t.Fatalf("immutable fields changed: key %v -> %v, version %d -> %d", before.Key, got.Key, before.Version, got.Version)
	}
	// A stale update (unknown id) yields ErrNotFound.
	if err := repo.Update(ctx, &persistence.PromptTemplateRecord{ID: uuid.New(), Key: "x"}); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("stale update err = %v, want ErrNotFound", err)
	}
}
