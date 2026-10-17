package prompt

import (
	"testing"

	"overseer/internal/state"
)

// P8-18 — WBS 8.12 state-instruction composition, TDD T8.12.1-T8.12.4.
//
// Composition resolves the EXACT state-instruction version captured in
// the StateMachineSnapshot (Phase 6, WBS 6.22) for the CURRENT state as
// a STATE_INSTRUCTION component: the snapshot's captured version and
// content hash are retained, instructions of other states are excluded,
// a later instruction edit is invisible (resolution reads only the
// snapshot), and a control-only state without a captured instruction
// composes to zero STATE_INSTRUCTION components (no implicit fallback —
// the WBS 8.12 task-level choice, unlike the P8-16/P8-17 missing-version
// failure contract where a cleared captured instruction IS a failure).
//
// Resolution source (stated per ACTIVE_TASK NOTES): the component's
// content is the captured instruction content at the snapshot's pinned
// version — the composition reads only snapshot-captured values, exactly
// mirroring the P8-16/P8-17 pattern (the RoleSnapshot carries
// version/content/hash and composition consumes them verbatim).

// snap818 builds a two-state machine snapshot: the named current state
// carries the given captured instruction version/hash (content is the
// instruction text at that version), the other state carries its own
// instruction.
func snap818(current, other string, version int, content, hash string) (state.StateMachineSnapshot, string) {
	return state.StateMachineSnapshot{
		Version: 1,
		States: []state.StateSnapshot{
			{Key: current, InstructionVersion: version, InstructionHash: hash},
			{Key: other, InstructionVersion: 2, InstructionHash: "h-other"},
		},
	}, content
}

