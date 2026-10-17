package integration

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
)

// newRoleRepo / newPromptRepo wire the real repositories to the harness pool.
func newRoleRepo(t *testing.T, h *Harness) *dbrepo.RoleRepository {
	t.Helper()
	return dbrepo.NewRoleRepository(h.App.DB().Raw())
}

func newPromptRepo(t *testing.T, h *Harness) *dbrepo.SystemPromptRepository {
	t.Helper()
	return dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
}

// newRoleRecord builds a RoleRecord for creation (domain-validated fields).
// parseProjectID parses the project ID returned by createPhaseProject.
func roleProjectID(t *testing.T, id string) uuid.UUID {
	t.Helper()
	p, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	return p
}

func newRoleRecord(t *testing.T, projectID *uuid.UUID, key, name, desc string) *persistence.RoleRecord {
	t.Helper()
	r, err := role.NewRole(role.RoleInput{ProjectID: projectID, Key: key, Name: name, Description: desc})
	if err != nil {
		t.Fatalf("NewRole(%q): %v", key, err)
	}
	rec := &persistence.RoleRecord{
		ID: r.ID, ProjectID: r.ProjectID, Key: r.Key, Name: r.Name,
		Description: r.Description, Enabled: r.Enabled, Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	return rec
}

// seedRole inserts a role and returns its ID string (for inline SQL).
func seedRole(t *testing.T, h *Harness, projectID *uuid.UUID, key, name string) string {
	t.Helper()
	repo := newRoleRepo(t, h)
	rec := newRoleRecord(t, projectID, key, name, "")
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed role %q: %v", key, err)
	}
	return rec.ID.String()
}

// P5-03 (WBS 5.1 persistence + 5.4/5.5): migration 00015 verification.
// T5.1.5 (DB half) — Duplicate key within the same scope is rejected.
func TestP503DuplicateScopedKeyRejected(t *testing.T) {
	h := NewHarness(t)
	repo := newRoleRepo(t, h)
	if err := repo.Create(context.Background(), newRoleRecord(t, nil, "impl-engineer", "E", "")); err != nil {
		t.Fatalf("first global engineer: %v", err)
	}
	if err := repo.Create(context.Background(), newRoleRecord(t, nil, "impl-engineer", "E2", "")); !isConflict(err) {
		t.Fatalf("duplicate global key: got %v, want ErrConflict", err)
	}
	// Same project scope (real project row — FK reference).
	pid := roleProjectID(t, createPhaseProject(t, h))
	if err := repo.Create(context.Background(), newRoleRecord(t, &pid, "qa-engineer", "T", "")); err != nil {
		t.Fatalf("first project tester: %v", err)
	}
	if err := repo.Create(context.Background(), newRoleRecord(t, &pid, "qa-engineer", "T2", "")); !isConflict(err) {
		t.Fatalf("duplicate project-scoped key: got %v, want ErrConflict", err)
	}
}

// T5.1.6 (DB half) — Same key allowed in different projects and globally.
func TestP503SameKeyDifferentProjects(t *testing.T) {
	h := NewHarness(t)
	repo := newRoleRepo(t, h)
	pidA := roleProjectID(t, createPhaseProject(t, h))
	pidB := roleProjectID(t, createPhaseProject(t, h))
	for _, rec := range []*persistence.RoleRecord{
		newRoleRecord(t, &pidA, "impl-engineer", "A", ""),
		newRoleRecord(t, &pidB, "impl-engineer", "B", ""),
		newRoleRecord(t, nil, "impl-engineer", "G", ""),
	} {
		if err := repo.Create(context.Background(), rec); err != nil {
			t.Fatalf("create %q (scope %v): %v", rec.Key, rec.ProjectID, err)
		}
	}
}

