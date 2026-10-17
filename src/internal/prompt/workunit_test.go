package prompt

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// P8-20 — WBS 8.14 work-unit context, TDD T8.14.1-T8.14.6.
//
// Composition renders the CURRENT work unit's structured fields (ID,
// name, kind, objective, dependencies, acceptance criteria, required
// tests, constraints — the WBS 8.14 field set) as exactly one
// TASK_CONTEXT component, so templates can directly reference the unit's
// structured data while ACTIVE_TASK (a state file) remains the narrative
// complement. The composition consumes ONLY the captured values supplied
// by the caller at the snapshot-pinned state (the P8-16..P8-19 pattern —
// there is no "latest" input); repeated composition over the same input
// is byte-identical.
//
// In-section field order (stated per ACTIVE_TASK NOTES): the WBS 8.14
// example order verbatim — ID, name, kind, objective, dependencies,
// acceptance criteria, required tests, constraints. Dependencies are
// rendered deterministically (sorted, so ANY dependency order yields
// byte-identical output — T8.14.5); acceptance criteria and required
// tests preserve input order verbatim (T8.14.3/T8.14.4). A cleared
// captured objective is a composition failure (the P8-16/P8-17
// typed-sentinel choice); every other empty field renders its
// "(none)" placeholder (the T8.12.4 zero-content choice — no failure,
// no placeholder-free omission, no invented content).

// unit820ID is the fixture unit's FIXED id (deterministic composition
// input; a fresh uuid per construction would change the rendered ID).
var unit820ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p820-unit-a"))

// unit820 is the canonical current unit: all eight WBS 8.14 fields
// populated, with multiple entries in the list fields so ordering
// assertions are non-vacuous.
func unit820() WorkUnitContext {
	in := WorkUnitContextInput{
		ID:            unit820ID,
		Name:          "Build the WBS append service",
		Kind:          "ATOMIC",
		Objective:     "Provide advisory-locked WBS version appends",
		Dependencies:  []string{"dep-B: persistence", "dep-A: domain model"},
		Acceptance:    []string{"accept #1", "accept #2"},
		RequiredTests: []string{"test T1", "test T2"},
		Constraints:   []string{"constraint C1", "constraint C2"},
	}
	c, err := NewWorkUnitContext(in)
	if err != nil {
		panic(err)
	}
	return c
}