// T8.12.1 — Correct state instruction included: the current state's
// captured instruction becomes exactly one STATE_INSTRUCTION component.
func TestT8121_CorrectStateInstructionIncluded(t *testing.T) {
	snap, _ := snap818("FIX", "VALIDATE", 3, "Correct the validation failures for the current work unit.\nDo not begin unrelated work.", "h-fix-3")
	comps, err := ComposeStateInstruction(snap, "FIX", "fix-instruction-v3")
	if err != nil {
		t.Fatalf("ComposeStateInstruction(FIX): %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("want exactly 1 STATE_INSTRUCTION component, got %d", len(comps))
	}
	c := comps[0]
	if c.Type != ComponentStateInstruction {
		t.Fatalf("want STATE_INSTRUCTION, got %s", c.Type)
	}
	if c.Source != SourceStateInstruction {
		t.Fatalf("want STATE_INSTRUCTION source, got %s", c.Source)
	}
	if c.SourceVersion != 3 {
		t.Fatalf("snapshot version must be used; want 3, got %d", c.SourceVersion)
	}
	if c.SourceHash != "h-fix-3" {
		t.Fatalf("source hash not retained; want h-fix-3, got %q", c.SourceHash)
	}
	if c.Content != "fix-instruction-v3" {
		t.Fatalf("captured content not used; got %q", c.Content)
	}
}

// T8.12.2 — Wrong-state instructions excluded: composing for a state
// other than the current one yields no component; a state with no
// captured instruction at all yields none either (no implicit fallback
// to any other state's instruction).
func TestT8122_WrongStateInstructionsExcluded(t *testing.T) {
	snap, _ := snap818("FIX", "VALIDATE", 3, "fix-v3", "h-fix-3")

	// A snapshot pinned at FIX must not leak VALIDATE's instruction.
	comps, err := ComposeStateInstruction(snap, "FIX", "fix-v3")
	if err != nil {
		t.Fatalf("ComposeStateInstruction(FIX): %v", err)
	}
	for _, c := range comps {
		if c.SourceVersion != 3 || c.SourceHash != "h-fix-3" {
			t.Fatalf("wrong-state instruction leaked: %+v", c)
		}
	}

	// The other state's own snapshot composes only its own instruction.
	other, _ := snap818("VALIDATE", "FIX", 2, "validate-v2", "h-validate-2")
	comps, err = ComposeStateInstruction(other, "VALIDATE", "validate-v2")
	if err != nil {
		t.Fatalf("ComposeStateInstruction(VALIDATE): %v", err)
	}
	if len(comps) != 1 || comps[0].SourceVersion != 2 || comps[0].SourceHash != "h-validate-2" {
		t.Fatalf("expected only VALIDATE's own instruction, got %+v", comps)
	}

	// A state with no captured instruction composes to zero components.
	noInstr := state.StateMachineSnapshot{Version: 1, States: []state.StateSnapshot{{Key: "EVALUATE", Kind: "CONTROL"}}}
	comps, err = ComposeStateInstruction(noInstr, "EVALUATE", "")
	if err != nil {
		t.Fatalf("ComposeStateInstruction(no-instruction): %v", err)
	}
	if len(comps) != 0 {
		t.Fatalf("state without a captured instruction must compose to zero components, got %+v", comps)
	}
}

// T8.12.3 — Historical version respected: the composition pins the
// snapshot's captured version (v3); a later current version (v7) has no
// input to the composition and cannot leak in.
func TestT8123_HistoricalVersionRespected(t *testing.T) {
	snap, _ := snap818("IMPLEMENT", "FIX", 3, "implement-v3", "h-impl-3")
	first, err := ComposeStateInstruction(snap, "IMPLEMENT", "implement-instruction-v3")
	if err != nil {
		t.Fatalf("ComposeStateInstruction(v3): %v", err)
	}
	// The instruction is later re-edited (v7) — irrelevant to this
	// historical composition: the snapshot still pins v3.
	_ = state.StateSnapshot{Key: "IMPLEMENT", InstructionVersion: 7, InstructionHash: "h-impl-7"}
	for i := 0; i < 50; i++ {
		again, err := ComposeStateInstruction(snap, "IMPLEMENT", "implement-instruction-v3")
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(again) != len(first) {
			t.Fatalf("repeated composition changed length: %d vs %d", len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("later edit leaked into historical composition at %d: %+v vs %+v", j, again[j], first[j])
			}
		}
		if again[0].SourceVersion != 3 || again[0].Content != "implement-instruction-v3" {
			t.Fatalf("historical version drifted: %+v", again[0])
		}
	}
}

// T8.12.4 — Control-only state without worker instructions handled
// correctly: a control state (e.g. EVALUATE) with no captured
// instruction is a legal composition — zero STATE_INSTRUCTION
// components, no failure, no fallback content (the WBS 8.12 choice:
// "without worker instructions" is normal for control states, unlike the
// P8-16/P8-17 missing-captured-instruction failure contract).
func TestT8124_ControlOnlyStateWithoutInstructions(t *testing.T) {
	for _, key := range []string{"EVALUATE", "COMPLETE", "INCOMPLETE"} {
		snap := state.StateMachineSnapshot{
			Version: 1,
			States: []state.StateSnapshot{
				{Key: key, Kind: "CONTROL"},
				{Key: "FIX", Kind: "ACTION", InstructionVersion: 2, InstructionHash: "h-fix-2"},
			},
		}
		comps, err := ComposeStateInstruction(snap, key, "")
		if err != nil {
			t.Fatalf("control-only state %s must compose cleanly, got %v", key, err)
		}
		if len(comps) != 0 {
			t.Fatalf("control-only state %s must compose to zero components, got %+v", key, comps)
		}
	}
}

// Determinism: repeated composition over the same snapshot is
// byte-identical (same composition-input requirement as T8.9.6).
func TestT8125_RepeatedCompositionByteIdentical(t *testing.T) {
	snap, _ := snap818("FIX", "VALIDATE", 3, "fix-v3", "h-fix-3")
	first, err := ComposeStateInstruction(snap, "FIX", "fix-v3")
	if err != nil {
		t.Fatalf("ComposeStateInstruction: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := ComposeStateInstruction(snap, "FIX", "fix-v3")
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(again) != len(first) || again[0] != first[0] {
			t.Fatalf("repeated composition not byte-identical at %d: %+v vs %+v", i, again, first)
		}
	}
}
