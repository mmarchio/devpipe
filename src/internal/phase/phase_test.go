package phase

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// T2.5.1 — Valid phase constructed.
func TestNewValidPhase(t *testing.T) {
	p, err := New(uuid.New(), 1, "Phase One", "desc")
	if err != nil {
		t.Fatalf("valid phase rejected: %v", err)
	}
	if p.ID == uuid.Nil {
		t.Fatal("ID not assigned")
	}
	if p.Order != 1 || p.Name != "Phase One" {
		t.Fatalf("fields not retained: %+v", p)
	}
	if p.CreatedAt.IsZero() || p.CreatedAt.Location() != time.UTC {
		t.Fatal("CreatedAt not assigned in UTC")
	}
	if p.StartedAt != nil || p.CompletedAt != nil {
		t.Fatal("timestamps must start nil")
	}
}

// T2.5.2 — Phase requires project ID.
func TestNewRequiresProjectID(t *testing.T) {
	if _, err := New(uuid.Nil, 1, "Name", ""); err == nil {
		t.Fatal("nil project ID accepted")
	}
}

// T2.5.3 — Phase order must be positive.
func TestNewOrderMustBePositive(t *testing.T) {
	for _, order := range []int{0, -1} {
		if _, err := New(uuid.New(), order, "Name", ""); err == nil {
			t.Fatalf("order %d accepted", order)
		}
	}
	if _, err := New(uuid.New(), 1, "Name", ""); err != nil {
		t.Fatalf("order 1 rejected: %v", err)
	}
}

// T2.5.4 — Unknown phase status rejected.
func TestUnknownStatusRejected(t *testing.T) {
	if _, err := ParseStatus("BOGUS"); err == nil {
		t.Fatal("unknown status accepted")
	}
	for _, valid := range []string{"PENDING", "PLANNING", "READY", "EXECUTING", "VALIDATING", "DONE", "FAILED"} {
		if _, err := ParseStatus(valid); err != nil {
			t.Fatalf("status %s rejected: %v", valid, err)
		}
	}
}

// T2.5.5 — Initial status is PENDING.
func TestNewDefaultsToPending(t *testing.T) {
	p, _ := New(uuid.New(), 1, "Name", "")
	if p.Status != StatusPENDING {
		t.Fatalf("initial status %v, want PENDING", p.Status)
	}
}

// T2.5.6 — Completed phase can record completion timestamp.
func TestCompletionRecordsTimestamp(t *testing.T) {
	p, _ := New(uuid.New(), 1, "Name", "")
	if p.StartedAt != nil {
		t.Fatal("StartedAt must be nil before start")
	}
	if err := p.MarkStarted(); err != nil {
		t.Fatalf("mark started: %v", err)
	}
	if p.StartedAt == nil || p.StartedAt.Location() != time.UTC {
		t.Fatal("StartedAt not set in UTC")
	}
	if p.Status != StatusEXECUTING {
		t.Fatalf("status after start %v, want EXECUTING", p.Status)
	}
	if p.CompletedAt != nil {
		t.Fatal("CompletedAt must be nil before completion")
	}
	if err := p.MarkCompleted(); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	if p.Status != StatusDONE {
		t.Fatalf("status after completion %v, want DONE", p.Status)
	}
	if p.CompletedAt == nil || p.CompletedAt.Location() != time.UTC {
		t.Fatal("CompletedAt not set in UTC")
	}
	if !p.CompletedAt.After(*p.StartedAt) && !p.CompletedAt.Equal(*p.StartedAt) {
		t.Fatal("CompletedAt earlier than StartedAt")
	}
}
