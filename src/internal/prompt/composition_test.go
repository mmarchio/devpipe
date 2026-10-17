package prompt

import (
	"testing"

	"github.com/google/uuid"
)

// P8-15 — WBS 8.9 canonical composition order, TDD T8.9.1-T8.9.6.
//
// The canonical order is the WBS 8.9 11-position order projected onto the
// P8-06 accepted 9-type component set: positions 6 (Architecture Context)
// and 10 (State-specific Prompt) have no type in the accepted set, so the
// known types map to the remaining positions with the same relative order:
// SYSTEM(1), ROLE_INSTRUCTION(2), STATE_INSTRUCTION(3), PROJECT_CONTEXT(4),
// PHASE_CONTEXT(5), TASK_CONTEXT(7), TEST_CONTEXT(8), CONTINUATION_CONTEXT(9),
// RESPONSE_PROTOCOL(11).

func comp89(t *testing.T, typ PromptComponentType, content string) PromptComponent {
	t.Helper()
	c, err := NewPromptComponent(typ, SourceSystemDefault, 0, content, uuid.New(), 1, "h")
	if err != nil {
		t.Fatalf("NewPromptComponent(%s): %v", typ, err)
	}
	return c
}

func TestT891_SystemPromptPrecedesRoleInstructions(t *testing.T) {
	in := Components{
		comp89(t, ComponentRoleInstruction, "role-a"),
		comp89(t, ComponentSystem, "system"),
	}
	out := CanonicalOrder(in)
	if len(out) != 2 || out[0].Type != ComponentSystem || out[1].Type != ComponentRoleInstruction {
		t.Fatalf("system prompt must precede role instructions; got %v", typesOf(out))
	}
}

func TestT892_RoleInstructionsPreserveConfiguredOrder(t *testing.T) {
	in := Components{
		comp89(t, ComponentRoleInstruction, "role-2"),
		comp89(t, ComponentRoleInstruction, "role-1"),
		comp89(t, ComponentRoleInstruction, "role-3"),
	}
	out := CanonicalOrder(in)
	if len(out) != 3 {
		t.Fatalf("want 3 components, got %d", len(out))
	}
	for i, want := range []string{"role-2", "role-1", "role-3"} {
		if out[i].Type != ComponentRoleInstruction || out[i].Content != want {
			t.Fatalf("configured order not preserved at %d: got %s/%q", i, out[i].Type, out[i].Content)
		}
	}
}

func TestT893_StateInstructionsCorrectlyPlaced(t *testing.T) {
	// Input order is deliberately the reverse of the canonical order.
	in := Components{
		comp89(t, ComponentProjectContext, "project"),
		comp89(t, ComponentStateInstruction, "state-a"),
		comp89(t, ComponentRoleInstruction, "role"),
		comp89(t, ComponentStateInstruction, "state-b"),
	}
	out := CanonicalOrder(in)
	if len(out) != 4 {
		t.Fatalf("want 4 components, got %d", len(out))
	}
	// State instructions sit between role instructions and project context.
	if out[0].Type != ComponentRoleInstruction || out[1].Type != ComponentStateInstruction ||
		out[2].Type != ComponentStateInstruction || out[3].Type != ComponentProjectContext {
		t.Fatalf("state instructions not in canonical position: %v", typesOf(out))
	}
	if out[1].Content != "state-a" || out[2].Content != "state-b" {
		t.Fatalf("same-type configured order lost: %q %q", out[1].Content, out[2].Content)
	}
}

func TestT894_TaskContextCorrectlyPlaced(t *testing.T) {
	in := Components{
		comp89(t, ComponentTestContext, "tests"),
		comp89(t, ComponentPhaseContext, "phase"),
		comp89(t, ComponentTaskContext, "task"),
	}
	out := CanonicalOrder(in)
	if len(out) != 3 || out[0].Type != ComponentPhaseContext || out[1].Type != ComponentTaskContext || out[2].Type != ComponentTestContext {
		t.Fatalf("task context not in canonical position: %v", typesOf(out))
	}
}

func TestT895_ResponseProtocolLastWhereConfigured(t *testing.T) {
	in := Components{
		comp89(t, ComponentResponseProtocol, "protocol"),
		comp89(t, ComponentSystem, "system"),
		comp89(t, ComponentContinuationContext, "continuation"),
	}
	out := CanonicalOrder(in)
	last := out[len(out)-1]
	if last.Type != ComponentResponseProtocol || last.Content != "protocol" {
		t.Fatalf("response protocol must be last: %v", typesOf(out))
	}
	if out[0].Type != ComponentSystem || out[1].Type != ComponentContinuationContext {
		t.Fatalf("remaining canonical order broken: %v", typesOf(out))
	}
}

func TestT896_RepeatedCompositionByteIdentical(t *testing.T) {
	srcID := uuid.New() // fixed id: repeated construction is comparable
	build := func() Components {
		mk := func(typ PromptComponentType, content string, version int) PromptComponent {
			c, err := NewPromptComponent(typ, SourceSystemDefault, 0, content, srcID, version, "h")
			if err != nil {
				t.Fatalf("NewPromptComponent(%s): %v", typ, err)
			}
			return c
		}
		return Components{
			mk(ComponentTestContext, "tests", 3),
			mk(ComponentRoleInstruction, "role-1", 1),
			mk(ComponentSystem, "system", 1),
			mk(ComponentStateInstruction, "state", 2),
			mk(ComponentResponseProtocol, "protocol", 1),
			mk(ComponentTaskContext, "task", 4),
			mk(ComponentProjectContext, "project", 1),
			mk(ComponentPhaseContext, "phase", 1),
			mk(ComponentContinuationContext, "continuation", 1),
		}
	}
	first := CanonicalOrder(build())
	for i := 0; i < 50; i++ {
		again := CanonicalOrder(build())
		if len(again) != len(first) {
			t.Fatalf("repeated composition changed length: %d vs %d", len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("repeated composition not byte-identical at %d: %+v vs %+v", j, again[j], first[j])
			}
		}
	}
}

func TestP815_CanonicalOrderIsStableAndNonMutating(t *testing.T) {
	in := Components{
		comp89(t, ComponentRoleInstruction, "role-2"),
		comp89(t, ComponentRoleInstruction, "role-1"),
	}
	out := CanonicalOrder(in)
	// Input slice must not be mutated in place.
	if in[0].Content != "role-2" || in[1].Content != "role-1" {
		t.Fatalf("CanonicalOrder mutated its input: %v", typesOf(in))
	}
	if out[0].Content != "role-2" || out[1].Content != "role-1" {
		t.Fatalf("same-position order must be stable: %v", typesOf(out))
	}
	// An empty input composes to an empty output.
	if out := CanonicalOrder(Components{}); len(out) != 0 {
		t.Fatalf("empty input must compose to empty output, got %d", len(out))
	}
}

func typesOf(in Components) []PromptComponentType {
	out := make([]PromptComponentType, len(in))
	for i, c := range in {
		out[i] = c.Type
	}
	return out
}
