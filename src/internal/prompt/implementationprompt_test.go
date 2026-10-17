package prompt

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// P8-25 — WBS 8.17 implementation (IMPLEMENT_WORK_UNIT) prompt composition.
//
// The composition consumes ONLY caller-supplied captured values (the
// P8-16..P8-19 / P8-22/P8-23 captured-values discipline — no "latest"
// input): the active work unit (P8-14 ComposeWorkUnit), the active unit's
// dependency contracts (P8-15 ComposeDependencyContracts), and the
// implementation response schema (P8-23
// ComposeImplementationResponseSchema). Repeated composition over the same
// input is byte-identical. Phase 8 includes the schema/instructions only;
// parsing/acceptance occurs later.
//
// Chosen composition (stated for TESTER/PM to verify):
//   - T8.17.1: the active unit is composed via ComposeWorkUnit — its ID,
//     objective, and structured fields appear in the render and the
//     component's SourceID provenance pins the active unit.
//   - T8.17.2: the active unit's acceptance criteria are rendered verbatim
//     (they ride on the composed work-unit component).
//   - T8.17.3: the active unit's required tests are rendered verbatim.
//   - T8.17.4: the active unit's dependency contracts are composed via
//     ComposeDependencyContracts (only the contracts the unit references).
//   - T8.17.5: a future work unit (not the active unit and not an explicit
//     dependency contract) is EXCLUDED from the render; a contract the unit
//     explicitly references IS included (the "unless explicitly needed"
//     half).
//   - T8.17.6: the render carries an explicit "completion target:" line
//     naming the active unit, so a worker can identify the exact
//     completion target (the active unit + its acceptance criteria + its
//     required tests) without unrelated conversation history.
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): the active
// unit is REQUIRED — a cleared captured objective fails the composition
// (ErrMissingWorkUnitObjective, propagated from the P8-14 work-unit
// composition). A cleared response-protocol version (<= 0) and an empty
// allowed-status capture are composition failures (ErrMissingProtocolVersion
// / ErrMissingAllowedStatuses, the P8-23 contract).

// implUnitID is the fixture's FIXED active-unit id (deterministic
// composition input; a fresh uuid per construction would change the
// rendered ID).
var implUnitID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p825-unit-a"))

// implInput is the canonical implementation-prompt input: the active unit
// with every WBS 8.14 field populated, its two dependency contracts (both
// referenced by the unit's dependency set), and the implementation response
// schema's captured identity/version + allowed statuses.
func implInput() ImplementationPromptInput {
	return ImplementationPromptInput{
		ActiveUnit: WorkUnitContextInput{
			ID:            implUnitID,
			Name:          "Build the WBS append service",
			Kind:          "ATOMIC",
			Objective:     "Provide advisory-locked WBS version appends",
			Dependencies:  []string{"dep-A: domain model", "dep-B: persistence"},
			Acceptance:    []string{"accept #1", "accept #2"},
			RequiredTests: []string{"test T1", "test T2"},
			Constraints:   []string{"constraint C1"},
		},
		DependencyContracts: []DependencyContract{
			{Name: "dep-A: domain model", Purpose: "domain type for WBS", Input: "version content", Output: "WBSVersion", Guarantees: []string{"no reuse of deleted numbers"}},
			{Name: "dep-B: persistence", Purpose: "append repo", Input: "WBSVersion", Output: "persisted row", Guarantees: []string{"advisory-locked MAX+1"}},
		},
		ProtocolID:      uuid.New(),
		ProtocolVersion: 3,
		AllowedStatuses: []string{"DONE", "INCOMPLETE", "BLOCKED"},
	}
}

// implUnitComponent returns the composed work-unit component (the one whose
// provenance is the WORK_UNIT source) from a composed result.
func implUnitComponent(t *testing.T, comps Components) PromptComponent {
	t.Helper()
	for _, c := range comps {
		if c.Source == SourceWorkUnit {
			return c
		}
	}
	t.Fatalf("no work-unit component in implementation composition")
	return PromptComponent{}
}

// implFullRender returns the concatenation of every component's content, so
// exclusion/presence assertions scan the whole render (not just the
// work-unit component).
func implFullRender(t *testing.T, comps Components) string {
	t.Helper()
	var b strings.Builder
	for _, c := range comps {
		b.WriteString(c.Content)
		b.WriteByte(0) // unit separator between components
	}
	return b.String()
}

