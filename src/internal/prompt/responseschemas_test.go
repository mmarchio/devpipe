package prompt

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// P8-23 — WBS 8.41/8.42/8.43 per-state response schema composition.
//
// Each composition renders its state's required response schema as
// exactly one RESPONSE_PROTOCOL component, consuming ONLY the
// caller-supplied configuration values (the P8-22 captured-values
// discipline: work-unit kinds and allowed statuses come in from the
// caller, never from a hard-coded list) and the captured protocol
// version (recorded in the content and on the component's provenance).
// Phase 8 includes the schema/instructions only — parsing/acceptance
// occurs later. Repeated composition over the same input is
// byte-identical.
//
// T8.42.5 (no next-unit selection): the implementation render supports
// the WBS 8.42 "remaining"/"next_action" FIELDS, but must carry no
// directive that tells the worker to select/choose the next WBS unit —
// the same transition-authority discipline as T8.24.3: the render scans
// clean against a bounded set of selection phrases while the
// next_action field remains present (chosen wording: the schema names
// the fields; only the transition-authority note speaks, and it names
// the overseer as the transition authority).

var p823Kinds = []string{"atomic", "milestone", "integration"}

var p823ImplStatuses = []string{"complete", "incomplete", "blocked", "failed"}

// T8.41.1 — Required unit fields specified: the planning schema names
// the required unit fields (id, kind, objective, constraints) and
// requires them.
func TestT8411_RequiredUnitFieldsSpecified(t *testing.T) {
	comps, err := ComposePlanningResponseSchema(uuid.New(), 1, p823Kinds)
	if err != nil {
		t.Fatalf("ComposePlanningResponseSchema: %v", err)
	}
	if len(comps) != 1 {
		t.Fatalf("want exactly 1 RESPONSE_PROTOCOL component, got %d", len(comps))
	}
	c := comps[0]
	if c.Type != ComponentResponseProtocol {
		t.Fatalf("want RESPONSE_PROTOCOL, got %s", c.Type)
	}
	for _, field := range []string{`"id"`, `"kind"`, `"objective"`, `"constraints"`, "required"} {
		if !strings.Contains(c.Content, field) {
			t.Fatalf("planning schema missing required unit field %q; content:\n%s", field, c.Content)
		}
	}
}

// T8.41.2 — Dependency field specified: the planning schema requires a
// dependencies field on each unit.
func TestT8412_DependencyFieldSpecified(t *testing.T) {
	comps, err := ComposePlanningResponseSchema(uuid.New(), 1, p823Kinds)
	if err != nil {
		t.Fatalf("ComposePlanningResponseSchema: %v", err)
	}
	if !strings.Contains(comps[0].Content, `"dependencies"`) {
		t.Fatalf("planning schema missing dependencies field; content:\n%s", comps[0].Content)
	}
}

// T8.41.3 — Acceptance criteria specified: the planning schema requires
// an acceptance field on each unit.
func TestT8413_AcceptanceCriteriaSpecified(t *testing.T) {
	comps, err := ComposePlanningResponseSchema(uuid.New(), 1, p823Kinds)
	if err != nil {
		t.Fatalf("ComposePlanningResponseSchema: %v", err)
	}
	if !strings.Contains(comps[0].Content, `"acceptance"`) {
		t.Fatalf("planning schema missing acceptance field; content:\n%s", comps[0].Content)
	}
}

// T8.41.4 — Tests specified: the planning schema requires a tests field
// on each unit.
func TestT8414_TestsSpecified(t *testing.T) {
	comps, err := ComposePlanningResponseSchema(uuid.New(), 1, p823Kinds)
	if err != nil {
		t.Fatalf("ComposePlanningResponseSchema: %v", err)
	}
	if !strings.Contains(comps[0].Content, `"tests"`) {
		t.Fatalf("planning schema missing tests field; content:\n%s", comps[0].Content)
	}
}

// T8.41.5 — Work-unit kinds specified from configuration: the render
// reflects the CALLER-SUPPLIED kinds verbatim and in captured order,
// and carries no kind that was not supplied (no hard-coded list — the
// P8-22 captured-values discipline).
func TestT8415_WorkUnitKindsFromConfiguration(t *testing.T) {
	comps, err := ComposePlanningResponseSchema(uuid.New(), 2, []string{"custom-a", "custom-b"})
	if err != nil {
		t.Fatalf("ComposePlanningResponseSchema: %v", err)
	}
	content := comps[0].Content
	for _, k := range []string{"custom-a", "custom-b"} {
		if !strings.Contains(content, k) {
			t.Fatalf("configured kind %q not included in render; content:\n%s", k, content)
		}
	}
	for _, absent := range []string{"atomic", "milestone", "integration"} {
		if strings.Contains(content, absent) {
			t.Fatalf("render carries kind %q that was not supplied (hard-coded list?); content:\n%s", absent, content)
		}
	}
	if i1, i2 := strings.Index(content, "custom-a"), strings.Index(content, "custom-b"); i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("configured kinds must render in captured order; content:\n%s", content)
	}
}

