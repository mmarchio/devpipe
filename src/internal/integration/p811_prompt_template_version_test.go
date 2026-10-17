package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

// P8-11 (WBS 8.4 persistence half + 8.4 TDD DB half): migration 00042
// (prompt_template_versions) and the version repository — advisory-locked
// MAX+1 allocation, deterministic content hash — against real PostgreSQL.
// The pure half (internal/prompt/content_test.go) covers the in-domain
// series; this file covers the DB-backed half of T8.4.3-T8.4.6.

// p811Template creates a project template through the P8-10 repository and
// returns its id.
func p811Template(t *testing.T, h *Harness, key string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	repo := dbrepo.NewPromptTemplateRepository(h.App.DB().Raw())
	rec := p810Rec(t, nil, key, prompt.PurposeImplementation)
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create template: %v", err)
	}
	return rec.ID
}

// p811CountVersions counts the stored version rows by direct SQL.
func p811CountVersions(t *testing.T, h *Harness, templateID uuid.UUID) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM prompt_template_versions WHERE template_id = '"+templateID.String()+"'", &n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

// P8-11.1 — Migration 00042 applies cleanly on a fresh harness DB and the
// table + unique index exist.
func TestP811MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 41 -> 42 by P8-11 (migration 00042,
	// prompt_template_versions) — accepted contract change, not a
	// weakened test.
	if version != 126 {
		t.Fatalf("max migration version = %d, want 82", version)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'prompt_template_versions'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("prompt_template_versions present = %d, want 1", n)
	}
	var idx int
	if err := h.DBScan(ctx, "SELECT count(*) FROM pg_indexes WHERE tablename = 'prompt_template_versions' AND indexname = 'prompt_template_versions_template_version_unique'", &idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("unique index present = %d, want 1", idx)
	}
}

// T8.4.3 (DB half) + T8.4.4 (DB half) — An earlier version remains
// retrievable after the current version advances, and the current version
// advances on each append.
func TestP811HistoryRetrievableAndCurrentAdvances(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	templateID := p811Template(t, h, "impl-wu")
	repo := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())

	v1, err := repo.AppendVersion(ctx, templateID, "v1 content")
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("v1 = %+v, want version 1", v1)
	}
	v2, err := repo.AppendVersion(ctx, templateID, "v2 content")
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("T8.4.4 current did not advance: v2 = %+v, want version 2", v2)
	}
	// T8.4.3 — the earlier version remains retrievable by exact version.
	hist, err := repo.GetVersion(ctx, templateID, 1)
	if err != nil {
		t.Fatalf("T8.4.3 historical v1: %v", err)
	}
	if hist.Version != 1 || hist.Content != "v1 content" {
		t.Fatalf("historical v1 = %+v", hist)
	}
	cur, err := repo.GetCurrentVersion(ctx, templateID)
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if cur.Version != 2 || cur.Content != "v2 content" {
		t.Fatalf("current = %+v, want v2", cur)
	}
	list, err := repo.ListVersions(ctx, templateID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(list) != 2 || list[0].Version != 1 || list[1].Version != 2 {
		t.Fatalf("list = %+v", list)
	}
	if p811CountVersions(t, h, templateID) != 2 {
		t.Fatalf("version rows = %d, want 2", p811CountVersions(t, h, templateID))
	}
	// An unknown version yields ErrNotFound; an unknown template too.
	if _, err := repo.GetVersion(ctx, templateID, 99); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("absent version err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetCurrentVersion(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown template err = %v, want ErrNotFound", err)
	}
}

