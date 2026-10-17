package prompt

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/role"

	"overseer/internal/statefile"
)

// P8-19 — WBS 8.13 state-file composition, TDD T8.13.1-T8.13.5.
//
// Composition renders a Phase 7 state-file snapshot's pinned files as
// components grouped by semantic classification into the WBS 8.13
// sections (Project / Phase / Task / Test context): each file appears at
// its snapshot-captured version with the snapshot's source hash retained
// (never "latest"), and the database retrieval order (the snapshot
// file-list order) CANNOT alter the output — the classification section
// plus a deterministic in-section ordering (type key, then file id)
// fully determine the result. An optional file with no captured version
// composes to no component and no section body (no failure — the
// P8-18 zero-component choice for missing content); a REQUIRED file
// missing its captured version is a composition failure
// (ErrMissingStateFileVersion).

// pin819 is one snapshot state-file pin: the content is the file content
// at the pinned version (empty when the optional file is absent), and
// the classification is the one persisted with the pin at snapshot time
// (P7-20: classification survives reconstruction without
// re-derivation, so composition consumes the persisted value, not a
// re-computation).
func pin819(key string, scope statefile.StateFileScope, version int, content, hash string) statefile.StateFileSnapshot {
	return statefile.StateFileSnapshot{
		StateFileID:    uuid.NewSHA1(uuid.NameSpaceOID, []byte("p819-"+key)),
		TypeKey:        key,
		Scope:          scope,
		Version:        version,
		Hash:           hash,
		Content:        content,
		Classification: statefile.ClassifySnapshotFile(key, scope),
	}
}

// snap819Files is the canonical five-section fixture (one file per WBS
// 8.13 example section). The returned list order is arbitrary
// (retrieval-like); composition must be invariant under it.
func snap819Files() []statefile.StateFileSnapshot {
	return []statefile.StateFileSnapshot{
		pin819("PROJECT_STATE", statefile.ScopeProject, 4, "project state v4", "h-ps-4"),
		pin819("PHASE_STATE", statefile.ScopePhase, 2, "phase state v2", "h-phs-2"),
		pin819("ACTIVE_TASK", statefile.ScopeWorkUnit, 7, "active task v7", "h-at-7"),
		pin819("TEST_STATE", statefile.ScopePhase, 3, "test state v3", "h-ts-3"),
	}
}

// required819 is the required type-key set for the fixture.
func required819() []string {
	return []string{"PROJECT_STATE", "PHASE_STATE", "ACTIVE_TASK", "TEST_STATE"}
}

// componentTypeForClass is the WBS 8.13 classification -> section
// component-type mapping pinned by the tests: PROJECT_CONTEXT ->
// PROJECT_CONTEXT, PHASE_CONTEXT -> PHASE_CONTEXT, TASK_CONTEXT ->
// TASK_CONTEXT, TEST_CONTEXT -> TEST_CONTEXT.
func componentTypeForClass(classification role.Classification) PromptComponentType {
	switch classification {
	case role.ClassificationProjectContext:
		return ComponentProjectContext
	case role.ClassificationPhaseContext:
		return ComponentPhaseContext
	case role.ClassificationTaskContext:
		return ComponentTaskContext
	case role.ClassificationTestContext:
		return ComponentTestContext
	}
	return PromptComponentType(classification)
}

// compsByType groups the composed components by their type.
func compsByType(comps Components) map[PromptComponentType][]PromptComponent {
	out := make(map[PromptComponentType][]PromptComponent)
	for _, c := range comps {
		out[c.Type] = append(out[c.Type], c)
	}
	return out
}

// T8.13.1 — Required files included: every snapshot pin with a captured
// version becomes exactly one component (nothing dropped, nothing
// invented).
func TestT8131_RequiredFilesIncluded(t *testing.T) {
	files := snap819Files()
	comps, err := ComposeStateFiles(files, required819())
	if err != nil {
		t.Fatalf("ComposeStateFiles: %v", err)
	}
	if len(comps) != len(files) {
		t.Fatalf("want %d components (one per captured file), got %d", len(files), len(comps))
	}
	by := compsByType(comps)
	for _, f := range files {
		typ := componentTypeForClass(f.Classification)
		if len(by[typ]) != 1 {
			t.Fatalf("section %s: want 1 component, got %d", f.Classification, len(by[typ]))
		}
		if by[typ][0].Content != f.Content {
			t.Fatalf("section %s: content %q, want %q", f.Classification, by[typ][0].Content, f.Content)
		}
		if by[typ][0].Source != SourceStateFile {
			t.Fatalf("section %s: source %s, want STATE_FILE", f.Classification, by[typ][0].Source)
		}
	}
}