// T8.42.1 — Status required: the implementation schema marks the status
// field as required.
func TestT8421_StatusRequired(t *testing.T) {
	comps, err := ComposeImplementationResponseSchema(uuid.New(), 1, p823ImplStatuses)
	if err != nil {
		t.Fatalf("ComposeImplementationResponseSchema: %v", err)
	}
	c := comps[0]
	if c.Type != ComponentResponseProtocol {
		t.Fatalf("want RESPONSE_PROTOCOL, got %s", c.Type)
	}
	for _, marker := range []string{`"status"`, "required"} {
		if !strings.Contains(c.Content, marker) {
			t.Fatalf("implementation schema missing %q; content:\n%s", marker, c.Content)
		}
	}
}

// T8.42.2 — Allowed statuses specified: the render carries the
// caller-supplied allowed statuses, spelled exactly, joined as the
// status field's value set (the WBS 8.42 status enumeration).
func TestT8422_AllowedStatusesSpecified(t *testing.T) {
	comps, err := ComposeImplementationResponseSchema(uuid.New(), 3, p823ImplStatuses)
	if err != nil {
		t.Fatalf("ComposeImplementationResponseSchema: %v", err)
	}
	content := comps[0].Content
	for _, status := range p823ImplStatuses {
		if !strings.Contains(content, status) {
			t.Fatalf("allowed status %q not included in render; content:\n%s", status, content)
		}
	}
	if !strings.Contains(content, "complete|incomplete|blocked|failed") {
		t.Fatalf("allowed statuses must render as the status field's exact value set; content:\n%s", content)
	}
}

// T8.42.3 — Summary required according to protocol: the implementation
// schema marks the summary field as required.
func TestT8423_SummaryRequired(t *testing.T) {
	comps, err := ComposeImplementationResponseSchema(uuid.New(), 1, p823ImplStatuses)
	if err != nil {
		t.Fatalf("ComposeImplementationResponseSchema: %v", err)
	}
	content := comps[0].Content
	for _, marker := range []string{`"summary"`, "required"} {
		if !strings.Contains(content, marker) {
			t.Fatalf("implementation schema missing %q; content:\n%s", marker, content)
		}
	}
}

// T8.42.4 — Remaining work supported: the implementation schema carries
// the remaining field (outstanding work the worker reports).
func TestT8424_RemainingWorkSupported(t *testing.T) {
	comps, err := ComposeImplementationResponseSchema(uuid.New(), 1, p823ImplStatuses)
	if err != nil {
		t.Fatalf("ComposeImplementationResponseSchema: %v", err)
	}
	if !strings.Contains(comps[0].Content, `"remaining"`) {
		t.Fatalf("implementation schema missing remaining field; content:\n%s", comps[0].Content)
	}
}

// T8.42.5 — Worker isn't instructed to select next WBS unit: the render
// supports the WBS 8.42 "next_action" field, yet carries no directive
// telling the worker to select/choose the next unit (transition
// authority stays with the overseer — T8.24.3 discipline; the bounded
// phrase set below is the chosen pin, and next_action's presence is
// asserted so the field is not dropped to satisfy the scan).
func TestT8425_NoNextUnitSelectionDirective(t *testing.T) {
	comps, err := ComposeImplementationResponseSchema(uuid.New(), 1, p823ImplStatuses)
	if err != nil {
		t.Fatalf("ComposeImplementationResponseSchema: %v", err)
	}
	content := comps[0].Content
	if !strings.Contains(content, `next_action`) {
		t.Fatalf("implementation schema must keep the next_action field; content:\n%s", content)
	}
	note := strings.ToLower(content)
	for _, directive := range []string{
		"select the next unit",
		"choose the next unit",
		"select the next wbs unit",
		"choose the next wbs unit",
		"select a unit",
		"choose a unit",
		"you may select",
		"you may choose",
	} {
		if strings.Contains(note, directive) {
			t.Fatalf("render instructs the worker to select the next WBS unit (contains %q); content:\n%s", directive, content)
		}
	}
}

// T8.43.1 — PASS supported: the validation schema's status field
// carries the pass value.
func TestT8431_PassSupported(t *testing.T) {
	comps, err := ComposeValidationResponseSchema(uuid.New(), 1)
	if err != nil {
		t.Fatalf("ComposeValidationResponseSchema: %v", err)
	}
	if len(comps) != 1 || comps[0].Type != ComponentResponseProtocol {
		t.Fatalf("want exactly 1 RESPONSE_PROTOCOL component, got %d", len(comps))
	}
	if !strings.Contains(comps[0].Content, `pass`) {
		t.Fatalf("validation schema missing pass status; content:\n%s", comps[0].Content)
	}
}

// T8.43.2 — FAIL supported: the validation schema's status field
// carries the fail value.
func TestT8432_FailSupported(t *testing.T) {
	comps, err := ComposeValidationResponseSchema(uuid.New(), 1)
	if err != nil {
		t.Fatalf("ComposeValidationResponseSchema: %v", err)
	}
	if !strings.Contains(comps[0].Content, `fail`) {
		t.Fatalf("validation schema missing fail status; content:\n%s", comps[0].Content)
	}
}