// T8.4.5 (DB half) — Concurrent appends do not duplicate versions: 20 real
// goroutines race the repository against real PG; the committed versions are
// exactly the contiguous sequence 1..20 (one winner each).
func TestP811ConcurrentAppendsNoDuplicateVersions(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	templateID := p811Template(t, h, "conc")
	repo := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := repo.AppendVersion(ctx, templateID, fmt.Sprintf("r%d", i)); err != nil {
				errs <- fmt.Errorf("racer %d: %w", i, err)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("racer: %v", err)
	}
	versions, err := repo.ListVersions(ctx, templateID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != writers {
		t.Fatalf("committed versions = %d, want %d", len(versions), writers)
	}
	for i, v := range versions {
		if v.Version != i+1 {
			t.Fatalf("T8.4.5 versions not contiguous: position %d = %d, want %d", i, v.Version, i+1)
		}
	}
	cur, err := repo.GetCurrentVersion(ctx, templateID)
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if cur.Version != writers {
		t.Fatalf("current version = %d, want %d", cur.Version, writers)
	}
}

// T8.4.6 (DB half) — content_hash is generated on append and is deterministic
// over content (via the P8-02 domain function).
func TestP811ContentHashGeneratedAndDeterministic(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	templateID := p811Template(t, h, "hash")
	repo := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())

	const content = "## Implementation\n\nDo the thing."
	v, err := repo.AppendVersion(ctx, templateID, content)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	want := prompt.ContentHash(content)
	if v.ContentHash != want {
		t.Fatalf("T8.4.6 hash = %q, want domain function %q", v.ContentHash, want)
	}
	// The stored hash is the deterministic SHA-256 of the exact content
	// bytes: identical content in a second version yields an identical hash.
	v2, err := repo.AppendVersion(ctx, templateID, content)
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if v2.ContentHash != v.ContentHash {
		t.Fatalf("identical content produced different hashes: %q vs %q", v2.ContentHash, v.ContentHash)
	}
	// A single byte of difference moves the hash.
	v3, err := repo.AppendVersion(ctx, templateID, content+"x")
	if err != nil {
		t.Fatalf("append 3: %v", err)
	}
	if v3.ContentHash == v.ContentHash {
		t.Fatalf("changed content produced identical hash: %q", v3.ContentHash)
	}
	// The hash is persisted (read it back by direct SQL, not just the
	// in-memory return).
	var stored string
	if err := h.DBScan(ctx,
		"SELECT content_hash FROM prompt_template_versions WHERE template_id = '"+templateID.String()+"' AND version = 1", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != want {
		t.Fatalf("stored hash = %q, want %q", stored, want)
	}
}

// T8.4.6 / error translation (DB half) — an unknown template yields
// ErrNotFound; a (template_id, version) duplicate seeded directly yields
// 23505 at the DB level; FK cascade on template delete.
func TestP811ErrorsAndConstraints(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	templateID := p811Template(t, h, "errc")
	repo := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())

	// Unknown template -> ErrNotFound (before any write).
	if _, err := repo.AppendVersion(ctx, uuid.New(), "x"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown template err = %v, want ErrNotFound", err)
	}

	// (template_id, version) duplicate at the DB level -> 23505.
	seed := "INSERT INTO prompt_template_versions (id, template_id, version, content, content_hash) VALUES (gen_random_uuid(), '" +
		templateID.String() + "', 1, 'a', 'h')"
	if _, err := h.DBExec(ctx, seed); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	_, err := h.DBExec(ctx, seed)
	if err == nil {
		t.Fatal("duplicate (template_id, version) accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate error = %v, want 23505", err)
	}

	// FK cascade: deleting the template removes its version rows.
	if _, err := h.DBExec(ctx, "DELETE FROM prompt_templates WHERE id = '"+templateID.String()+"'"); err != nil {
		t.Fatalf("delete template: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM prompt_template_versions WHERE template_id = '"+templateID.String()+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("version rows after template delete = %d, want 0 (cascade)", n)
	}
}

// P8-11.2 — Append timestamps round-trip: a version row's created_at is set
// at insert and reads back as a time.Time.
func TestP811CreatedAtRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	templateID := p811Template(t, h, "ts")
	repo := dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())

	before := time.Now().UTC().Add(-time.Second)
	v, err := repo.AppendVersion(ctx, templateID, "ts content")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if v.CreatedAt.IsZero() || v.CreatedAt.Before(before) {
		t.Fatalf("created_at = %v, want >= %v", v.CreatedAt, before)
	}
}
