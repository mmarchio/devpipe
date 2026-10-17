package wbs

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// T2.9.1 — WBS associated with phase.
func TestT291AssociatedWithPhase(t *testing.T) {
	pid := uuid.New()
	d, err := NewDocument(pid, "# Scope\n\nDo the thing.")
	if err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	if d.PhaseID != pid {
		t.Fatalf("PhaseID mismatch: %v", d.PhaseID)
	}
	if d.ID == uuid.Nil {
		t.Fatal("ID not assigned")
	}
	if d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		t.Fatal("timestamps not assigned")
	}
	if d.CreatedAt.Location() != time.UTC {
		t.Fatal("timestamps not UTC")
	}
}

// T2.9.2 — Empty WBS rejected.
func TestT292EmptyContentRejected(t *testing.T) {
	for _, c := range []string{"", "   \n\t "} {
		if _, err := NewDocument(uuid.New(), c); err == nil {
			t.Fatalf("content %q accepted", c)
		}
	}
	if _, err := NewDocument(uuid.Nil, "content"); err == nil {
		t.Fatal("nil phase ID accepted")
	}
}

// T2.9.3 — Content round-trips without modification (markdown/unicode).
func TestT293ContentRoundTrip(t *testing.T) {
	content := "# Phase 7 — Authentication\n\n- **bold** list\n- code: `x := fmt.Sprintf(\"%d\", 42)`\n\n> unicode: 日本語テスト, emoji ✓\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	d, _ := NewDocument(uuid.New(), content)
	if d.Content != content {
		t.Fatalf("content modified:\n got %q\nwant %q", d.Content, content)
	}
	updated := "v2 content — changed"
	if err := d.Update(updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	if d.Content != updated {
		t.Fatal("updated content not retained")
	}
}

// T2.9.4 — Updating WBS increments version.
func TestT294UpdateIncrementsVersion(t *testing.T) {
	d, _ := NewDocument(uuid.New(), "v1")
	if d.Version != 1 {
		t.Fatalf("initial version %d, want 1", d.Version)
	}
	if err := d.Update("v2"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if d.Version != 2 {
		t.Fatalf("version after one update %d, want 2", d.Version)
	}
	if err := d.Update("v3"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if d.Version != 3 {
		t.Fatalf("version after two updates %d, want 3", d.Version)
	}
}

// T2.9.5 — WBS update changes UpdatedAt.
func TestT295UpdateChangesUpdatedAt(t *testing.T) {
	d, _ := NewDocument(uuid.New(), "v1")
	before := d.UpdatedAt
	// Force a measurable gap so the test is not flaky at clock granularity.
	time.Sleep(2 * time.Millisecond)
	if err := d.Update("v2"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !d.UpdatedAt.After(before) {
		t.Fatalf("UpdatedAt did not advance: before=%v after=%v", before, d.UpdatedAt)
	}
}

// T2.9.6 — WBS version cannot move backward.
func TestT296VersionCannotMoveBackward(t *testing.T) {
	d, _ := NewDocument(uuid.New(), "v1")
	if err := d.Update("v2"); err != nil {
		t.Fatalf("update: %v", err)
	}
	d.Version = 5 // simulate a later version
	if err := d.Update("v6"); err != nil {
		t.Fatalf("update at 5: %v", err)
	}
	if d.Version != 6 {
		t.Fatalf("version %d, want 6", d.Version)
	}
	// A stale writer at version 2 trying to update must be rejected.
	stale, _ := NewDocument(d.PhaseID, "stale v1")
	if err := stale.Update("stale v2"); err != nil {
		t.Fatalf("stale update: %v", err)
	}
	if err := d.SetVersion(2); err == nil {
		t.Fatal("SetVersion allowed a backward move")
	}
}
