package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
)

func newInstructionRepo(t *testing.T, h *Harness) *dbrepo.InstructionRepository {
	t.Helper()
	return dbrepo.NewInstructionRepository(h.App.DB().Raw())
}

func newInstructionRecord(t *testing.T, projectID *uuid.UUID, key, name, content string) *persistence.InstructionRecord {
	t.Helper()
	in, err := role.NewInstruction(role.InstructionInput{ProjectID: projectID, Key: key, Name: name, Content: content})
	if err != nil {
		t.Fatalf("NewInstruction(%q): %v", key, err)
	}
	return &persistence.InstructionRecord{
		ID: in.ID, ProjectID: in.ProjectID, Key: in.Key, Name: in.Name,
		Description: in.Description, Content: in.Content, Version: in.Version,
		Enabled: in.Enabled, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
	}
}

// T5.7.1 (persistence half) — Instruction created and readable back.
func TestP506InstructionCreated(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionRepo(t, h)
	rec := newInstructionRecord(t, nil, "tdd", "TDD", "Write tests first.")
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(context.Background(), rec.ID.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Key != "TDD" || got.Name != "TDD" {
		t.Fatalf("got %q/%q, want TDD/TDD", got.Key, got.Name)
	}
}

// T5.7.2 — Content stored: multiline markdown + unicode round-trips exactly.
func TestP506ContentStored(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionRepo(t, h)
	content := "# TDD\n\n## Cycle\n1. RED\n2. GREEN\n3. REFACTOR\n\n- naïve — café — 実装"
	rec := newInstructionRecord(t, nil, "tdd", "TDD", content)
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(context.Background(), rec.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != content {
		t.Fatalf("stored content not preserved:\n got %q\nwant %q", got.Content, content)
	}
}

// T5.7.4 (DB half) — Duplicate scoped key rejected; same key across scopes ok.
func TestP506DuplicateScopedKeyRejected(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionRepo(t, h)
	if err := repo.Create(context.Background(), newInstructionRecord(t, nil, "tdd", "A", "c")); err != nil {
		t.Fatalf("first global tdd: %v", err)
	}
	if err := repo.Create(context.Background(), newInstructionRecord(t, nil, "tdd", "B", "c")); !isConflict(err) {
		t.Fatalf("duplicate global instruction key: got %v, want ErrConflict", err)
	}
	pid := roleProjectID(t, createPhaseProject(t, h))
	if err := repo.Create(context.Background(), newInstructionRecord(t, &pid, "tdd", "P", "c")); err != nil {
		t.Fatalf("project-scoped tdd: %v", err)
	}
}

// T5.7.5 (persistence half) — Disabled instruction retained in storage.
func TestP506DisabledRetained(t *testing.T) {
	h := NewHarness(t)
	repo := newInstructionRepo(t, h)
	rec := newInstructionRecord(t, nil, "tdd", "TDD", "c")
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	// New instructions are enabled by default.
	var enabled bool
	if err := h.DBScan(context.Background(),
		"SELECT enabled FROM instructions WHERE id = '"+rec.ID.String()+"'", &enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatalf("newly created instruction not enabled by default")
	}
	// Disable it directly; the row must be RETAINED (WBS 5.7: disabling is a
	// field flip, exclusion happens in resolution, not storage).
	if _, err := h.DBExec(context.Background(),
		"UPDATE instructions SET enabled = false WHERE id = '"+rec.ID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	var after bool
	if err := h.DBScan(context.Background(),
		"SELECT enabled FROM instructions WHERE id = '"+rec.ID.String()+"'", &after); err != nil {
		t.Fatalf("expected row to remain (disabled-retained), got scan error: %v", err)
	}
	if after {
		t.Fatalf("disabled flag not persisted")
	}
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM instructions WHERE id = '"+rec.ID.String()+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("disabled-retained row count = %d, want 1 (not deleted)", n)
	}
}
