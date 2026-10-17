package prompt

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// P8-26 — WBS 8.18 validation (VALIDATE_WORK_UNIT) prompt composition.
//
// The composition consumes ONLY caller-supplied captured values (the
// P8-16..P8-19 / P8-22/P8-23 captured-values discipline — no "latest"
// input): the active work unit (P8-14 ComposeWorkUnit — carries the work
// unit, its acceptance criteria, and its required tests), the captured
// test state (one TEST_CONTEXT component), the captured
// implementation status/artifacts "when available" (one additional
// TASK_CONTEXT component, absent when nothing is captured), and the
// validation response schema (P8-23
// ComposeValidationResponseSchema). Repeated composition over the same
// input is byte-identical. Phase 8 includes the schema/instructions only;
// parsing/acceptance occurs later.
//
// Chosen composition (stated for TESTER/PM to verify):
//   - T8.18.1: the unit's acceptance criteria ride verbatim (captured
//     order) on the composed work-unit component.
//   - T8.18.2: the unit's required tests ride verbatim (captured order)
//     on the composed work-unit component.
//   - T8.18.3: the captured test state is rendered as exactly one
//     TEST_CONTEXT component carrying the captured content verbatim; an
//     empty captured test state composes to ZERO components (no failure,
//     no placeholder — the "when available" choice, mirroring P8-18).
//   - T8.18.4: "implementation context correctly scoped" — the captured
//     implementation status + artifacts fields (and ONLY those two) are
//     rendered verbatim on one additional TASK_CONTEXT component with a
//     fixed scoping note: "scope: validate this work unit only; report
//     findings via the response protocol; the OVERSEER owns workflow
//     transitions." That note is the fixed boundary: the TESTER is told
//     to validate and REPORT, never to implement unrelated fixes. The
//     negative half is pinned by a bounded list of delegation phrases
//     that must be ABSENT from the implementation-context component
//     (lowercased scan). When no status and no artifacts are captured the
//     component is absent entirely (zero components, no failure).
//   - T8.18.5: the validation response schema comes from the P8-23
//     ComposeValidationResponseSchema — one RESPONSE_PROTOCOL component
//     whose content records "Response protocol vN" and whose
//     provenance carries the captured protocol id/version.
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): the
// active unit is REQUIRED — a cleared captured objective fails the
// composition (ErrMissingWorkUnitObjective, propagated from the P8-14
// work-unit composition). A cleared response-protocol version (<= 0) is a
// composition failure (ErrMissingProtocolVersion, the P8-23 contract).

// valUnitID is the fixture's FIXED active-unit id (deterministic
// composition input; a fresh uuid per construction would change the
// rendered ID).
var valUnitID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p826-unit-a"))

// valInput is the canonical validation-prompt input: the work unit with
// every WBS 8.14 field populated, a captured test state, a captured
// implementation status + artifacts, and the validation response
// schema's captured identity/version.
func valInput() ValidationPromptInput {
	return ValidationPromptInput{
		ActiveUnit: WorkUnitContextInput{
			ID:            valUnitID,
			Name:          "Build the WBS append service",
			Kind:          "ATOMIC",
			Objective:     "Provide advisory-locked WBS version appends",
			Dependencies:  []string{"dep-A: domain model"},
			Acceptance:    []string{"accept #1", "accept #2"},
			RequiredTests: []string{"test T1", "test T2"},
			Constraints:   []string{"constraint C1"},
		},
		TestState: "27 tests: 25 passed, 2 failing — test T1, test T2",
		Implementation: ImplementationStatusInput{
			Status:    "implementation reported DONE",
			Artifacts: []string{"internal/prompt/implementationprompt.go"},
		},
		ProtocolID:      valProtocolID,
		ProtocolVersion: 3,
	}
}

// valProtocolID is the fixture's FIXED protocol id (deterministic
// composition input).
var valProtocolID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p826-protocol"))

// valWorkUnitComponent returns the composed work-unit component (the one
// whose source is WORK_UNIT and whose type is TASK_CONTEXT and which
// carries the unit's objective) from a composed result.
func valWorkUnitComponent(t *testing.T, comps Components) PromptComponent {
	t.Helper()
	for _, c := range comps {
		if c.Type == ComponentTaskContext && c.Source == SourceWorkUnit &&
			strings.Contains(c.Content, "Provide advisory-locked WBS version appends") {
			return c
		}
	}
	t.Fatalf("no work-unit component in validation composition")
	return PromptComponent{}
}

