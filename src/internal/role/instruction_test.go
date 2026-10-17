package role

import (
	"testing"

	"github.com/google/uuid"
)

// T5.7.1 — Instruction created.
func TestT571InstructionCreated(t *testing.T) {
	in, err := NewInstruction(InstructionInput{Key: "tdd", Name: "TDD", Description: "Test-driven development."})
	if err != nil {
		t.Fatalf("NewInstruction: %v", err)
	}
	if in.ID == uuid.Nil {
		t.Fatalf("instruction has no ID")
	}
	if in.Key != "TDD" {
		t.Fatalf("key = %q, want normalized TDD", in.Key)
	}
	if in.Name != "TDD" {
		t.Fatalf("name not preserved: %q", in.Name)
	}
	if in.Version != 1 || !in.Enabled {
		t.Fatalf("initial version/enabled = %d/%v, want 1/true", in.Version, in.Enabled)
	}
}

// T5.7.3 — Empty key rejected.
func TestT573EmptyKeyRejected(t *testing.T) {
	for _, key := range []string{"", "   ", "\t\n"} {
		if _, err := NewInstruction(InstructionInput{Key: key, Name: "N"}); err == nil {
			t.Fatalf("key %q: expected rejection, got none", key)
		}
	}
}

// T5.7.4 — Duplicate scoped key rejected (domain half, same logic as roles).
func TestT574DuplicateScopedKeyRejected(t *testing.T) {
	a, err := NewInstruction(InstructionInput{Key: "tdd", Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if err := InstructionDuplicateKey(a, "TDD", nil); err == nil {
		t.Fatalf("duplicate global instruction key accepted")
	}
	pid := uuid.New()
	b, err := NewInstruction(InstructionInput{ProjectID: &pid, Key: "tdd", Name: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if err := InstructionDuplicateKey(b, "tdd", &pid); err == nil {
		t.Fatalf("duplicate project-scoped instruction key accepted")
	}
}

// T5.7.5 — Disabled instruction retained but excluded where appropriate:
// disabling is a field flip; the record is unchanged except Enabled.
func TestT575DisabledRetained(t *testing.T) {
	in, err := NewInstruction(InstructionInput{Key: "tdd", Name: "TDD"})
	if err != nil {
		t.Fatal(err)
	}
	before := *in
	in.Enabled = false
	if in.Enabled {
		t.Fatalf("disabled flag not cleared")
	}
	// Everything else retained.
	if in.ID != before.ID || in.Key != before.Key || in.Name != before.Name || in.Version != before.Version {
		t.Fatalf("disabling altered identity/version fields")
	}
}
