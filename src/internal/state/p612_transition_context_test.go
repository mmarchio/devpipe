package state

import (
	"reflect"
	"testing"

	"overseer/internal/phase"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// T6.25.1 — Context constructed correctly.
func TestT6251ContextConstructedCorrectly(t *testing.T) {
	vp := true
	in := TransitionContextInput{
		Outcome:            "SUCCESS",
		ContextUsage:       0.55,
		ContextThreshold:   0.5,
		RemainingWorkUnits: 3,
		RemainingPhases:    2,
		ValidationPassed:   &vp,
		ProjectStatus:      project.StatusRUNNING,
		PhaseStatus:        phase.StatusEXECUTING,
		WorkUnitStatus:     wbs.WorkUnitStatusINCOMPLETE,
	}
	ctx, err := NewTransitionContext(in)
	if err != nil {
		t.Fatalf("NewTransitionContext: %v", err)
	}
	if ctx.Outcome != "SUCCESS" {
		t.Fatalf("Outcome: got %q", ctx.Outcome)
	}
	if ctx.ContextUsage != 0.55 || ctx.ContextThreshold != 0.5 {
		t.Fatalf("context usage/threshold: got %v/%v", ctx.ContextUsage, ctx.ContextThreshold)
	}
	if ctx.RemainingWorkUnits != 3 || ctx.RemainingPhases != 2 {
		t.Fatalf("remaining: got %v/%v", ctx.RemainingWorkUnits, ctx.RemainingPhases)
	}
	if ctx.ValidationPassed == nil || *ctx.ValidationPassed != true {
		t.Fatalf("ValidationPassed: got %v", ctx.ValidationPassed)
	}
	if ctx.ProjectStatus != project.StatusRUNNING {
		t.Fatalf("ProjectStatus: got %v", ctx.ProjectStatus)
	}
	if ctx.PhaseStatus != phase.StatusEXECUTING {
		t.Fatalf("PhaseStatus: got %v", ctx.PhaseStatus)
	}
	if ctx.WorkUnitStatus != wbs.WorkUnitStatusINCOMPLETE {
		t.Fatalf("WorkUnitStatus: got %v", ctx.WorkUnitStatus)
	}

	// Declared values surface to the condition engine under the declared field
	// names (WBS 6.10 field set).
	values := ctx.ConditionValues()
	if got := values["context_usage"]; got != 0.55 {
		t.Fatalf("context_usage: got %v", got)
	}
	if got := values["project.context_threshold"]; got != 0.5 {
		t.Fatalf("project.context_threshold: got %v", got)
	}
	if got := values["remaining_work_units"]; got != 3 {
		t.Fatalf("remaining_work_units: got %v", got)
	}
	if got := values["remaining_phases"]; got != 2 {
		t.Fatalf("remaining_phases: got %v", got)
	}
	if got := values["validation_passed"]; got != true {
		t.Fatalf("validation_passed: got %v", got)
	}
	// Derived declared value: all work units done exactly when none remain.
	if got := values["all_work_units_done"]; got != false {
		t.Fatalf("all_work_units_done: got %v", got)
	}
	if _, ok := values["project.status"]; ok {
		t.Fatalf("undeclared field project.status leaked into condition values")
	}
}

// T6.25.2 — Missing optional values handled.
func TestT6252MissingOptionalValuesHandled(t *testing.T) {
	ctx, err := NewTransitionContext(TransitionContextInput{
		Outcome:            "FAIL",
		ContextUsage:       0.1,
		ContextThreshold:   0.5,
		RemainingWorkUnits: 0,
		RemainingPhases:    0,
		// ValidationPassed intentionally nil (missing).
		ProjectStatus:  project.StatusREADY,
		PhaseStatus:    phase.StatusPENDING,
		WorkUnitStatus: wbs.WorkUnitStatusPENDING,
	})
	if err != nil {
		t.Fatalf("NewTransitionContext with nil optional: %v", err)
	}
	if ctx.ValidationPassed != nil {
		t.Fatalf("ValidationPassed: want nil, got %v", *ctx.ValidationPassed)
	}
	// The missing optional is representable and serializable.
	s1 := ctx.Serialize()
	if s1 == "" {
		t.Fatalf("Serialize: empty output")
	}
	if got := ctx.ConditionValues()["validation_passed"]; got != nil {
		t.Fatalf("missing ValidationPassed must not surface a value, got %v", got)
	}
	// A condition on the missing value is deterministically false (T6.10.7),
	// never a panic.
	c, err := NewCondition(ConditionInput{
		Field: "validation_passed", Operator: "==", Value: ConditionValue{Literal: true},
	})
	if err != nil {
		t.Fatalf("NewCondition: %v", err)
	}
	if c.Evaluate(ctx.ConditionValues()) {
		t.Fatalf("validation_passed missing vs literal true: want false, got true")
	}
}

// T6.25.3 — Condition engine cannot access undeclared values.
func TestT6253NoUndeclaredValues(t *testing.T) {
	ctx, err := NewTransitionContext(fullContextInput())
	if err != nil {
		t.Fatalf("NewTransitionContext: %v", err)
	}
	values := ctx.ConditionValues()
	want := map[string]any{
		"all_work_units_done":       true,
		"context_usage":             0.55,
		"remaining_work_units":      0,
		"remaining_phases":          2,
		"validation_passed":         true,
		"project.context_threshold": 0.5,
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("ConditionValues: got %#v, want exactly the declared field set %#v", values, want)
	}
	// The engine reads only ConditionValues(); a condition on any undeclared
	// field is rejected at construction (T6.10.5), so no accessor path exists.
	for _, f := range []string{"project.status", "phase.status", "work_unit_status", "outcome"} {
		if _, err := NewCondition(ConditionInput{Field: f, Operator: "==", Value: ConditionValue{Literal: 1}}); err == nil {
			t.Fatalf("undeclared field %q accepted by the condition engine", f)
		}
	}
}

// T6.25.4 — Serialization deterministic.
func TestT6254SerializationDeterministic(t *testing.T) {
	in := TransitionContextInput{
		Outcome:            "COMPLETE",
		ContextUsage:       0.75,
		ContextThreshold:   0.8,
		RemainingWorkUnits: 1,
		RemainingPhases:    0,
		ProjectStatus:      project.StatusRUNNING,
		PhaseStatus:        phase.StatusDONE,
		WorkUnitStatus:     wbs.WorkUnitStatusDONE,
	}
	first, err := NewTransitionContext(in)
	if err != nil {
		t.Fatalf("NewTransitionContext: %v", err)
	}
	second, err := NewTransitionContext(in)
	if err != nil {
		t.Fatalf("NewTransitionContext: %v", err)
	}
	want := first.Serialize()
	if want == "" {
		t.Fatalf("Serialize: empty output")
	}
	for i := 0; i < 5; i++ {
		if got := first.Serialize(); got != want {
			t.Fatalf("repeated Serialize: output changed")
		}
	}
	if got := second.Serialize(); got != want {
		t.Fatalf("equal contexts: Serialize must be byte-identical")
	}

	// A missing optional serializes to a distinct, stable marker.
	if got := first.Serialize(); !contains(got, `validation=-`) {
		t.Fatalf("nil optional must serialize as absent, got %q", got)
	}
	vp := true
	withV, err := NewTransitionContext(TransitionContextInput{
		Outcome:            "COMPLETE",
		ContextUsage:       0.75,
		ContextThreshold:   0.8,
		RemainingWorkUnits: 1,
		RemainingPhases:    0,
		ValidationPassed:   &vp,
		ProjectStatus:      project.StatusRUNNING,
		PhaseStatus:        phase.StatusDONE,
		WorkUnitStatus:     wbs.WorkUnitStatusDONE,
	})
	if err != nil {
		t.Fatalf("NewTransitionContext: %v", err)
	}
	if got := withV.Serialize(); !contains(got, `validation="true"`) || got == want {
		t.Fatalf("present optional must serialize differently, got %q", got)
	}
}

// fullContextInput builds a fully-populated context input.
func fullContextInput() TransitionContextInput {
	vp := true
	return TransitionContextInput{
		Outcome:            "SUCCESS",
		ContextUsage:       0.55,
		ContextThreshold:   0.5,
		RemainingWorkUnits: 0,
		RemainingPhases:    2,
		ValidationPassed:   &vp,
		ProjectStatus:      project.StatusRUNNING,
		PhaseStatus:        phase.StatusEXECUTING,
		WorkUnitStatus:     wbs.WorkUnitStatusDONE,
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// Contract guard: the declared surface of the transition context. A signature
// drift breaks this package's build, pinning the T6.25 contract.
var _ = func() (int, int) {
	var c TransitionContext
	_ = c
	return len(c.Serialize()), len(c.ConditionValues())
}