// T8.43.3 — Criteria results supported: the validation schema carries
// the criteria field.
func TestT8433_CriteriaResultsSupported(t *testing.T) {
	comps, err := ComposeValidationResponseSchema(uuid.New(), 1)
	if err != nil {
		t.Fatalf("ComposeValidationResponseSchema: %v", err)
	}
	if !strings.Contains(comps[0].Content, `"criteria"`) {
		t.Fatalf("validation schema missing criteria field; content:\n%s", comps[0].Content)
	}
}

// T8.43.4 — Test results supported: the validation schema carries the
// tests field.
func TestT8434_TestResultsSupported(t *testing.T) {
	comps, err := ComposeValidationResponseSchema(uuid.New(), 1)
	if err != nil {
		t.Fatalf("ComposeValidationResponseSchema: %v", err)
	}
	if !strings.Contains(comps[0].Content, `"tests"`) {
		t.Fatalf("validation schema missing tests field; content:\n%s", comps[0].Content)
	}
}

// T8.43.5 — Failures supported: the validation schema carries the
// failures field.
func TestT8435_FailuresSupported(t *testing.T) {
	comps, err := ComposeValidationResponseSchema(uuid.New(), 1)
	if err != nil {
		t.Fatalf("ComposeValidationResponseSchema: %v", err)
	}
	if !strings.Contains(comps[0].Content, `"failures"`) {
		t.Fatalf("validation schema missing failures field; content:\n%s", comps[0].Content)
	}
}

// Failure contract (the P8-22 pattern): a cleared captured version is a
// composition FAILURE with the typed sentinel for all three schemas —
// no silent fallback.
func TestP823_ClearedCapturedVersionIsFailure(t *testing.T) {
	pid := uuid.New()
	if _, err := ComposePlanningResponseSchema(pid, 0, p823Kinds); !errors.Is(err, ErrMissingProtocolVersion) {
		t.Fatalf("planning cleared version err = %v, want ErrMissingProtocolVersion", err)
	}
	if _, err := ComposeImplementationResponseSchema(pid, 0, p823ImplStatuses); !errors.Is(err, ErrMissingProtocolVersion) {
		t.Fatalf("implementation cleared version err = %v, want ErrMissingProtocolVersion", err)
	}
	if _, err := ComposeValidationResponseSchema(pid, 0); !errors.Is(err, ErrMissingProtocolVersion) {
		t.Fatalf("validation cleared version err = %v, want ErrMissingProtocolVersion", err)
	}
}

// Failure contract: empty configuration inputs are composition
// FAILURES with typed sentinels (kinds / allowed statuses come from
// configuration — an empty capture fails rather than rendering a
// hard-coded default).
func TestP823_EmptyConfigurationInputsAreFailure(t *testing.T) {
	pid := uuid.New()
	if _, err := ComposePlanningResponseSchema(pid, 1, nil); !errors.Is(err, ErrMissingWorkUnitKinds) {
		t.Fatalf("planning empty kinds err = %v, want ErrMissingWorkUnitKinds", err)
	}
	if _, err := ComposeImplementationResponseSchema(pid, 1, nil); !errors.Is(err, ErrMissingAllowedStatuses) {
		t.Fatalf("implementation empty statuses err = %v, want ErrMissingAllowedStatuses", err)
	}
}

// Determinism: repeated composition over the same input is
// byte-identical for all three schemas (the composition-input
// requirement shared with the other P8 composition tasks).
func TestP823_RepeatedCompositionByteIdentical(t *testing.T) {
	pid := uuid.New()
	firstPlan, err := ComposePlanningResponseSchema(pid, 3, p823Kinds)
	if err != nil {
		t.Fatalf("ComposePlanningResponseSchema: %v", err)
	}
	firstImpl, err := ComposeImplementationResponseSchema(pid, 3, p823ImplStatuses)
	if err != nil {
		t.Fatalf("ComposeImplementationResponseSchema: %v", err)
	}
	firstVal, err := ComposeValidationResponseSchema(pid, 3)
	if err != nil {
		t.Fatalf("ComposeValidationResponseSchema: %v", err)
	}
	for i := 0; i < 50; i++ {
		plan, err := ComposePlanningResponseSchema(pid, 3, p823Kinds)
		if err != nil {
			t.Fatalf("planning repeat %d: %v", i, err)
		}
		if len(plan) != len(firstPlan) || plan[0] != firstPlan[0] {
			t.Fatalf("planning repeated composition not byte-identical at %d", i)
		}
		impl, err := ComposeImplementationResponseSchema(pid, 3, p823ImplStatuses)
		if err != nil {
			t.Fatalf("implementation repeat %d: %v", i, err)
		}
		if len(impl) != len(firstImpl) || impl[0] != firstImpl[0] {
			t.Fatalf("implementation repeated composition not byte-identical at %d", i)
		}
		val, err := ComposeValidationResponseSchema(pid, 3)
		if err != nil {
			t.Fatalf("validation repeat %d: %v", i, err)
		}
		if len(val) != len(firstVal) || val[0] != firstVal[0] {
			t.Fatalf("validation repeated composition not byte-identical at %d", i)
		}
	}
}
