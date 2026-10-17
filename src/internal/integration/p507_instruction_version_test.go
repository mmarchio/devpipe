package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
)

func newInstructionVersionRepo(t *testing.T, h *Harness) *dbrepo.InstructionVersionRepository {
	t.Helper()
	return dbrepo.NewInstructionVersionRepository(h.App.DB().Raw())
}

// seedInstruction creates a global instruction and returns its ID string.
func seedInstruction(t *testing.T, h *Harness, key, name, content string) string {
	t.Helper()
	repo := newInstructionRepo(t, h)
	rec := newInstructionRecord(t, nil, key, name, content)
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed instruction: %v", err)
	}
	return rec.ID.String()
}

// T5.8.1 — Initial version created (v1) with hash.
func TestP507InitialVersionCreated(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionVersionRepo(t, h)
	id := seedInstruction(t, h, "tdd", "TDD", "seed")

	v1, err := repo.AppendVersion(context.Background(), id, "# TDD v1\nWrite tests first.")
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 {
		t.Fatalf("initial version = %d, want 1", v1.Version)
	}
	var cur int
	if err := h.DBScan(context.Background(),
		"SELECT current_version FROM instructions WHERE id = '"+id+"'", &cur); err != nil {
		t.Fatal(err)
	}
	if cur != 1 {
		t.Fatalf("current_version = %d, want 1", cur)
	}
}

// T5.8.2 — Update increments version.
func TestP507UpdateIncrementsVersion(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionVersionRepo(t, h)
	id := seedInstruction(t, h, "tdd", "TDD", "seed")
	if _, err := repo.AppendVersion(context.Background(), id, "v1"); err != nil {
		t.Fatal(err)
	}
	v2, err := repo.AppendVersion(context.Background(), id, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 {
		t.Fatalf("second version = %d, want 2", v2.Version)
	}
}

// T5.8.3 — Historical version retrievable.
func TestP507HistoricalRetrievable(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionVersionRepo(t, h)
	id := seedInstruction(t, h, "tdd", "TDD", "seed")
	for i, c := range []string{"v1", "v2", "v3"} {
		if _, err := repo.AppendVersion(context.Background(), id, c); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	hist, err := repo.GetVersion(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if hist.Content != "v1" {
		t.Fatalf("v1 content = %q, want v1", hist.Content)
	}
}

// T5.8.4 — Content hash generated (stored hash == hash(content)).
func TestP507ContentHashGenerated(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionVersionRepo(t, h)
	id := seedInstruction(t, h, "tdd", "TDD", "seed")
	const c = "# instructions\n- step one\n- step two"
	v, err := repo.AppendVersion(context.Background(), id, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.ContentHash == "" {
		t.Fatalf("no content hash generated")
	}
	if v.ContentHash != role.HashSystemPromptContent(c) {
		t.Fatalf("stored hash != hash(content)")
	}
	got, err := repo.GetVersion(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash != role.HashSystemPromptContent(got.Content) {
		t.Fatalf("retrieved stored hash does not match hash(retrieved content)")
	}
}

// T5.8.5 — Stale concurrent update rejected (expected version < current).
func TestP507StaleConcurrentUpdateRejected(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionVersionRepo(t, h)
	id := seedInstruction(t, h, "tdd", "TDD", "seed")

	// Reach current version 2.
	if _, err := repo.AppendVersion(context.Background(), id, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AppendVersion(context.Background(), id, "v2"); err != nil {
		t.Fatal(err)
	}
	// A writer that believes the current version is 1 (stale) is rejected.
	if _, err := repo.UpdateWithExpected(context.Background(), id, "stale edit", 1); !isConflict(err) {
		t.Fatalf("stale update (expected 1, actual 2): got %v, want ErrConflict", err)
	}
	// No version was written by the stale attempt.
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM instruction_versions WHERE instruction_id = '"+id+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("version count = %d, want 2 (stale update must not write)", n)
	}
	// A matching expected version succeeds (control — non-vacuity).
	v3, err := repo.UpdateWithExpected(context.Background(), id, "v3", 2)
	if err != nil {
		t.Fatalf("matching update: %v", err)
	}
	if v3.Version != 3 {
		t.Fatalf("version = %d, want 3", v3.Version)
	}
	_ = persistence.ErrConflict
}