// valImplComponent returns the implementation-context component (the
// TASK_CONTEXT component carrying the fixed "scope:" note), if present.
func valImplComponent(comps Components) (PromptComponent, bool) {
	for _, c := range comps {
		if c.Type == ComponentTaskContext && strings.Contains(c.Content, "scope: validate this work unit only") {
			return c, true
		}
	}
	return PromptComponent{}, false
}

// T8.18.1 — Acceptance criteria included: the work unit's acceptance
// criteria appear verbatim in the render, in captured order.
func TestT8181_AcceptanceCriteriaIncluded(t *testing.T) {
	comps, err := ComposeValidationPrompt(valInput())
	if err != nil {
		t.Fatalf("ComposeValidationPrompt: %v", err)
	}
	c := valWorkUnitComponent(t, comps)
	i1 := strings.Index(c.Content, "accept #1")
	i2 := strings.Index(c.Content, "accept #2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("acceptance criteria missing from render:\n%s", c.Content)
	}
	if i1 > i2 {
		t.Fatalf("acceptance criteria order not preserved:\n%s", c.Content)
	}
}

// T8.18.2 — Required tests included: the work unit's required tests
// appear verbatim in the render, in captured order.
func TestT8182_RequiredTestsIncluded(t *testing.T) {
	comps, err := ComposeValidationPrompt(valInput())
	if err != nil {
		t.Fatalf("ComposeValidationPrompt: %v", err)
	}
	c := valWorkUnitComponent(t, comps)
	i1 := strings.Index(c.Content, "test T1")
	i2 := strings.Index(c.Content, "test T2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("required tests missing from render:\n%s", c.Content)
	}
	if i1 > i2 {
		t.Fatalf("required tests order not preserved:\n%s", c.Content)
	}
}

// T8.18.3 — Test state included: the captured test state is rendered as
// exactly one TEST_CONTEXT component carrying the captured content
// verbatim. The ABSENT half (the "when available" pin): an empty captured
// test state composes to zero TEST_CONTEXT components and no error.
func TestT8183_TestStateIncluded(t *testing.T) {
	comps, err := ComposeValidationPrompt(valInput())
	if err != nil {
		t.Fatalf("ComposeValidationPrompt: %v", err)
	}
	var testComps []PromptComponent
	for _, c := range comps {
		if c.Type == ComponentTestContext {
			testComps = append(testComps, c)
		}
	}
	if len(testComps) != 1 {
		t.Fatalf("want exactly 1 TEST_CONTEXT component, got %d", len(testComps))
	}
	if testComps[0].Content != "27 tests: 25 passed, 2 failing — test T1, test T2" {
		t.Fatalf("test state not rendered verbatim:\n%s", testComps[0].Content)
	}
	// Absent half: empty captured test state -> zero components, no error.
	in := valInput()
	in.TestState = ""
	comps2, err := ComposeValidationPrompt(in)
	if err != nil {
		t.Fatalf("ComposeValidationPrompt(empty test state): %v", err)
	}
	for _, c := range comps2 {
		if c.Type == ComponentTestContext {
			t.Fatalf("empty test state must compose to zero components, got:\n%s", c.Content)
		}
	}
}

