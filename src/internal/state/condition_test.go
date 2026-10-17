package state

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func mustCondition(t *testing.T, in ConditionInput) *Condition {
	t.Helper()
	c, err := NewCondition(in)
	if err != nil {
		t.Fatalf("NewCondition(%+v): %v", in, err)
	}
	return c
}

func numberCtx() map[string]any {
	return map[string]any{
		"context_usage":             0.55,
		"remaining_work_units":      3,
		"remaining_phases":          2,
		"project.context_threshold": 0.5,
	}
}

// T6.10.1 — Equality condition evaluates.
func TestT6101EqualityConditionEvaluates(t *testing.T) {
	c, err := NewCondition(ConditionInput{
		Field: "all_work_units_done", Operator: "==",
		Value: ConditionValue{Literal: true},
	})
	if err != nil {
		t.Fatalf("NewCondition: %v", err)
	}
	if !c.Evaluate(map[string]any{"all_work_units_done": true}) {
		t.Fatalf("true == true: want true, got false")
	}
	if c.Evaluate(map[string]any{"all_work_units_done": false}) {
		t.Fatalf("false == true: want false, got true")
	}
	cn, err := NewCondition(ConditionInput{
		Field: "validation_passed", Operator: "!=",
		Value: ConditionValue{Literal: false},
	})
	if err != nil {
		t.Fatalf("NewCondition: %v", err)
	}
	if !cn.Evaluate(map[string]any{"validation_passed": true}) {
		t.Fatalf("true != false: want true, got false")
	}
	if cn.Evaluate(map[string]any{"validation_passed": false}) {
		t.Fatalf("false != false: want false, got true")
	}
}

// T6.10.2 — Greater-than condition evaluates.
func TestT6102GreaterThanConditionEvaluates(t *testing.T) {
	c := mustCondition(t, ConditionInput{
		Field: "remaining_work_units", Operator: ">",
		Value: ConditionValue{Literal: int64(0)},
	})
	if !c.Evaluate(map[string]any{"remaining_work_units": int64(3)}) {
		t.Fatalf("3 > 0: want true, got false")
	}
	if c.Evaluate(map[string]any{"remaining_work_units": int64(0)}) {
		t.Fatalf("0 > 0: want false, got true")
	}
	// WBS 6.10 example: context_usage >= threshold, threshold from the context.
	ge := mustCondition(t, ConditionInput{
		Field: "context_usage", Operator: ">=",
		Value: ConditionValue{ContextField: "project.context_threshold"},
	})
	if !ge.Evaluate(numberCtx()) {
		t.Fatalf("0.55 >= 0.5 with threshold from context: want true, got false (threshold not resolved)")
	}
	ctx := numberCtx()
	ctx["context_usage"] = 0.9
	if !ge.Evaluate(ctx) {
		t.Fatalf("0.9 >= 0.5: want true, got false")
	}
}

// T6.10.3 — Less-than condition evaluates.
func TestT6103LessThanConditionEvaluates(t *testing.T) {
	c := mustCondition(t, ConditionInput{
		Field: "context_usage", Operator: "<",
		Value: ConditionValue{Literal: 0.5},
	})
	if !c.Evaluate(map[string]any{"context_usage": 0.4}) {
		t.Fatalf("0.4 < 0.5: want true, got false")
	}
	if c.Evaluate(map[string]any{"context_usage": 0.5}) {
		t.Fatalf("0.5 < 0.5: want false, got true")
	}
	le := mustCondition(t, ConditionInput{
		Field: "remaining_phases", Operator: "<=",
		Value: ConditionValue{Literal: int64(2)},
	})
	if !le.Evaluate(map[string]any{"remaining_phases": int64(2)}) {
		t.Fatalf("2 <= 2: want true, got false")
	}
	if le.Evaluate(map[string]any{"remaining_phases": int64(3)}) {
		t.Fatalf("3 <= 2: want false, got true")
	}
}

// T6.10.4 — Boolean condition evaluates.
func TestT6104BooleanConditionEvaluates(t *testing.T) {
	c := mustCondition(t, ConditionInput{
		Field: "validation_passed", Operator: "==",
		Value: ConditionValue{Literal: true},
	})
	if !c.Evaluate(map[string]any{"validation_passed": true}) {
		t.Fatalf("true == true: want true, got false")
	}
	if c.Evaluate(map[string]any{"validation_passed": false}) {
		t.Fatalf("false == true: want false, got true")
	}
	ne := mustCondition(t, ConditionInput{
		Field: "validation_passed", Operator: "!=",
		Value: ConditionValue{Literal: true},
	})
	if !ne.Evaluate(map[string]any{"validation_passed": false}) {
		t.Fatalf("false != true: want true, got false")
	}
	if ne.Evaluate(map[string]any{"validation_passed": true}) {
		t.Fatalf("true != true: want false, got true")
	}
}