// T5.4.1 — System prompt stored; T5.5.1 — initial prompt is v1.
func TestP503PromptStoredInitialV1(t *testing.T) {
	h := NewHarness(t)
	repo := newPromptRepo(t, h)
	roleID := seedRole(t, h, nil, "impl-engineer", "E")

	v1, err := repo.AppendVersion(context.Background(), roleID, "You are the implementation engineer.")
	if err != nil {
		t.Fatalf("AppendVersion: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("initial version = %d, want 1", v1.Version)
	}
	var cur int
	if err := h.DBScan(context.Background(),
		"SELECT current_system_prompt_version FROM roles WHERE id = '"+roleID+"'", &cur); err != nil {
		t.Fatal(err)
	}
	if cur != 1 {
		t.Fatalf("current_system_prompt_version = %d, want 1", cur)
	}
}

// T5.4.2 — System prompt retrieved exactly.
func TestP503PromptRetrievedExactly(t *testing.T) {
	h := NewHarness(t)
	repo := newPromptRepo(t, h)
	roleID := seedRole(t, h, nil, "impl-engineer", "E")
	const c = "You are the implementation engineer.\nImplement only the assigned work unit."
	if _, err := repo.AppendVersion(context.Background(), roleID, c); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetVersion(context.Background(), roleID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != c {
		t.Fatalf("retrieved content = %q, want %q", got.Content, c)
	}
	if got.ContentHash != role.HashSystemPromptContent(c) {
		t.Fatalf("stored hash does not match role.HashSystemPromptContent(content)")
	}
}

// T5.4.3 — Multiline Markdown preserved byte-for-byte.
func TestP503PromptMultilineMarkdownPreserved(t *testing.T) {
	h := NewHarness(t)
	repo := newPromptRepo(t, h)
	roleID := seedRole(t, h, nil, "impl-engineer", "E")
	md := "# ENGINEER\n\n## Responsibilities\n- implement\n  - sub\n\n```\ncode\n```\n"
	if _, err := repo.AppendVersion(context.Background(), roleID, md); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetVersion(context.Background(), roleID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != md {
		t.Fatalf("markdown not preserved: got %q", got.Content)
	}
}

// T5.4.4 — Unicode preserved.
func TestP503PromptUnicodePreserved(t *testing.T) {
	h := NewHarness(t)
	repo := newPromptRepo(t, h)
	roleID := seedRole(t, h, nil, "impl-engineer", "E")
	uni := "Rôle — 実装エンジニア\n- café — naïve — über"
	if _, err := repo.AppendVersion(context.Background(), roleID, uni); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetVersion(context.Background(), roleID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != uni {
		t.Fatalf("unicode not preserved: got %q", got.Content)
	}
}

// T5.4.5 — Empty required system prompt rejected (whitespace-only too).
func TestP503EmptyPromptRejected(t *testing.T) {
	h := NewHarness(t)
	repo := newPromptRepo(t, h)
	roleID := seedRole(t, h, nil, "impl-engineer", "E")
	for _, c := range []string{"", "   ", "\t\n"} {
		if _, err := repo.AppendVersion(context.Background(), roleID, c); err == nil {
			t.Fatalf("empty/whitespace prompt %q accepted, want rejection", c)
		}
	}
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM role_system_prompts WHERE role_id = '"+roleID+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("empty prompts persisted %d rows, want 0", n)
	}
}

// T5.5.2 — Edit creates v2; T5.5.3 — historical v1 remains retrievable;
// T5.5.4 — current version points to v2.
func TestP503PromptVersioning(t *testing.T) {
	h := NewHarness(t)
	repo := newPromptRepo(t, h)
	roleID := seedRole(t, h, nil, "impl-engineer", "E")

	const c1, c2 = "prompt v1", "prompt v2"
	if _, err := repo.AppendVersion(context.Background(), roleID, c1); err != nil {
		t.Fatal(err)
	}
	v2, err := repo.AppendVersion(context.Background(), roleID, c2)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 {
		t.Fatalf("second version = %d, want 2", v2.Version)
	}
	hist, err := repo.GetVersion(context.Background(), roleID, 1)
	if err != nil {
		t.Fatalf("v1 no longer retrievable: %v", err)
	}
	if hist.Content != c1 {
		t.Fatalf("v1 content = %q, want %q", hist.Content, c1)
	}
	cur, err := repo.GetCurrentVersion(context.Background(), roleID)
	if err != nil {
		t.Fatal(err)
	}
	if cur != 2 {
		t.Fatalf("current version = %d, want 2", cur)
	}
	// v1 row untouched (append-only).
	var countV1 int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM role_system_prompts WHERE role_id = '"+roleID+"' AND version = 1", &countV1); err != nil {
		t.Fatal(err)
	}
	if countV1 != 1 {
		t.Fatalf("v1 rows = %d, want 1 (append-only)", countV1)
	}
}

// T5.5.5 — Concurrent edits cannot create duplicate version numbers.
func TestP503ConcurrentPromptEditsNoDuplicates(t *testing.T) {
	h := NewHarness(t)
	repo := newPromptRepo(t, h)
	roleID := seedRole(t, h, nil, "impl-engineer", "E")

	const n = 12
	results := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := repo.AppendVersion(context.Background(), roleID, "content "+itoa(i))
			if err != nil {
				results <- -1
				return
			}
			results <- v.Version
		}(i)
	}
	wg.Wait()
	close(results)

	versions := []int{}
	for v := range results {
		if v < 0 {
			t.Fatal("concurrent AppendVersion failed")
		}
		versions = append(versions, v)
	}
	sort.Ints(versions)
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("concurrent versions = %v, want exactly 1..%d", versions, n)
		}
	}
	var cur int
	if err := h.DBScan(context.Background(),
		"SELECT current_system_prompt_version FROM roles WHERE id = '"+roleID+"'", &cur); err != nil {
		t.Fatal(err)
	}
	if cur != n {
		t.Fatalf("current version after concurrent appends = %d, want %d", cur, n)
	}
}

// T5.5.6 — An existing execution snapshot remains bound to its historical
// version: after a later edit, the historical version's content is intact and
// a snapshot pinned to v1 still reads v1.
func TestP503SnapshotBoundToHistoricalVersion(t *testing.T) {
	h := NewHarness(t)
	repo := newPromptRepo(t, h)
	roleID := seedRole(t, h, nil, "impl-engineer", "E")

	const c1, c2 = "ENGINEER v3", "ENGINEER v4"
	if _, err := repo.AppendVersion(context.Background(), roleID, c1); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendVersion(context.Background(), roleID, c2); err != nil {
		t.Fatal(err)
	}
	// A snapshot bound to v1 (Execution A) must still read the v1 content
	// after the role is edited to v2.
	a, err := repo.GetVersion(context.Background(), roleID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Content != c1 {
		t.Fatalf("historical v1 content mutated: %q", a.Content)
	}
	b, err := repo.GetVersion(context.Background(), roleID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if b.Content != c2 {
		t.Fatalf("current v2 content = %q, want %q", b.Content, c2)
	}
}

// isConflict reports whether err is a persistence conflict (DB 23505).
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	return err == persistence.ErrConflict
}