// T8.13.2 — Correct versions used: each component carries the
// snapshot-CAPTURED version and hash (provenance retained verbatim); a
// later file version has no input to the composition and cannot leak in
// (repeated composition is byte-identical at the pinned version).
func TestT8132_CorrectVersionsUsed(t *testing.T) {
	files := snap819Files()
	first, err := ComposeStateFiles(files, required819())
	if err != nil {
		t.Fatalf("ComposeStateFiles: %v", err)
	}
	by := compsByType(first)
	for _, f := range files {
		c := by[componentTypeForClass(f.Classification)][0]
		if c.SourceVersion != f.Version {
			t.Fatalf("file %s: version %d, want the captured %d", f.TypeKey, c.SourceVersion, f.Version)
		}
		if c.SourceHash != f.Hash {
			t.Fatalf("file %s: hash %q, want the captured %q", f.TypeKey, c.SourceHash, f.Hash)
		}
		if c.SourceID != f.StateFileID {
			t.Fatalf("file %s: source id %s, want %s", f.TypeKey, c.SourceID, f.StateFileID)
		}
	}
	// The files are later re-edited (new versions) — irrelevant to this
	// historical composition: the snapshot pins are immutable input.
	_ = pin819("PROJECT_STATE", statefile.ScopeProject, 9, "project state v9", "h-ps-9")
	for i := 0; i < 50; i++ {
		again, err := ComposeStateFiles(files, required819())
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(again) != len(first) {
			t.Fatalf("repeat %d: length %d vs %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("repeat %d: component %d drifted: %+v vs %+v", i, j, again[j], first[j])
			}
		}
	}
}

// sectionOrder is the WBS 8.13 section order (mirroring the WBS 8.9
// canonical ranks 4 < 5 < 7 < 8): Project, Phase, Task, Test.
var sectionOrder = map[PromptComponentType]int{
	ComponentProjectContext: 4,
	ComponentPhaseContext:   5,
	ComponentTaskContext:    7,
	ComponentTestContext:    8,
}

// T8.13.3 — Classification determines section: each component lands in
// its pin's classification section (the persisted classification is the
// section's source of truth, not the live type key), and the sections
// appear in the fixed WBS 8.13 order regardless of input order.
func TestT8133_ClassificationDeterminesSection(t *testing.T) {
	files := snap819Files()
	comps, err := ComposeStateFiles(files, required819())
	if err != nil {
		t.Fatalf("ComposeStateFiles: %v", err)
	}
	if len(comps) != len(files) {
		t.Fatalf("want %d components, got %d", len(files), len(comps))
	}
	// The output must be grouped by classification section in the
	// fixed section order.
	var prevRank int
	for _, c := range comps {
		rank, ok := sectionOrder[c.Type]
		if !ok {
			t.Fatalf("component of type %s is not a WBS 8.13 section type", c.Type)
		}
		if rank < prevRank {
			t.Fatalf("section order violated: rank %d after rank %d", rank, prevRank)
		}
		prevRank = rank
		// The component's section must match its pin's persisted
		// classification.
		var pinned statefile.StateFileSnapshot
		for _, f := range files {
			if f.StateFileID == c.SourceID {
				pinned = f
				break
			}
		}
		if pinned.Classification == "" {
			t.Fatalf("component %s matched no pin", c.SourceID)
		}
		if c.Type != componentTypeForClass(pinned.Classification) {
			t.Fatalf("pin %s (class %s): component type %s", pinned.TypeKey, pinned.Classification, c.Type)
		}
	}
}

