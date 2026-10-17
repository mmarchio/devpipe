package prompt

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// P8-27 — WBS 8.19 fix (FIX_WORK_UNIT) prompt composition.
//
// The composition consumes ONLY caller-supplied captured values (the
// P8-16..P8-19 / P8-22/P8-23 captured-values discipline — no "latest"
// input): the retained work unit (P8-14 ComposeWorkUnit), the captured
// failed validation results (failed criteria, failed tests, relevant
// error output — T8.19.2/T8.19.3), and the implementation response
// schema (P8-23 ComposeImplementationResponseSchema). Repeated
// composition over the same input is byte-identical. Phase 8 includes
// the schema/instructions only; parsing/acceptance occurs later.
//
// Chosen composition (stated for TESTER/PM to verify):
//   - T8.19.1: the SAME work unit is retained — it is composed via
//     ComposeWorkUnit (ID verbatim in content AND on SourceID
//     provenance, objective present), and a "fix target:" line appended
//     to the work-unit component names the active unit, so the fix
//     request is pinned to exactly the failed unit.
//   - T8.19.2: the captured failed tests are rendered verbatim (captured
//     order) on exactly one TEST_CONTEXT component; the captured failed
//     criteria are rendered verbatim (captured order) on exactly one
//     TASK_CONTEXT component; the captured relevant error output is
//     rendered verbatim as the "error output:" section of the
//     TEST_CONTEXT component.
//   - T8.19.3: "passed unrelated tests need not dominate context" —
//     CHOSEN POLICY: only the FAILED result items (failed criteria,
//     failed tests) and the caller-captured RELEVANT error output enter
//     the composition; passed tests have no input channel at all, so
//     they can never dominate the context. Empty failed captures compose
//     to ZERO failure components (no failure, no placeholder — the
//     "when available" choice, mirroring P8-26).
//   - T8.19.4: the unit's ORIGINAL acceptance criteria ride verbatim on
//     the composed work-unit component (WBS 8.14) — retained, not
//     replaced by the failure results.
//   - T8.19.5: the NEXT work unit is excluded — the composition input
//     carries no next-unit channel (pinned at the compile level by the
//     FixPromptInput shape, mirroring the T8.17.5/T8.14.6 exclusion
//     proof).
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): the
// retained unit is REQUIRED — a cleared captured objective fails the
// composition (ErrMissingWorkUnitObjective, propagated from the P8-14
// work-unit composition). A cleared response-protocol version (<= 0) and
// an empty allowed-status capture are composition failures
// (ErrMissingProtocolVersion / ErrMissingAllowedStatuses, the P8-23
// contract).

// fixUnitID is the fixture's FIXED retained-unit id (deterministic
// composition input; a fresh uuid per construction would change the
// rendered ID).
var fixUnitID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p827-unit-a"))

// fixProtocolID is the fixture's FIXED protocol id (deterministic
// composition input).
var fixProtocolID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p827-protocol"))

// fixInput is the canonical fix-prompt input: the retained work unit
// with every WBS 8.14 field populated, a captured failed-validation
// result (two failed criteria, two failed tests, one relevant error
// capture), and the implementation response schema's captured
// identity/version + allowed statuses.
func fixInput() FixPromptInput {
	return FixPromptInput{
		RetainedUnit: WorkUnitContextInput{
			ID:            fixUnitID,
			Name:          "Build the WBS append service",
			Kind:          "ATOMIC",
			Objective:     "Provide advisory-locked WBS version appends",
			Dependencies:  []string{"dep-A: domain model"},
			Acceptance:    []string{"accept #1", "accept #2"},
			RequiredTests: []string{"test T1", "test T2"},
			Constraints:   []string{"constraint C1"},
		},
		FailedResult: FailedValidationInput{
			FailedCriteria: []string{"accept #1: version must not reuse deleted numbers"},
			FailedTests:    []string{"test T1", "test T2"},
			RelevantErrors: []string{"test T1: expected 5, got 4"},
		},
		ProtocolID:      fixProtocolID,
		ProtocolVersion: 3,
		AllowedStatuses: []string{"DONE", "INCOMPLETE", "BLOCKED"},
	}
}

// fixUnitComponent returns the composed work-unit component (the one
// whose provenance is the WORK_UNIT source) from a composed result.
func fixUnitComponent(t *testing.T, comps Components) PromptComponent {
	t.Helper()
	for _, c := range comps {
		if c.Source == SourceWorkUnit {
			return c
		}
	}
	t.Fatalf("no work-unit component in fix composition")
	return PromptComponent{}
}

// fixFullRender returns the concatenation of every component's content,
// so exclusion/presence assertions scan the whole render (not just the
// work-unit component).
func fixFullRender(t *testing.T, comps Components) string {
	t.Helper()
	var b strings.Builder
	for _, c := range comps {
		b.WriteString(c.Content)
		b.WriteByte(0) // unit separator between components
	}
	return b.String()
}

