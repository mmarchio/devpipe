package project

import (
	"strings"
	"testing"
	"time"
)

// T2.1.1 — Valid project can be constructed.
func TestNewValidProject(t *testing.T) {
	p, err := New("Example", "A sample project")
	if err != nil {
		t.Fatalf("valid project rejected: %v", err)
	}
	if p.ID == [16]byte{} {
		t.Fatal("ID not assigned")
	}
	if p.Name != "Example" || p.Description != "A sample project" {
		t.Fatalf("fields not retained: %+v", p)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatal("timestamps not assigned")
	}
	if p.CreatedAt.Location() != time.UTC || p.UpdatedAt.Location() != time.UTC {
		t.Fatal("timestamps not UTC")
	}
}

// T2.1.2 — Empty name rejected.
func TestNewEmptyNameRejected(t *testing.T) {
	if _, err := New("", "desc"); err == nil {
		t.Fatal("empty name accepted")
	}
}

// T2.1.3 — Whitespace-only name rejected.
func TestNewWhitespaceNameRejected(t *testing.T) {
	if _, err := New("   \t\n", "desc"); err == nil {
		t.Fatal("whitespace-only name accepted")
	}
}

// T2.1.4 — Initial status defaults to DRAFT.
func TestNewDefaultsToDraft(t *testing.T) {
	p, err := New("Name", "")
	if err != nil {
		t.Fatalf("valid project rejected: %v", err)
	}
	if p.Status != StatusDRAFT {
		t.Fatalf("initial status %v, want DRAFT", p.Status)
	}
}

// T2.1.5 — Unknown project status rejected.
func TestUnknownStatusRejected(t *testing.T) {
	if _, err := ParseStatus("NOT_A_STATUS"); err == nil {
		t.Fatal("unknown status accepted")
	}
	for _, valid := range []string{"DRAFT", "READY", "RUNNING", "PAUSED", "COMPLETED", "FAILED", "CANCELLED"} {
		if _, err := ParseStatus(valid); err != nil {
			t.Fatalf("status %s rejected: %v", valid, err)
		}
	}
}

// T2.1.6 — Project ID cannot silently change after creation.
func TestIDImmutable(t *testing.T) {
	p, err := New("Name", "desc")
	if err != nil {
		t.Fatalf("valid project rejected: %v", err)
	}
	original := p.ID
	p.Name = "Renamed"
	p.Description = "Changed"
	if p.ID != original {
		t.Fatalf("ID changed: %x -> %x", original, p.ID)
	}
}

func TestStatusStringRoundTrip(t *testing.T) {
	if strings.TrimSpace(StatusDRAFT.String()) == "" {
		t.Fatal("DRAFT has no string form")
	}
	if StatusDRAFT.String() != "DRAFT" {
		t.Fatalf("DRAFT.String() = %q", StatusDRAFT.String())
	}
}
