package prompt

import (
	"errors"
	"overseer/internal/role"
	"testing"
)

// P8-17 — WBS 8.11 role-instruction composition, TDD T8.11.1-T8.11.4.
//
// Composition renders the RoleSnapshot's ordered instruction set (WBS
// 5.14 assignment order) as ROLE_INSTRUCTION components: every assigned
// instruction is included, at its snapshot-captured version and content,
// with the snapshot's source hash retained, and a later instruction edit
// is invisible because resolution reads only the snapshot.

func instrSnap(key, name string, version int, content, hash string) role.InstructionSnapshot {
	return role.InstructionSnapshot{
		Key:            key,
		Name:           name,
		Version:        version,
		Content:        content,
		ContentHash:    hash,
		Classification: role.ClassificationRoleInstruction,
	}
}

// T8.11.1 — All instructions included: every assigned instruction
// appears in the output, exactly once.
func TestT8111_AllInstructionsIncluded(t *testing.T) {
	snap := role.RoleSnapshot{
		RoleKey: "ENGINEER",
		Instructions: []role.InstructionSnapshot{
			instrSnap("ENGINEER", "ENGINEER.md", 1, "engineer rules", "h-eng"),
			instrSnap("TDD", "TDD.md", 2, "write the failing test first", "h-tdd"),
			instrSnap("CODING_STANDARDS", "CODING_STANDARDS.md", 1, "standards", "h-cs"),
		},
	}
	comps, err := ComposeRoleInstructions(snap)
	if err != nil {
		t.Fatalf("ComposeRoleInstructions: %v", err)
	}
	if len(comps) != 3 {
		t.Fatalf("want all 3 instructions, got %d", len(comps))
	}
	seen := map[string]bool{}
	for _, c := range comps {
		if c.Type != ComponentRoleInstruction {
			t.Fatalf("want ROLE_INSTRUCTION components, got %s", c.Type)
		}
		if c.Source != SourceRoleInstruction {
			t.Fatalf("want ROLE_INSTRUCTION source, got %s", c.Source)
		}
		seen[c.Content] = true
	}
	for _, want := range []string{"engineer rules", "write the failing test first", "standards"} {
		if !seen[want] {
			t.Fatalf("instruction %q missing from composition", want)
		}
	}
}

// T8.11.2 — Correct versions included: each component carries its
// snapshot-captured version and source hash.
func TestT8112_CorrectVersionsIncluded(t *testing.T) {
	snap := role.RoleSnapshot{
		RoleKey: "ENGINEER",
		Instructions: []role.InstructionSnapshot{
			instrSnap("TDD", "TDD.md", 2, "tdd-v2", "h-tdd-2"),
			instrSnap("ENGINEER", "ENGINEER.md", 5, "eng-v5", "h-eng-5"),
		},
	}
	comps, err := ComposeRoleInstructions(snap)
	if err != nil {
		t.Fatalf("ComposeRoleInstructions: %v", err)
	}
	want := map[string]struct {
		version int
		hash    string
	}{
		"tdd-v2": {2, "h-tdd-2"},
		"eng-v5": {5, "h-eng-5"},
	}
	for _, c := range comps {
		w, ok := want[c.Content]
		if !ok {
			t.Fatalf("unexpected content %q", c.Content)
		}
		if c.SourceVersion != w.version {
			t.Fatalf("snapshot version must be used; want %d, got %d", w.version, c.SourceVersion)
		}
		if c.SourceHash != w.hash {
			t.Fatalf("source hash not retained; want %q, got %q", w.hash, c.SourceHash)
		}
	}
}