// T8.17.1 — Correct active unit included: the composed work-unit component
// carries the active unit's ID verbatim in its content AND as its SourceID
// provenance, and carries the unit's objective; a different active unit
// yields a different render.
func TestT8171_CorrectActiveUnitIncluded(t *testing.T) {
	comps, err := ComposeImplementationPrompt(implInput())
	if err != nil {
		t.Fatalf("ComposeImplementationPrompt: %v", err)
	}
	c := implUnitComponent(t, comps)
	if c.SourceID != implUnitID {
		t.Fatalf("work-unit component SourceID = %s, want %s", c.SourceID, implUnitID)
	}
	if !strings.Contains(c.Content, implUnitID.String()) {
		t.Fatalf("render missing active unit ID; content:\n%s", c.Content)
	}
	if !strings.Contains(c.Content, "Provide advisory-locked WBS version appends") {
		t.Fatalf("render missing active unit objective; content:\n%s", c.Content)
	}
	// A distinct active unit renders with that unit's id, not the other's.
	in := implInput()
	in.ActiveUnit.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p825-unit-other"))
	comps2, err := ComposeImplementationPrompt(in)
	if err != nil {
		t.Fatalf("ComposeImplementationPrompt(other): %v", err)
	}
	c2 := implUnitComponent(t, comps2)
	if !strings.Contains(c2.Content, in.ActiveUnit.ID.String()) {
		t.Fatalf("render missing other unit ID; content:\n%s", c2.Content)
	}
	if strings.Contains(c2.Content, implUnitID.String()) {
		t.Fatalf("stale active unit ID present; content:\n%s", c2.Content)
	}
}

// T8.17.2 — Acceptance criteria included: the active unit's acceptance
// criteria appear verbatim in the render (the WBS 8.17 "acceptance
// criteria" section), in captured order.
func TestT8172_AcceptanceCriteriaIncluded(t *testing.T) {
	comps, err := ComposeImplementationPrompt(implInput())
	if err != nil {
		t.Fatalf("ComposeImplementationPrompt: %v", err)
	}
	c := implUnitComponent(t, comps)
	i1 := strings.Index(c.Content, "accept #1")
	i2 := strings.Index(c.Content, "accept #2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("acceptance criteria missing from render:\n%s", c.Content)
	}
	if i1 > i2 {
		t.Fatalf("acceptance criteria order not preserved:\n%s", c.Content)
	}
}

// T8.17.3 — Tests included: the active unit's required tests appear
// verbatim in the render (the WBS 8.17 "required tests" section), in
// captured order.
func TestT8173_RequiredTestsIncluded(t *testing.T) {
	comps, err := ComposeImplementationPrompt(implInput())
	if err != nil {
		t.Fatalf("ComposeImplementationPrompt: %v", err)
	}
	c := implUnitComponent(t, comps)
	i1 := strings.Index(c.Content, "test T1")
	i2 := strings.Index(c.Content, "test T2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("required tests missing from render:\n%s", c.Content)
	}
	if i1 > i2 {
		t.Fatalf("required tests order not preserved:\n%s", c.Content)
	}
}

// T8.17.4 — Dependencies included: the active unit's dependency contracts
// are composed (each referenced contract appears as a DEPENDENCY_CONTRACT
// component carrying its summary); the unit's own dependency list also
// appears on the work-unit component.
func TestT8174_DependenciesIncluded(t *testing.T) {
	comps, err := ComposeImplementationPrompt(implInput())
	if err != nil {
		t.Fatalf("ComposeImplementationPrompt: %v", err)
	}
	// The unit's own dependency list rides on the work-unit component.
	uc := implUnitComponent(t, comps)
	for _, d := range []string{"dep-A: domain model", "dep-B: persistence"} {
		if !strings.Contains(uc.Content, d) {
			t.Fatalf("work-unit component missing dependency %q:\n%s", d, uc.Content)
		}
	}
	// Each referenced contract is composed as a dependency-contract component.
	var contractCount int
	for _, c := range comps {
		if c.Source != SourceDependencyContract {
			continue
		}
		contractCount++
		if !strings.Contains(c.Content, "Purpose: ") {
			t.Fatalf("dependency-contract component missing purpose summary:\n%s", c.Content)
		}
	}
	if contractCount != 2 {
		t.Fatalf("want 2 dependency-contract components (both referenced), got %d", contractCount)
	}
}