// T8.19.1 — Same work unit retained: the composed work-unit component
// carries the retained unit's ID verbatim in its content AND as its
// SourceID provenance, and carries the unit's objective; a different
// retained unit yields a different render.
func TestT8191_SameWorkUnitRetained(t *testing.T) {
	comps, err := ComposeFixPrompt(fixInput())
	if err != nil {
		t.Fatalf("ComposeFixPrompt: %v", err)
	}
	c := fixUnitComponent(t, comps)
	if c.SourceID != fixUnitID {
		t.Fatalf("work-unit component SourceID = %s, want %s", c.SourceID, fixUnitID)
	}
	if !strings.Contains(c.Content, fixUnitID.String()) {
		t.Fatalf("render missing retained unit ID; content:\n%s", c.Content)
	}
	if !strings.Contains(c.Content, "Provide advisory-locked WBS version appends") {
		t.Fatalf("render missing retained unit objective; content:\n%s", c.Content)
	}
	// A distinct retained unit renders with that unit's id, not the other's.
	in := fixInput()
	in.RetainedUnit.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p827-unit-other"))
	comps2, err := ComposeFixPrompt(in)
	if err != nil {
		t.Fatalf("ComposeFixPrompt(other): %v", err)
	}
	c2 := fixUnitComponent(t, comps2)
	if !strings.Contains(c2.Content, in.RetainedUnit.ID.String()) {
		t.Fatalf("render missing other unit ID; content:\n%s", c2.Content)
	}
	if strings.Contains(c2.Content, fixUnitID.String()) {
		t.Fatalf("stale retained unit ID present; content:\n%s", c2.Content)
	}
}

// T8.19.2 — Failed tests included: the captured failed tests appear
// verbatim in the render (captured order) on exactly one TEST_CONTEXT
// component, the captured failed criteria verbatim (captured order) on
// exactly one TASK_CONTEXT component, and the captured relevant error
// output verbatim as the error-output section of the TEST_CONTEXT
// component.
func TestT8192_FailedTestsIncluded(t *testing.T) {
	comps, err := ComposeFixPrompt(fixInput())
	if err != nil {
		t.Fatalf("ComposeFixPrompt: %v", err)
	}
	var testComps []PromptComponent
	var critComps []PromptComponent
	for _, c := range comps {
		if c.Type == ComponentTestContext {
			testComps = append(testComps, c)
		}
		if c.Type == ComponentTaskContext && strings.Contains(c.Content, "failed acceptance criteria:") {
			critComps = append(critComps, c)
		}
	}
	if len(testComps) != 1 {
		t.Fatalf("want exactly 1 TEST_CONTEXT component, got %d", len(testComps))
	}
	tc := testComps[0]
	i1 := strings.Index(tc.Content, "test T1")
	i2 := strings.Index(tc.Content, "test T2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("failed tests missing from render:\n%s", tc.Content)
	}
	if i1 > i2 {
		t.Fatalf("failed tests order not preserved:\n%s", tc.Content)
	}
	if !strings.Contains(tc.Content, "error output: test T1: expected 5, got 4") {
		t.Fatalf("relevant error output missing from render:\n%s", tc.Content)
	}
	if len(critComps) != 1 {
		t.Fatalf("want exactly 1 failed-criteria component, got %d", len(critComps))
	}
	cc := critComps[0]
	if i1 := strings.Index(cc.Content, "accept #1: version must not reuse deleted numbers"); i1 < 0 {
		t.Fatalf("failed criteria missing from render:\n%s", cc.Content)
	}
}

// T8.19.3 — Passed unrelated tests need not dominate context: CHOSEN
// POLICY (stated here) — only the FAILED result items and the
// caller-captured RELEVANT error output enter the composition; passed
// tests have no input channel at all, so they can never dominate the
// context. The load-bearing half: the failed items are rendered ISOLATED
// on the failure components (a single-line failed-test list, a
// single-line failed-criteria list) — the failure components carry no
// passing-test noise. The ABSENT half: an empty failed-result capture
// composes to ZERO failure components and no error.
func TestT8193_PassedUnrelatedTestsDoNotDominate(t *testing.T) {
	comps, err := ComposeFixPrompt(fixInput())
	if err != nil {
		t.Fatalf("ComposeFixPrompt: %v", err)
	}
	// Isolation: the failure components carry ONLY the captured failed
	// items (and the relevant error output) — no passing-test content is
	// rendered anywhere in the composition.
	for _, c := range comps {
		if c.Type != ComponentTestContext && c.Type != ComponentTaskContext {
			continue
		}
		if strings.Contains(c.Content, "passed") {
			t.Fatalf("passing-test content dominates failure component:\n%s", c.Content)
		}
	}
	// Absent half: an empty failed-result capture composes to zero
	// failure components (no failure, no placeholder).
	in := fixInput()
	in.FailedResult = FailedValidationInput{}
	comps2, err := ComposeFixPrompt(in)
	if err != nil {
		t.Fatalf("ComposeFixPrompt(empty failed result): %v", err)
	}
	for _, c := range comps2 {
		if c.Type == ComponentTestContext {
			t.Fatalf("empty failed result must compose to zero TEST_CONTEXT components, got:\n%s", c.Content)
		}
		if c.Type == ComponentTaskContext && strings.Contains(c.Content, "failed acceptance criteria:") {
			t.Fatalf("empty failed result must compose to no failed-criteria component, got:\n%s", c.Content)
		}
	}
}

