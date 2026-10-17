package prompt

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// P8-24 — WBS 8.16 planning (PHASE_PLAN) prompt composition.
//
// The composition consumes ONLY caller-supplied configuration values (the
// P8-22/P8-23 captured-values discipline): the current WBS version, the
// planning policy, and the project granularity setting — never a
// hard-coded list — plus the work-unit kinds the P8-23 planning response
// schema renders. Repeated composition over the same input is
// byte-identical. Phase 8 includes the schema/instructions only;
// parsing/acceptance occurs later.
//
// Chosen rendering (stated for TESTER/PM to verify):
//   - T8.16.1: the current WBS version is recorded as a "WBS v<N>" line
//     (worker-visible in the content and traceable on the component's
//     SourceVersion).
//   - T8.16.2: the planning policy is rendered VERBATIM from configuration;
//     an empty policy renders an explicit "(not configured)" marker — the
//     non-atomic-project case. A policy is present for atomic projects.
//   - T8.16.3: the project granularity setting is rendered VERBATIM and
//     honored (never overridden by an implementation default).
//   - T8.16.4: the planning configuration component carries NO work-unit
//     content (id / objective / acceptance) — the planner CREATES units, it
//     does not consume a captured unit, so no work-unit field labels appear
//     (the response schema's JSON field names are placeholders, not unit
//     content, and live on the RESPONSE_PROTOCOL component, not this one).
//   - T8.16.5: the structured planning response requirements come from the
//     P8-23 ComposePlanningResponseSchema (a RESPONSE_PROTOCOL component).
//
// Failure contract (the P8-10/P8-11/P8-22 choice, stated here): a cleared
// WBS version (<= 0) is a composition FAILURE (ErrMissingWBSVersion). An
// empty policy or granularity is NOT a failure — both are project
// configuration values a project may leave unset (rendered with an explicit
// marker), mirroring the WBS 8.16 "for projects configured for atomic
// planning, include the configured policy" conditional.

var p824Kinds = []string{"atomic", "milestone", "integration"}

func p824Input() PlanningPromptInput {
	return PlanningPromptInput{
		WBSVersion:      3,
		PlanningPolicy:  "Decompose into independently testable work units.",
		Granularity:     "project-atomic",
		WorkUnitKinds:   p824Kinds,
		ProtocolID:      uuid.New(),
		ProtocolVersion: 2,
	}
}

// planningConfigComponent returns the non-schema component (the planning
// configuration) from a composed result.
func planningConfigComponent(t *testing.T, comps Components) PromptComponent {
	t.Helper()
	for _, c := range comps {
		if c.Type != ComponentResponseProtocol {
			return c
		}
	}
	t.Fatalf("no non-response-protocol component in planning composition")
	return PromptComponent{}
}

// T8.16.1 — Current WBS version included: the render records the
// caller-supplied WBS version (the "WBS v<N>" line) and a different
// version yields a different render.
func TestT8161_CurrentWBSVersionIncluded(t *testing.T) {
	in := p824Input()
	in.WBSVersion = 3
	comps, err := ComposePlanningPrompt(in)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt: %v", err)
	}
	c := planningConfigComponent(t, comps)
	if !strings.Contains(c.Content, "WBS v3") {
		t.Fatalf("planning config missing WBS v3; content:\n%s", c.Content)
	}
	if c.SourceVersion != 3 {
		t.Fatalf("planning config SourceVersion = %d, want 3", c.SourceVersion)
	}
	in2 := in
	in2.WBSVersion = 7
	comps2, err := ComposePlanningPrompt(in2)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt(v7): %v", err)
	}
	c2 := planningConfigComponent(t, comps2)
	if !strings.Contains(c2.Content, "WBS v7") {
		t.Fatalf("planning config missing WBS v7; content:\n%s", c2.Content)
	}
	if strings.Contains(c2.Content, "WBS v3") {
		t.Fatalf("stale WBS v3 present when version is 7; content:\n%s", c2.Content)
	}
}

// T8.16.2 — Planning policy included: the render carries the
// caller-supplied planning policy verbatim (an empty policy renders the
// explicit "(not configured)" marker — the non-atomic-project case).
func TestT8162_PlanningPolicyIncluded(t *testing.T) {
	in := p824Input()
	policy := "Decompose into independently testable work units."
	in.PlanningPolicy = policy
	comps, err := ComposePlanningPrompt(in)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt: %v", err)
	}
	c := planningConfigComponent(t, comps)
	if !strings.Contains(c.Content, policy) {
		t.Fatalf("planning config missing supplied policy; content:\n%s", c.Content)
	}
	// A distinct policy renders verbatim and does not carry the other.
	in2 := in
	in2.PlanningPolicy = "Prefer small, independently testable primitives."
	comps2, err := ComposePlanningPrompt(in2)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt(policy2): %v", err)
	}
	c2 := planningConfigComponent(t, comps2)
	if !strings.Contains(c2.Content, "Prefer small, independently testable primitives.") {
		t.Fatalf("planning config missing second policy; content:\n%s", c2.Content)
	}
	// Empty policy -> explicit marker, not a failure.
	in3 := in
	in3.PlanningPolicy = ""
	comps3, err := ComposePlanningPrompt(in3)
	if err != nil {
		t.Fatalf("empty policy should not fail: %v", err)
	}
	if !strings.Contains(planningConfigComponent(t, comps3).Content, "(not configured)") {
		t.Fatalf("empty policy should render (not configured); content:\n%s", planningConfigComponent(t, comps3).Content)
	}
}

