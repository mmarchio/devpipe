package prompt

import (
	"testing"

	"github.com/google/uuid"
)

// P8-06 — WBS 8.8 prompt-component model, T8.8.1–T8.8.5.
//
// Accepted component type set (pinned per NOTES; the WBS list is
// illustrative): SYSTEM, ROLE_INSTRUCTION, STATE_INSTRUCTION,
// PROJECT_CONTEXT, PHASE_CONTEXT, TASK_CONTEXT, TEST_CONTEXT,
// CONTINUATION_CONTEXT, RESPONSE_PROTOCOL.

func TestT8_8_1ComponentRetainsSourceType(t *testing.T) {
	for _, tc := range []struct {
		typ    PromptComponentType
		src    PromptSource
		expect PromptComponentType
	}{
		{ComponentSystem, SourceSystemDefault, ComponentSystem},
		{ComponentRoleInstruction, SourceRoleInstruction, ComponentRoleInstruction},
		{ComponentStateInstruction, SourceStateInstruction, ComponentStateInstruction},
		{ComponentProjectContext, SourceStateFile, ComponentProjectContext},
		{ComponentPhaseContext, SourceStateFile, ComponentPhaseContext},
		{ComponentTaskContext, SourceWorkUnit, ComponentTaskContext},
		{ComponentTestContext, SourceStateFile, ComponentTestContext},
		{ComponentContinuationContext, SourceContinuation, ComponentContinuationContext},
		{ComponentResponseProtocol, SourceResponseProtocol, ComponentResponseProtocol},
	} {
		c, err := NewPromptComponent(tc.typ, tc.src, 1, "content", uuid.New(), 1, "abc123")
		if err != nil {
			t.Fatalf("NewPromptComponent(%s): %v", tc.typ, err)
		}
		if c.Type != tc.expect {
			t.Errorf("Type = %q, want %q", c.Type, tc.expect)
		}
		if c.Source != tc.src {
			t.Errorf("Source = %q, want %q", c.Source, tc.src)
		}
	}
}

func TestT8_8_2VersionRetained(t *testing.T) {
	c, err := NewPromptComponent(ComponentRoleInstruction, SourceRoleInstruction, 0, "prompt v3", uuid.New(), 3, "hash-v3")
	if err != nil {
		t.Fatalf("NewPromptComponent: %v", err)
	}
	if c.SourceVersion != 3 {
		t.Fatalf("SourceVersion = %d, want 3", c.SourceVersion)
	}
}

func TestT8_8_3HashRetained(t *testing.T) {
	id := uuid.New()
	c, err := NewPromptComponent(ComponentTaskContext, SourceWorkUnit, 2, "task ctx", id, 1, ContentHash("task ctx"))
	if err != nil {
		t.Fatalf("NewPromptComponent: %v", err)
	}
	want := ContentHash("task ctx")
	if c.SourceHash != want {
		t.Errorf("SourceHash = %q, want %q", c.SourceHash, want)
	}
	if c.SourceID != id {
		t.Errorf("SourceID = %v, want %v", c.SourceID, id)
	}
}

func TestT8_8_4OrderingRetained(t *testing.T) {
	// Declared order differs from insertion order; the collection must
	// retain the declared Order fields and ByOrder must restore
	// declared order deterministically.
	c1, _ := NewPromptComponent(ComponentSystem, SourceSystemDefault, 3, "system", uuid.Nil, 1, "h1")
	c2, _ := NewPromptComponent(ComponentRoleInstruction, SourceRoleInstruction, 1, "role", uuid.Nil, 1, "h2")
	c3, _ := NewPromptComponent(ComponentTaskContext, SourceWorkUnit, 2, "task", uuid.Nil, 1, "h3")
	list := Components{c1, c2, c3}
	for i, want := range []int{3, 1, 2} {
		if list[i].Order != want {
			t.Fatalf("list[%d].Order = %d, want %d (insertion order must be retained)", i, list[i].Order, want)
		}
	}
	ordered := list.ByOrder()
	for i, want := range []PromptComponentType{ComponentRoleInstruction, ComponentTaskContext, ComponentSystem} {
		if ordered[i].Type != want {
			t.Fatalf("ByOrder()[%d].Type = %s, want %s", i, ordered[i].Type, want)
		}
	}
	if len(list) != 3 || list[0].Order != 3 {
		t.Fatalf("ByOrder must not mutate the receiver")
	}
}

func TestT8_8_5UnknownTypeRejected(t *testing.T) {
	if _, err := NewPromptComponent(PromptComponentType("NOT_A_TYPE"), SourceSystemDefault, 0, "x", uuid.Nil, 0, ""); err == nil {
		t.Fatalf("unknown type accepted, want rejection")
	}
	if PromptComponentType("").Known() {
		t.Fatalf("empty type reported Known")
	}
	if PromptComponentType("system").Known() {
		t.Fatalf("lowercase type reported Known; the accepted set is case-sensitive")
	}
}