// T8.14.1 — Correct unit ID rendered: the composed component carries
// the unit's ID verbatim in its rendered content AND as its SourceID
// provenance; no other unit's id appears.
func TestT8141_CorrectUnitIDRendered(t *testing.T) {
	comps, err := ComposeWorkUnit(unit820())
	if err != nil {
		t.Fatalf("ComposeWorkUnit: %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("want exactly 1 TASK_CONTEXT component, got %d", len(comps))
	}
	c := comps[0]
	if c.Type != ComponentTaskContext {
		t.Fatalf("want TASK_CONTEXT, got %s", c.Type)
	}
	if c.SourceID != unit820ID {
		t.Fatalf("source id %s, want %s", c.SourceID, unit820ID)
	}
	if !strings.Contains(c.Content, unit820ID.String()) {
		t.Fatalf("rendered content does not carry the unit ID:\n%s", c.Content)
	}
}

// T8.14.2 — Objective rendered: the objective appears verbatim in the
// rendered content.
func TestT8142_ObjectiveRendered(t *testing.T) {
	comps, err := ComposeWorkUnit(unit820())
	if err != nil {
		t.Fatalf("ComposeWorkUnit: %v", err)
	}
	want := "Provide advisory-locked WBS version appends"
	if !strings.Contains(comps[0].Content, want) {
		t.Fatalf("objective not rendered:\n%s", comps[0].Content)
	}
}

// T8.14.3 — Acceptance criteria preserve order: input order is rendered
// verbatim (not re-sorted); the "(none)" case is the empty-set render.
func TestT8143_AcceptanceCriteriaPreserveOrder(t *testing.T) {
	comps, err := ComposeWorkUnit(unit820())
	if err != nil {
		t.Fatalf("ComposeWorkUnit: %v", err)
	}
	content := comps[0].Content
	i1 := strings.Index(content, "accept #1")
	i2 := strings.Index(content, "accept #2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("acceptance criteria missing from render:\n%s", content)
	}
	if i1 > i2 {
		t.Fatalf("acceptance criteria order not preserved:\n%s", content)
	}
	// A criterion whose input order is the REVERSE of sorted order
	// (numeric suffix order "10" < "2") must survive verbatim.
	in := unit820()
	in.Acceptance = []string{"earlier criterion 2", "later criterion 10"}
	comps, err = ComposeWorkUnit(in)
	if err != nil {
		t.Fatalf("ComposeWorkUnit: %v", err)
	}
	content = comps[0].Content
	a := strings.Index(content, "earlier criterion 2")
	b := strings.Index(content, "later criterion 10")
	if a < 0 || b < 0 || a > b {
		t.Fatalf("criteria input order not preserved:\n%s", content)
	}
	// Empty set renders the "(none)" placeholder (no failure).
	empty := unit820()
	empty.Acceptance = nil
	comps, err = ComposeWorkUnit(empty)
	if err != nil {
		t.Fatalf("ComposeWorkUnit(empty acceptance): %v", err)
	}
	if !strings.Contains(comps[0].Content, "(none)") {
		t.Fatalf("empty acceptance must render (none):\n%s", comps[0].Content)
	}
}

// T8.14.4 — Required tests preserve order: same contract as
// T8.14.3 for the required-tests field.
func TestT8144_RequiredTestsPreserveOrder(t *testing.T) {
	comps, err := ComposeWorkUnit(unit820())
	if err != nil {
		t.Fatalf("ComposeWorkUnit: %v", err)
	}
	content := comps[0].Content
	i1 := strings.Index(content, "test T1")
	i2 := strings.Index(content, "test T2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("required tests missing from render:\n%s", content)
	}
	if i1 > i2 {
		t.Fatalf("required tests order not preserved:\n%s", content)
	}
	// Reverse-sorted input order must survive verbatim.
	in := unit820()
	in.RequiredTests = []string{"aa-test", "zz-test"}
	comps, err = ComposeWorkUnit(in)
	if err != nil {
		t.Fatalf("ComposeWorkUnit: %v", err)
	}
	content = comps[0].Content
	a := strings.Index(content, "aa-test")
	b := strings.Index(content, "zz-test")
	if a < 0 || b < 0 || a > b {
		t.Fatalf("required tests input order not preserved:\n%s", content)
	}
}

// T8.14.5 — Dependencies rendered deterministically: the same dependency
// SET in ANY input order composes to byte-identical output; empty
// dependencies render the "(none)" placeholder.
func TestT8145_DependenciesRenderedDeterministically(t *testing.T) {
	a := unit820()
	a.Dependencies = []string{"dep-A: domain model", "dep-B: persistence", "dep-C: tests"}
	b := unit820()
	b.Dependencies = []string{"dep-C: tests", "dep-A: domain model", "dep-B: persistence"}
	ca, err := ComposeWorkUnit(a)
	if err != nil {
		t.Fatalf("ComposeWorkUnit(a): %v", err)
	}
	cb, err := ComposeWorkUnit(b)
	if err != nil {
		t.Fatalf("ComposeWorkUnit(b): %v", err)
	}
	if len(ca) != 1 || len(cb) != 1 {
		t.Fatalf("want 1 component per composition, got %d/%d", len(ca), len(cb))
	}
	if ca[0].Content != cb[0].Content {
		t.Fatalf("dependency order must not change the render:\n--- a ---\n%s\n--- b ---\n%s", ca[0].Content, cb[0].Content)
	}
	for _, dep := range []string{"dep-A: domain model", "dep-B: persistence", "dep-C: tests"} {
		if !strings.Contains(ca[0].Content, dep) {
			t.Fatalf("dependency %q missing from render:\n%s", dep, ca[0].Content)
		}
	}
	// Sorted render order: dep-A before dep-B before dep-C.
	ca_ := strings.Index(ca[0].Content, "dep-A: domain model")
	cb_ := strings.Index(ca[0].Content, "dep-B: persistence")
	cc := strings.Index(ca[0].Content, "dep-C: tests")
	if ca_ > cb_ || cb_ > cc {
		t.Fatalf("dependencies not rendered in deterministic sorted order:\n%s", ca[0].Content)
	}
	// No dependencies: the placeholder, no failure.
	none := unit820()
	none.Dependencies = nil
	comps, err := ComposeWorkUnit(none)
	if err != nil {
		t.Fatalf("ComposeWorkUnit(no deps): %v", err)
	}
	if !strings.Contains(comps[0].Content, "(none)") {
		t.Fatalf("empty dependencies must render (none):\n%s", comps[0].Content)
	}
}

// T8.14.6 — No unrelated work unit included: only the composed unit's
// identity/content appears; a sibling unit's id, name, and objective are
// all absent from the render (and the component's provenance pins the
// composed unit alone).
func TestT8146_NoUnrelatedWorkUnitIncluded(t *testing.T) {
	other := WorkUnitContextInput{
		ID:        uuid.NewSHA1(uuid.NameSpaceOID, []byte("p820-unit-b")),
		Name:      "An unrelated unit",
		Kind:      "VALIDATION",
		Objective: "Validate the unrelated objective",
	}
	comps, err := ComposeWorkUnit(unit820())
	if err != nil {
		t.Fatalf("ComposeWorkUnit: %v", err)
	}
	content := comps[0].Content
	if strings.Contains(content, other.ID.String()) {
		t.Fatalf("unrelated unit ID leaked:\n%s", content)
	}
	if strings.Contains(content, other.Name) {
		t.Fatalf("unrelated unit name leaked:\n%s", content)
	}
	if strings.Contains(content, other.Objective) {
		t.Fatalf("unrelated unit objective leaked:\n%s", content)
	}
	if !strings.Contains(content, unit820().Name) {
		t.Fatalf("the composed unit's own name must be present:\n%s", content)
	}
	if comps[0].SourceID != unit820ID {
		t.Fatalf("provenance must pin the composed unit: %s", comps[0].SourceID)
	}
}

// Missing captured objective: the P8-16/P8-17 typed-sentinel failure
// contract (a cleared captured REQUIRED value fails the composition).
func TestT8147_MissingCapturedObjectiveFails(t *testing.T) {
	bad := unit820()
	bad.Objective = ""
	_, err := ComposeWorkUnit(bad)
	if err == nil {
		t.Fatal("cleared captured objective must fail the composition")
	}
	if !errors.Is(err, ErrMissingWorkUnitObjective) {
		t.Fatalf("want ErrMissingWorkUnitObjective, got %v", err)
	}
	// Whitespace-only is cleared too.
	bad.Objective = "   "
	_, err = ComposeWorkUnit(bad)
	if !errors.Is(err, ErrMissingWorkUnitObjective) {
		t.Fatalf("want ErrMissingWorkUnitObjective for whitespace objective, got %v", err)
	}
}

// Determinism pin: repeated composition over the same input is
// byte-identical (same composition-input requirement as T8.9.6).
func TestT8148_RepeatedCompositionByteIdentical(t *testing.T) {
	first, err := ComposeWorkUnit(unit820())
	if err != nil {
		t.Fatalf("ComposeWorkUnit: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := ComposeWorkUnit(unit820())
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