// T8.16.3 — Project granularity setting honored: the render carries the
// caller-supplied granularity verbatim (honored, never overridden by an
// implementation default); a distinct value renders verbatim.
func TestT8163_ProjectGranularityHonored(t *testing.T) {
	in := p824Input()
	in.Granularity = "project-atomic"
	comps, err := ComposePlanningPrompt(in)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt: %v", err)
	}
	c := planningConfigComponent(t, comps)
	if !strings.Contains(c.Content, "project-atomic") {
		t.Fatalf("planning config missing supplied granularity; content:\n%s", c.Content)
	}
	in2 := in
	in2.Granularity = "coarse-milestone"
	comps2, err := ComposePlanningPrompt(in2)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt(gran2): %v", err)
	}
	c2 := planningConfigComponent(t, comps2)
	if !strings.Contains(c2.Content, "coarse-milestone") {
		t.Fatalf("planning config missing second granularity; content:\n%s", c2.Content)
	}
}

// T8.16.4 — Work-unit implementation content not invented by composer:
// the planning configuration component carries NO work-unit field labels
// (the planner creates units, it does not consume a captured unit). The
// input is marker-free; the scan is scoped to the configuration component
// (the response schema's JSON field names are placeholders on a separate
// component and do not count as invented unit content).
func TestT8164_NoInventedUnitContent(t *testing.T) {
	in := p824Input()
	comps, err := ComposePlanningPrompt(in)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt: %v", err)
	}
	c := planningConfigComponent(t, comps)
	for _, label := range []string{
		"ID: ",
		"objective: ",
		"acceptance criteria: ",
		"required tests: ",
		"INVENTED_UNIT",
	} {
		if strings.Contains(c.Content, label) {
			t.Fatalf("planning config invented unit content %q; content:\n%s", label, c.Content)
		}
	}
}

// T8.16.5 — Structured response requirements included: the composition
// emits a RESPONSE_PROTOCOL component built by the P8-23 planning
// response schema (required unit fields), carrying the captured protocol
// version.
func TestT8165_StructuredResponseRequirementsIncluded(t *testing.T) {
	in := p824Input()
	comps, err := ComposePlanningPrompt(in)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt: %v", err)
	}
	var proto *PromptComponent
	for i := range comps {
		if comps[i].Type == ComponentResponseProtocol {
			proto = &comps[i]
			break
		}
	}
	if proto == nil {
		t.Fatalf("no RESPONSE_PROTOCOL component in planning composition")
	}
	if proto.SourceVersion != 2 {
		t.Fatalf("response protocol SourceVersion = %d, want 2", proto.SourceVersion)
	}
	for _, field := range []string{`"id"`, `"kind"`, `"objective"`, `"dependencies"`, `"acceptance"`, `"tests"`, `"constraints"`, "required"} {
		if !strings.Contains(proto.Content, field) {
			t.Fatalf("planning response schema missing %q; content:\n%s", field, proto.Content)
		}
	}
}

// Failure contract (the P8-22 cleared-captured-value choice): a cleared
// WBS version (<= 0) is a composition FAILURE with the typed sentinel —
// no silent fallback to a default version.
func TestP824_ClearedWBSVersionIsFailure(t *testing.T) {
	for _, v := range []int{0, -1} {
		in := p824Input()
		in.WBSVersion = v
		if _, err := ComposePlanningPrompt(in); !errors.Is(err, ErrMissingWBSVersion) {
			t.Fatalf("cleared WBS version %d err = %v, want ErrMissingWBSVersion", v, err)
		}
	}
}

// Failure contract: an empty work-unit-kind capture is a composition
// FAILURE (the P8-23 planning-response-schema contract propagates through
// the planning composition — no silent hard-coded list).
func TestP824_EmptyWorkUnitKindsIsFailure(t *testing.T) {
	in := p824Input()
	in.WorkUnitKinds = nil
	if _, err := ComposePlanningPrompt(in); !errors.Is(err, ErrMissingWorkUnitKinds) {
		t.Fatalf("empty kinds err = %v, want ErrMissingWorkUnitKinds", err)
	}
}

// Determinism: repeated composition over the same input is byte-identical
// (the composition-input requirement shared with the other P8 tasks).
func TestP824_RepeatedCompositionByteIdentical(t *testing.T) {
	in := p824Input()
	first, err := ComposePlanningPrompt(in)
	if err != nil {
		t.Fatalf("ComposePlanningPrompt: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := ComposePlanningPrompt(in)
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