// T8.11.3 — Correct order maintained: output order is the snapshot
// assignment order, never re-sorted.
func TestT8113_CorrectOrderMaintained(t *testing.T) {
	mk := func() role.RoleSnapshot {
		return role.RoleSnapshot{
			RoleKey: "ENGINEER",
			Instructions: []role.InstructionSnapshot{
				instrSnap("ENGINEER", "ENGINEER.md", 1, "eng", "h1"),
				instrSnap("TDD", "TDD.md", 1, "tdd", "h2"),
				instrSnap("CODING_STANDARDS", "CODING_STANDARDS.md", 1, "cs", "h3"),
			},
		}
	}
	wantOrder := []string{"eng", "tdd", "cs"}
	comps, err := ComposeRoleInstructions(mk())
	if err != nil {
		t.Fatalf("ComposeRoleInstructions: %v", err)
	}
	for i, want := range wantOrder {
		if comps[i].Content != want {
			t.Fatalf("assignment order not preserved at %d; want %q, got %q", i, want, comps[i].Content)
		}
	}
	// Deliberately non-alphabetical order survives too.
	s2 := mk()
	s2.Instructions = append([]role.InstructionSnapshot{}, s2.Instructions[2], s2.Instructions[0], s2.Instructions[1])
	comps, err = ComposeRoleInstructions(s2)
	if err != nil {
		t.Fatalf("ComposeRoleInstructions(swapped): %v", err)
	}
	for i, want := range []string{"cs", "eng", "tdd"} {
		if comps[i].Content != want {
			t.Fatalf("swapped assignment order not preserved at %d; want %q, got %q", i, want, comps[i].Content)
		}
	}
}

// T8.11.4 — Later instruction edit doesn't affect historical
// composition: resolution reads only the snapshot, so a later v7 current
// instruction has no input to the composition.
func TestT8114_LaterEditDoesNotAffectHistoricalComposition(t *testing.T) {
	snap := role.RoleSnapshot{
		RoleKey: "ENGINEER",
		Instructions: []role.InstructionSnapshot{
			instrSnap("TDD", "TDD.md", 4, "tdd-v4", "h-tdd-4"),
		},
	}
	first, err := ComposeRoleInstructions(snap)
	if err != nil {
		t.Fatalf("ComposeRoleInstructions: %v", err)
	}
	// The role later re-edits the instruction (v7) — irrelevant here:
	// the historical snapshot still pins v4.
	_ = instrSnap("TDD", "TDD.md", 7, "tdd-v7", "h-tdd-7")
	for i := 0; i < 50; i++ {
		again, err := ComposeRoleInstructions(snap)
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
		if again[0].SourceVersion != 4 || again[0].Content != "tdd-v4" {
			t.Fatalf("historical version drifted: %+v", again[0])
		}
	}
}

// A cleared captured version/content is a composition failure for that
// instruction (the 8.10 missing-version contract applied per instruction).
func TestP817_MissingCapturedInstructionFails(t *testing.T) {
	for _, bad := range []role.InstructionSnapshot{
		{Key: "TDD", Name: "TDD.md", Classification: role.ClassificationRoleInstruction}, // zero
		instrSnap("TDD", "TDD.md", 0, "", ""),
		instrSnap("TDD", "TDD.md", 3, "", "h3"), // version but no content
	} {
		snap := role.RoleSnapshot{RoleKey: "ENGINEER", Instructions: []role.InstructionSnapshot{bad}}
		_, err := ComposeRoleInstructions(snap)
		if err == nil {
			t.Fatalf("missing captured instruction must fail composition: %+v", bad)
		}
		if !errors.Is(err, ErrMissingInstructionVersion) {
			t.Fatalf("want ErrMissingInstructionVersion, got %v", err)
		}
	}
}

// An empty assignment set composes to zero components (no implicit
// fallback content).
func TestP817_EmptyInstructionSetIsEmpty(t *testing.T) {
	comps, err := ComposeRoleInstructions(role.RoleSnapshot{RoleKey: "ENGINEER"})
	if err != nil {
		t.Fatalf("ComposeRoleInstructions(empty): %v", err)
	}
	if len(comps) != 0 {
		t.Fatalf("empty assignment set must compose to zero components, got %d", len(comps))
	}
}