// T8.18.4 — Implementation context correctly scoped:
//   - the captured status + artifact fields (and only those) render
//     verbatim on the implementation-context component;
//   - the fixed scoping note ("scope: validate this work unit only;
//     report findings via the response protocol; the OVERSEER owns
//     workflow transitions.") is present — the positive half proving the
//     TESTER reports findings and the OVERSEER owns transitions;
//   - the negative half: a bounded list of delegation phrases that would
//     tell the TESTER to implement unrelated fixes must be ABSENT from
//     the implementation-context component (lowercased scan — a
//     presence assertion on the note alone cannot prove the negative);
//   - absent half: no captured status and no captured artifacts composes
//     to no implementation component (zero components, no failure).
func TestT8184_ImplementationContextCorrectlyScoped(t *testing.T) {
	comps, err := ComposeValidationPrompt(valInput())
	if err != nil {
		t.Fatalf("ComposeValidationPrompt: %v", err)
	}
	c, ok := valImplComponent(comps)
	if !ok {
		t.Fatalf("implementation-context component missing from composition")
	}
	// The captured fields render verbatim.
	if !strings.Contains(c.Content, "implementation reported DONE") {
		t.Fatalf("implementation status missing:\n%s", c.Content)
	}
	if !strings.Contains(c.Content, "internal/prompt/implementationprompt.go") {
		t.Fatalf("implementation artifact missing:\n%s", c.Content)
	}
	// Bounded absence: phrases that would delegate unrelated-fix work to
	// the TESTER must not appear.
	lower := strings.ToLower(c.Content)
	for _, phrase := range []string{
		"fix unrelated",
		"implement fixes",
		"repair other",
		"you may fix",
		"you may advance",
		"you may choose",
		"you may select",
		"advance the phase",
		"choose the next state",
	} {
		if strings.Contains(lower, phrase) {
			t.Fatalf("delegation phrase %q leaked into implementation context:\n%s", phrase, c.Content)
		}
	}
	// Absent half: nothing captured -> no implementation component.
	in := valInput()
	in.Implementation = ImplementationStatusInput{}
	comps2, err := ComposeValidationPrompt(in)
	if err != nil {
		t.Fatalf("ComposeValidationPrompt(empty implementation): %v", err)
	}
	if _, ok := valImplComponent(comps2); ok {
		t.Fatalf("empty implementation capture must compose to no component")
	}
}

// T8.18.5 — Validation response schema included: the P8-23
// ComposeValidationResponseSchema renders exactly one RESPONSE_PROTOCOL
// component; its content records the captured version ("Response
// protocol v3") and its provenance carries the captured protocol id and
// version.
func TestT8185_ValidationResponseSchemaIncluded(t *testing.T) {
	comps, err := ComposeValidationPrompt(valInput())
	if err != nil {
		t.Fatalf("ComposeValidationPrompt: %v", err)
	}
	var protoComps []PromptComponent
	for _, c := range comps {
		if c.Type == ComponentResponseProtocol {
			protoComps = append(protoComps, c)
		}
	}
	if len(protoComps) != 1 {
		t.Fatalf("want exactly 1 RESPONSE_PROTOCOL component, got %d", len(protoComps))
	}
	c := protoComps[0]
	if !strings.Contains(c.Content, "Response protocol v3") {
		t.Fatalf("render missing captured protocol version line:\n%s", c.Content)
	}
	if c.SourceID != valProtocolID {
		t.Fatalf("protocol component SourceID = %s, want %s", c.SourceID, valProtocolID)
	}
	if c.SourceVersion != 3 {
		t.Fatalf("protocol component SourceVersion = %d, want 3", c.SourceVersion)
	}
}

// Failure contract (the P8-14/P8-22 choice): the active unit is REQUIRED
// — a cleared captured objective fails the composition with the typed
// sentinel (no silent fallback).
func TestP826_MissingActiveUnitObjectiveIsFailure(t *testing.T) {
	in := valInput()
	in.ActiveUnit.Objective = ""
	if _, err := ComposeValidationPrompt(in); !errors.Is(err, ErrMissingWorkUnitObjective) {
		t.Fatalf("cleared objective err = %v, want ErrMissingWorkUnitObjective", err)
	}
}

// Failure contract: a cleared response-protocol version (<= 0) is a
// composition failure (the P8-23 contract propagated through the
// composition).
func TestP826_ClearedProtocolVersionIsFailure(t *testing.T) {
	for _, v := range []int{0, -1} {
		in := valInput()
		in.ProtocolVersion = v
		if _, err := ComposeValidationPrompt(in); !errors.Is(err, ErrMissingProtocolVersion) {
			t.Fatalf("cleared protocol version %d err = %v, want ErrMissingProtocolVersion", v, err)
		}
	}
}

// Determinism: repeated composition over the same input is byte-identical
// (the composition-input requirement shared with the other P8 tasks).
func TestP826_RepeatedCompositionByteIdentical(t *testing.T) {
	in := valInput()
	first, err := ComposeValidationPrompt(in)
	if err != nil {
		t.Fatalf("ComposeValidationPrompt: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := ComposeValidationPrompt(in)
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