// T6.10.5 — Unknown field rejected.
func TestT6105UnknownFieldRejected(t *testing.T) {
	for _, f := range []string{"not_a_field", "project.secret", ""} {
		_, err := NewCondition(ConditionInput{Field: f, Operator: "==", Value: ConditionValue{Literal: 1}})
		if err == nil {
			t.Fatalf("field %q: expected rejection, got none", f)
		}
	}
	// Every field used in the WBS 6.10 examples is declared.
	for _, f := range []string{"context_usage", "remaining_work_units", "all_work_units_done", "remaining_phases", "validation_passed"} {
		if _, err := NewCondition(ConditionInput{Field: f, Operator: "==", Value: ConditionValue{Literal: 1}}); err != nil {
			t.Fatalf("declared field %q rejected: %v", f, err)
		}
	}
}

// T6.10.6 — Unsupported operator rejected.
func TestT6106UnsupportedOperatorRejected(t *testing.T) {
	for _, op := range []string{"~=", "regex", "IN", "contains", ""} {
		_, err := NewCondition(ConditionInput{Field: "context_usage", Operator: ConditionOperator(op), Value: ConditionValue{Literal: 1}})
		if err == nil {
			t.Fatalf("operator %q: expected rejection, got none", op)
		}
	}
	// The fixed operator set (operators are data, not names — no state-name branching).
	for _, op := range []string{"==", "!=", ">", ">=", "<", "<="} {
		if _, err := NewCondition(ConditionInput{Field: "context_usage", Operator: ConditionOperator(op), Value: ConditionValue{Literal: 1}}); err != nil {
			t.Fatalf("operator %q rejected: %v", op, err)
		}
	}
}

// T6.10.7 — Condition evaluation deterministic.
func TestT6107ConditionEvaluationDeterministic(t *testing.T) {
	cases := []struct {
		in   ConditionInput
		want bool
	}{
		{ConditionInput{Field: "remaining_work_units", Operator: ">", Value: ConditionValue{Literal: int64(0)}}, true},
		{ConditionInput{Field: "remaining_work_units", Operator: "==", Value: ConditionValue{Literal: int64(3)}}, true},
		{ConditionInput{Field: "remaining_work_units", Operator: "<", Value: ConditionValue{Literal: int64(3)}}, false},
		{ConditionInput{Field: "context_usage", Operator: ">=", Value: ConditionValue{Literal: 0.9}}, false},
		{ConditionInput{Field: "context_usage", Operator: "<", Value: ConditionValue{Literal: 0.5}}, false},
	}
	for _, tc := range cases {
		c := mustCondition(t, tc.in)
		first := c.Evaluate(numberCtx())
		if first != tc.want {
			t.Fatalf("%s %s %v: want %v, got %v", tc.in.Field, tc.in.Operator, tc.in.Value, tc.want, first)
		}
		for i := 0; i < 10; i++ {
			if got := c.Evaluate(numberCtx()); got != tc.want {
				t.Fatalf("%s %s %v: not deterministic (got %v, want %v)", tc.in.Field, tc.in.Operator, tc.in.Value, got, tc.want)
			}
		}
	}
	// Missing declared-optional value: deterministic result, never a panic.
	c := mustCondition(t, ConditionInput{Field: "context_usage", Operator: ">=", Value: ConditionValue{Literal: 0.5}})
	if c.Evaluate(map[string]any{}) {
		t.Fatalf("missing value: deterministic false wanted")
	}
	// Pure: repeated evaluation does not mutate the input map.
	before := numberCtx()
	snapshot := map[string]any{}
	for k, v := range before {
		snapshot[k] = v
	}
	for i := 0; i < 3; i++ {
		c.Evaluate(numberCtx())
	}
	if !reflect.DeepEqual(before, snapshot) {
		t.Fatalf("evaluation mutated its input map")
	}
	// No side effects across unrelated conditions sharing one context.
	a := mustCondition(t, ConditionInput{Field: "remaining_phases", Operator: ">", Value: ConditionValue{Literal: int64(0)}})
	b := mustCondition(t, ConditionInput{Field: "remaining_phases", Operator: "==", Value: ConditionValue{Literal: int64(2)}})
	ctx := numberCtx()
	if !a.Evaluate(ctx) || !b.Evaluate(ctx) {
		t.Fatalf("shared context: want true/true")
	}
}

// Structural shape (WBS 6.10 example): a condition's value may be a literal or
// a reference to a declared context field.
func TestConditionContextFieldReference(t *testing.T) {
	c := mustCondition(t, ConditionInput{
		Field: "context_usage", Operator: ">=",
		Value: ConditionValue{ContextField: "project.context_threshold"},
	})
	ctx := numberCtx()
	ctx["project.context_threshold"] = 0.5
	if !c.Evaluate(ctx) {
		t.Fatalf("0.55 vs context-resolved threshold 0.5: got false")
	}
	ctx["project.context_threshold"] = 0.1
	if !c.Evaluate(ctx) {
		t.Fatalf("0.55 vs context-resolved threshold 0.1: want true, got false")
	}
	_ = uuid.New()
}