// T8.13.4 — Database retrieval order does not change prompt: the
// snapshot file list may be read back in ANY order (DB retrieval order);
// every permutation must compose to a byte-identical component set.
// The deterministic in-section ordering (type key, then file id) makes
// the output a pure function of the pin set, not of the list order.
func TestT8134_RetrievalOrderInvariance(t *testing.T) {
	// A larger fixture (two files in the same section) so in-section
	// ordering is load-bearing, not vacuous.
	files := append(snap819Files(),
		pin819("ARCHITECTURE", statefile.ScopeProject, 1, "architecture v1", "h-arch-1"),
		pin819("TASK_ARCHIVE", statefile.ScopeProject, 5, "task archive v5", "h-ta-5"),
	)
	required := append(required819(), "ARCHITECTURE", "TASK_ARCHIVE")
	first, err := ComposeStateFiles(files, required)
	if err != nil {
		t.Fatalf("ComposeStateFiles: %v", err)
	}
	// All 720 permutations of the 6-element list must be byte-identical.
	perms := 0
	var perm []int
	var rec func(n int)
	rec = func(n int) {
		if n == 0 {
			perms++
			order := make([]statefile.StateFileSnapshot, 0, len(files))
			for _, i := range perm {
				order = append(order, files[i])
			}
			comps, err := ComposeStateFiles(order, required)
			if err != nil {
				t.Fatalf("perm %d: %v", perms, err)
			}
			if len(comps) != len(first) {
				t.Fatalf("perm %d: length %d vs %d", perms, len(comps), len(first))
			}
			for j := range first {
				if comps[j] != first[j] {
					t.Fatalf("perm %d: component %d differs from canonical: %+v vs %+v", perms, j, comps[j], first[j])
				}
			}
			return
		}
		for i := 0; i < len(files); i++ {
			used := false
			for _, u := range perm {
				if u == i {
					used = true
					break
				}
			}
			if !used {
				perm = append(perm, i)
				rec(n - 1)
				perm = perm[:len(perm)-1]
			}
		}
	}
	rec(len(files))
	if perms != 720 {
		t.Fatalf("expected 720 permutations, visited %d", perms)
	}
	// Within the PROJECT_CONTEXT section the deterministic order is
	// (type key, file id): ARCHITECTURE before PROJECT_STATE before
	// TASK_ARCHIVE.
	by := compsByType(first)
	proj := by[ComponentProjectContext]
	if len(proj) != 3 {
		t.Fatalf("project section: want 3 components, got %d", len(proj))
	}
	want := []string{"architecture v1", "project state v4", "task archive v5"}
	for i := range want {
		if proj[i].Content != want[i] {
			t.Fatalf("in-section order: position %d is %q, want %q", i, proj[i].Content, want[i])
		}
	}
}

// T8.13.5 — Optional absent file handled: a pin whose type is NOT
// required and which carries no captured version composes to no
// component and no section body — no failure, no placeholder (the
// P8-18 zero-component choice). A REQUIRED file missing its captured
// version is a composition failure (ErrMissingStateFileVersion).
func TestT8135_OptionalAbsentFileHandled(t *testing.T) {
	// Optional TEST_STATE absent: the snapshot carries the pin with no
	// captured version (empty content, version 0) and the type is not
	// required.
	files := []statefile.StateFileSnapshot{
		pin819("PROJECT_STATE", statefile.ScopeProject, 4, "project state v4", "h-ps-4"),
		pin819("PHASE_STATE", statefile.ScopePhase, 2, "phase state v2", "h-phs-2"),
		pin819("ACTIVE_TASK", statefile.ScopeWorkUnit, 7, "active task v7", "h-at-7"),
		pin819("TEST_STATE", statefile.ScopePhase, 0, "", ""),
	}
	comps, err := ComposeStateFiles(files, []string{"PROJECT_STATE", "PHASE_STATE", "ACTIVE_TASK"})
	if err != nil {
		t.Fatalf("optional absent file must compose cleanly, got %v", err)
	}
	by := compsByType(comps)
	if len(by[ComponentTestContext]) != 0 {
		t.Fatalf("absent optional file must compose to no component, got %+v", by[ComponentTestContext])
	}
	if len(by[ComponentProjectContext]) != 1 || len(by[ComponentPhaseContext]) != 1 || len(by[ComponentTaskContext]) != 1 {
		t.Fatalf("present files must still compose: %+v", compsByType(comps))
	}

	// Required file with a cleared captured version: composition
	// failure (the stated P8-19 choice, mirroring P8-16/P8-17).
	bad := snap819Files()
	bad[0] = pin819("PROJECT_STATE", statefile.ScopeProject, 0, "", "")
	_, err = ComposeStateFiles(bad, required819())
	if err == nil {
		t.Fatal("required file with no captured version must fail composition")
	}
	if !errors.Is(err, ErrMissingStateFileVersion) {
		t.Fatalf("want ErrMissingStateFileVersion, got %v", err)
	}

	// Required file present: the same set composes cleanly.
	if _, err := ComposeStateFiles(snap819Files(), required819()); err != nil {
		t.Fatalf("required files present must compose cleanly, got %v", err)
	}
}

// TestT8136_RepeatedCompositionByteIdentical — determinism pin: the
// output is a pure function of the pin set; repeated composition over
// the same snapshot is byte-identical (same requirement as T8.9.6).
func TestT8136_RepeatedCompositionByteIdentical(t *testing.T) {
	files := snap819Files()
	first, err := ComposeStateFiles(files, required819())
	if err != nil {
		t.Fatalf("ComposeStateFiles: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := ComposeStateFiles(files, required819())
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(again) != len(first) {
			t.Fatalf("repeat %d: length %d vs %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("repeat %d: component %d not byte-identical", i, j)
			}
		}
	}
}