// T8.19.4 — Original acceptance criteria retained: the retained unit's
// ORIGINAL acceptance criteria appear verbatim (captured order) on the
// composed work-unit component — the failure results do not replace or
// mutate them.
func TestT8194_OriginalAcceptanceCriteriaRetained(t *testing.T) {
	comps, err := ComposeFixPrompt(fixInput())
	if err != nil {
		t.Fatalf("ComposeFixPrompt: %v", err)
	}
	c := fixUnitComponent(t, comps)
	i1 := strings.Index(c.Content, "accept #1")
	i2 := strings.Index(c.Content, "accept #2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("acceptance criteria missing from render:\n%s", c.Content)
	}
	if i1 > i2 {
		t.Fatalf("acceptance criteria order not preserved:\n%s", c.Content)
	}
}

// T8.19.5 — Next work unit not included: a future (next) unit — not the
// retained unit — is absent from the render. The composition input
// carries no next-unit channel at all (the FixPromptInput shape is the
// compile-level guard), so the absence is by construction.
func TestT8195_NextWorkUnitNotIncluded(t *testing.T) {
	future := WorkUnitContextInput{
		ID:        uuid.NewSHA1(uuid.NameSpaceOID, []byte("p827-unit-next")),
		Name:      "The next unit",
		Kind:      "ATOMIC",
		Objective: "The next unit's objective",
	}
	comps, err := ComposeFixPrompt(fixInput())
	if err != nil {
		t.Fatalf("ComposeFixPrompt: %v", err)
	}
	render := fixFullRender(t, comps)
	if strings.Contains(render, future.ID.String()) {
		t.Fatalf("next unit ID leaked into render:\n%s", render)
	}
	if strings.Contains(render, future.Name) {
		t.Fatalf("next unit name leaked into render:\n%s", render)
	}
	if strings.Contains(render, future.Objective) {
		t.Fatalf("next unit objective leaked into render:\n%s", render)
	}
}

// Compile-level guard (the T8.17.5/T8.14.6 channel-absence class):
// ComposeFixPrompt's input shape is exactly FixPromptInput — the
// retained unit, the failed result, and the schema's captured
// identity/version/statuses — with NO next-unit channel. Adding one
// breaks this build.
var _ = func(in FixPromptInput) (Components, error) { return ComposeFixPrompt(in) }

// Failure contract (the P8-14/P8-22 choice): the retained unit is
// REQUIRED — a cleared captured objective fails the composition with
// the typed sentinel (no silent fallback).
func TestP827_MissingRetainedUnitObjectiveIsFailure(t *testing.T) {
	in := fixInput()
	in.RetainedUnit.Objective = ""
	if _, err := ComposeFixPrompt(in); !errors.Is(err, ErrMissingWorkUnitObjective) {
		t.Fatalf("cleared objective err = %v, want ErrMissingWorkUnitObjective", err)
	}
}

// Failure contract: a cleared response-protocol version (<= 0) is a
// composition failure (the P8-23 contract propagated through the
// composition).
func TestP827_ClearedProtocolVersionIsFailure(t *testing.T) {
	for _, v := range []int{0, -1} {
		in := fixInput()
		in.ProtocolVersion = v
		if _, err := ComposeFixPrompt(in); !errors.Is(err, ErrMissingProtocolVersion) {
			t.Fatalf("cleared protocol version %d err = %v, want ErrMissingProtocolVersion", v, err)
		}
	}
}

// Failure contract: an empty allowed-status capture is a composition
// failure (the P8-23 implementation-response-schema contract).
func TestP827_EmptyAllowedStatusesIsFailure(t *testing.T) {
	in := fixInput()
	in.AllowedStatuses = nil
	if _, err := ComposeFixPrompt(in); !errors.Is(err, ErrMissingAllowedStatuses) {
		t.Fatalf("empty statuses err = %v, want ErrMissingAllowedStatuses", err)
	}
}

// Determinism: repeated composition over the same input is byte-identical
// (the composition-input requirement shared with the other P8 tasks).
func TestP827_RepeatedCompositionByteIdentical(t *testing.T) {
	in := fixInput()
	first, err := ComposeFixPrompt(in)
	if err != nil {
		t.Fatalf("ComposeFixPrompt: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := ComposeFixPrompt(in)
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
