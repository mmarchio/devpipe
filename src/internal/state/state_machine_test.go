package state

import (
	"testing"

	"github.com/google/uuid"
)

// T6.1.1 — Valid state machine created.
func TestT611ValidMachineCreated(t *testing.T) {
	m, err := NewMachine(MachineInput{
		ProjectID:   uuid.New(),
		Name:        "Default Development Workflow",
		Description: "Standard development workflow.",
	})
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	if m.ID == uuid.Nil {
		t.Fatalf("new machine has no ID")
	}
	if m.ProjectID == uuid.Nil {
		t.Fatalf("new machine has no project association")
	}
	if m.Name != "Default Development Workflow" || m.Description != "Standard development workflow." {
		t.Fatalf("name/description not preserved: %q / %q", m.Name, m.Description)
	}
	if m.Version != 1 {
		t.Fatalf("initial version = %d, want 1", m.Version)
	}
	if !m.Enabled {
		t.Fatalf("new machine must start enabled")
	}
	if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Fatalf("timestamps must be set on creation")
	}
}

// T6.1.2 — Project association required.
func TestT612ProjectAssociationRequired(t *testing.T) {
	if _, err := NewMachine(MachineInput{
		ProjectID: uuid.Nil,
		Name:      "No Project",
	}); err == nil {
		t.Fatalf("machine without a project: expected rejection, got none")
	}
}

// T6.1.3 — Empty name rejected.
func TestT613EmptyNameRejected(t *testing.T) {
	for _, name := range []string{"", "   ", "\t\n"} {
		if _, err := NewMachine(MachineInput{ProjectID: uuid.New(), Name: name}); err == nil {
			t.Fatalf("name %q: expected rejection, got none", name)
		}
	}
}

// T6.1.4 — Initial version is 1.
func TestT614InitialVersionIsOne(t *testing.T) {
	for i := 0; i < 3; i++ {
		m, err := NewMachine(MachineInput{ProjectID: uuid.New(), Name: "M"})
		if err != nil {
			t.Fatalf("NewMachine: %v", err)
		}
		if m.Version != 1 {
			t.Fatalf("machine %d initial version = %d, want 1", i, m.Version)
		}
	}
}

// T6.1.5 — Enabled status persisted on the value.
func TestT615EnabledStatusPersisted(t *testing.T) {
	pid := uuid.New()
	m, err := NewMachine(MachineInput{ProjectID: pid, Name: "E"})
	if err != nil {
		t.Fatalf("NewMachine: %v", err)
	}
	if !m.Enabled {
		t.Fatalf("new machine must start enabled")
	}
	m.Enabled = false
	if m.Enabled {
		t.Fatalf("Enabled must be persisted on the domain value")
	}
	disabled := false
	d, err := NewMachine(MachineInput{ProjectID: pid, Name: "D", Enabled: &disabled})
	if err != nil {
		t.Fatalf("NewMachine disabled: %v", err)
	}
	if d.Enabled {
		t.Fatalf("explicitly disabled machine must stay disabled")
	}
}

// T6.1.6 — Unknown project rejected (domain half).
// A zero ProjectID is the only expressible unknown project at the domain
// level; the DB FK half lands in P6-05.
func TestT616UnknownProjectRejectedDomain(t *testing.T) {
	if _, err := NewMachine(MachineInput{ProjectID: uuid.Nil, Name: "Orphan"}); err == nil {
		t.Fatalf("machine with unknown project: expected rejection, got none")
	}
}

// Compile-level guard: the constructor takes no context (pure domain).
var _ = func() (*Machine, error) {
	return NewMachine(MachineInput{ProjectID: uuid.Nil, Name: ""})
}
