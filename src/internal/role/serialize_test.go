package role

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fullSnapshotFixture returns a RoleSnapshot with every component populated:
// the full surface a serializer must cover (WBS 5.30).
func fullSnapshotFixture() *RoleSnapshot {
	return &RoleSnapshot{
		RoleID:      uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		RoleKey:     "ENGINEER",
		RoleVersion: 3,
		SystemPrompt: PromptSnapshot{
			Version:        3,
			Content:        "You are the ENGINEER.\nImplement only the assigned work unit.",
			ContentHash:    HashSystemPromptContent("You are the ENGINEER.\nImplement only the assigned work unit."),
			Classification: ClassificationSystem,
		},
		Instructions: []InstructionSnapshot{
			{
				Key:            "TDD",
				Name:           "TDD.md",
				Version:        2,
				Content:        "Write the failing test first.",
				ContentHash:    HashSystemPromptContent("Write the failing test first."),
				Classification: ClassificationRoleInstruction,
			},
			{
				Key:            "ENGINEER",
				Name:           "ENGINEER.md",
				Version:        4,
				Content:        "Implement the work unit.",
				ContentHash:    HashSystemPromptContent("Implement the work unit."),
				Classification: ClassificationRoleInstruction,
			},
		},
		AllowedStates: []string{"continuation", "implementation"},
		StateFiles:    []string{"ACTIVE_TASK", "PROJECT_STATE"},
	}
}

// T5.30.1 — Same snapshot serializes identically (byte-for-byte stable across
// repeated calls; unchanged configuration never changes the output).
func TestT5301SameSnapshotSerializesIdentically(t *testing.T) {
	snap := fullSnapshotFixture()
	first := snap.Serialize()
	for i := 0; i < 100; i++ {
		if got := snap.Serialize(); got != first {
			t.Fatalf("serialization not byte-stable: call %d differs", i)
		}
	}
	if !strings.Contains(first, "ENGINEER") || !strings.Contains(first, "\"implementation\"") {
		t.Fatalf("serialized form missing recognizable role/state content: %q", first)
	}
	// An independently constructed, equal snapshot serializes to the same bytes.
	clone := fullSnapshotFixture()
	if clone.Serialize() != first {
		t.Fatal("two equal snapshots produced different serializations")
	}
}

// T5.30.2 — Map iteration/order doesn't alter output: two logically equal
// snapshots whose list orders differ (storage order vs input order, the
// deduplicated state sets) must serialize identically.
func TestT5302OrderDoesNotAlterOutput(t *testing.T) {
	base := fullSnapshotFixture()
	want := base.Serialize()

	reorderedStates := fullSnapshotFixture()
	reorderedStates.AllowedStates = []string{"implementation", "continuation"}
	if got := reorderedStates.Serialize(); got != want {
		t.Fatal("reordered allowed states changed the serialized output")
	}

	reorderedFiles := fullSnapshotFixture()
	reorderedFiles.StateFiles = []string{"PROJECT_STATE", "ACTIVE_TASK"}
	if got := reorderedFiles.Serialize(); got != want {
		t.Fatal("reordered state files changed the serialized output")
	}
}

// T5.30.3 — Instruction order deterministic: serialization preserves assignment
// order, and a permutation of the instructions changes the representation.
func TestT5303InstructionOrderDeterministic(t *testing.T) {
	base := fullSnapshotFixture()
	want := base.Serialize()
	if !strings.Contains(want, "instruction:TDD@2") || !strings.Contains(want, "instruction:ENGINEER@4") {
		t.Fatalf("serialized form missing instruction entries: %q", want)
	}
	// Deterministic under repetition.
	if base.Serialize() != want {
		t.Fatal("instruction serialization is not deterministic")
	}
	// Assignment order is preserved: the first instruction appears before the
	// second in the serialized output.
	iTdd := strings.Index(want, "instruction:TDD@2")
	iEng := strings.Index(want, "instruction:ENGINEER@4")
	if iTdd == -1 || iEng == -1 || iTdd > iEng {
		t.Fatalf("instruction order not preserved in serialization: TDD@%d ENGINEER@%d", iTdd, iEng)
	}
	// Swapping the assignment order changes the representation (deliberate
	// order sensitivity, matching WBS 5.16 instruction-order semantics).
	swapped := fullSnapshotFixture()
	swapped.Instructions = []InstructionSnapshot{fullSnapshotFixture().Instructions[1], fullSnapshotFixture().Instructions[0]}
	if got := swapped.Serialize(); got == want {
		t.Fatal("swapped instruction order produced an identical serialization")
	}
}

// T5.30.4 — Configuration modification changes the serialized representation:
// every relevant component is represented in the output.
func TestT5304ConfigurationModificationChangesRepresentation(t *testing.T) {
	base := fullSnapshotFixture()
	want := base.Serialize()

	mutations := map[string]func(*RoleSnapshot){
		"role key":       func(s *RoleSnapshot) { s.RoleKey = "REVIEWER" },
		"role version":   func(s *RoleSnapshot) { s.RoleVersion = 4 },
		"prompt version": func(s *RoleSnapshot) { s.SystemPrompt.Version = 4 },
		"prompt content": func(s *RoleSnapshot) {
			s.SystemPrompt.Content = "You are the ENGINEER. (revised)"
			s.SystemPrompt.ContentHash = HashSystemPromptContent(s.SystemPrompt.Content)
		},
		"prompt classification": func(s *RoleSnapshot) { s.SystemPrompt.Classification = ClassificationRoleInstruction },
		"drop instruction":      func(s *RoleSnapshot) { s.Instructions = s.Instructions[:1] },
		"instruction content": func(s *RoleSnapshot) {
			s.Instructions[0].Content = "Write the failing test first. (v3)"
			s.Instructions[0].ContentHash = HashSystemPromptContent(s.Instructions[0].Content)
		},
		"instruction version":        func(s *RoleSnapshot) { s.Instructions[1].Version = 5 },
		"instruction name":           func(s *RoleSnapshot) { s.Instructions[0].Name = "TDD-revised.md" },
		"instruction classification": func(s *RoleSnapshot) { s.Instructions[0].Classification = ClassificationSystem },
		"allowed states":             func(s *RoleSnapshot) { s.AllowedStates = []string{"implementation", "review"} },
		"state files":                func(s *RoleSnapshot) { s.StateFiles = []string{"ACTIVE_TASK"} },
	}
	for name, mutate := range mutations {
		mutated := fullSnapshotFixture()
		mutate(mutated)
		if got := mutated.Serialize(); got == want {
			t.Fatalf("mutation %q did not change the serialized representation", name)
		}
	}
}