// T8.17.5 — Future work units excluded unless explicitly needed: a future
// unit (not the active unit, not an explicit dependency contract) is absent
// from the render, while a contract the unit explicitly references IS
// present (the "explicitly needed" half).
func TestT8175_FutureWorkUnitsExcluded(t *testing.T) {
	future := WorkUnitContextInput{
		ID:        uuid.NewSHA1(uuid.NameSpaceOID, []byte("p825-unit-future")),
		Name:      "A future unit",
		Kind:      "ATOMIC",
		Objective: "The future unit's objective",
	}
	// A contract NOT referenced by the active unit is also excluded.
	unrelated := DependencyContract{
		Name:    "dep-C: unrelated future work",
		Purpose: "an unrelated dependency",
	}
	in := implInput()
	// The unrelated contract is supplied but the unit does not reference it.
	in.DependencyContracts = append(in.DependencyContracts, unrelated)
	comps, err := ComposeImplementationPrompt(in)
	if err != nil {
		t.Fatalf("ComposeImplementationPrompt: %v", err)
	}
	render := implFullRender(t, comps)
	if strings.Contains(render, future.ID.String()) {
		t.Fatalf("future unit ID leaked into render:\n%s", render)
	}
	if strings.Contains(render, future.Name) {
		t.Fatalf("future unit name leaked into render:\n%s", render)
	}
	if strings.Contains(render, future.Objective) {
		t.Fatalf("future unit objective leaked into render:\n%s", render)
	}
	// The unrelated (non-referenced) contract is excluded.
	if strings.Contains(render, "dep-C: unrelated future work") {
		t.Fatalf("unreferenced contract leaked into render:\n%s", render)
	}
	// The explicitly-referenced contracts remain (the "explicitly needed"
	// half).
	for _, d := range []string{"dep-A: domain model", "dep-B: persistence"} {
		if !strings.Contains(render, d) {
			t.Fatalf("referenced dependency %q missing from render:\n%s", d, render)
		}
	}
}

// T8.17.6 — Worker can identify exact completion target from prompt: the
// render carries an explicit "completion target:" line naming the active
// unit, and the target is defined by the unit's objective + acceptance
// criteria + required tests (all present in the render), so a worker can
// identify the exact completion target without unrelated conversation
// history. The line names the ACTIVE unit, not a future one.
func TestT8176_ExactCompletionTargetIdentifiable(t *testing.T) {
	comps, err := ComposeImplementationPrompt(implInput())
	if err != nil {
		t.Fatalf("ComposeImplementationPrompt: %v", err)
	}
	render := implFullRender(t, comps)
	i := strings.Index(render, "completion target:")
	if i < 0 {
		t.Fatalf("render missing completion-target line:\n%s", render)
	}
	targetLine := render[i:]
	// The completion target names the active unit (identifiable).
	if !strings.Contains(targetLine, implUnitID.String()) {
		t.Fatalf("completion target does not name the active unit:\n%s", render)
	}
	// The target's "done" definition elements are all present in the render.
	for _, marker := range []string{
		"Provide advisory-locked WBS version appends", // objective
		"accept #1", "accept #2", // acceptance criteria
		"test T1", "test T2", // required tests
	} {
		if !strings.Contains(render, marker) {
			t.Fatalf("completion target not fully identifiable — missing %q:\n%s", marker, render)
		}
	}
}

// Failure contract (the P8-14/P8-22 choice): the active unit is REQUIRED —
// a cleared captured objective fails the composition with the typed
// sentinel (no silent fallback).
func TestP825_MissingActiveUnitObjectiveIsFailure(t *testing.T) {
	in := implInput()
	in.ActiveUnit.Objective = ""
	if _, err := ComposeImplementationPrompt(in); !errors.Is(err, ErrMissingWorkUnitObjective) {
		t.Fatalf("cleared objective err = %v, want ErrMissingWorkUnitObjective", err)
	}
}

// Failure contract: a cleared response-protocol version (<= 0) is a
// composition failure (the P8-23 contract propagated through the
// composition).
func TestP825_ClearedProtocolVersionIsFailure(t *testing.T) {
	for _, v := range []int{0, -1} {
		in := implInput()
		in.ProtocolVersion = v
		if _, err := ComposeImplementationPrompt(in); !errors.Is(err, ErrMissingProtocolVersion) {
			t.Fatalf("cleared protocol version %d err = %v, want ErrMissingProtocolVersion", v, err)
		}
	}
}

// Failure contract: an empty allowed-status capture is a composition
// failure (the P8-23 implementation-response-schema contract).
func TestP825_EmptyAllowedStatusesIsFailure(t *testing.T) {
	in := implInput()
	in.AllowedStatuses = nil
	if _, err := ComposeImplementationPrompt(in); !errors.Is(err, ErrMissingAllowedStatuses) {
		t.Fatalf("empty statuses err = %v, want ErrMissingAllowedStatuses", err)
	}
}

// Determinism: repeated composition over the same input is byte-identical
// (the composition-input requirement shared with the other P8 tasks).
func TestP825_RepeatedCompositionByteIdentical(t *testing.T) {
	in := implInput()
	first, err := ComposeImplementationPrompt(in)
	if err != nil {
		t.Fatalf("ComposeImplementationPrompt: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := ComposeImplementationPrompt(in)
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(got) != len(first) {
			t.Fatalf("repeat %d: component count %d, want %d", i, len(got), len(first))
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("repeat %d: component %d not byte-identical", i, j)
			}
		}
	}
}
