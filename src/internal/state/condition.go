package state

import (
	"fmt"
)

// ConditionOperator is the fixed, structured operator set for transition
// conditions (WBS 6.10). Operators are data, never state/outcome names.
type ConditionOperator string

const (
	OpEQ  ConditionOperator = "=="
	OpNEQ ConditionOperator = "!="
	OpGT  ConditionOperator = ">"
	OpGTE ConditionOperator = ">="
	OpLT  ConditionOperator = "<"
	OpLTE ConditionOperator = "<="
)

// supportedOperators is the accepted set; anything else is rejected at
// construction (T6.10.6).
var supportedOperators = map[ConditionOperator]bool{
	OpEQ: true, OpNEQ: true, OpGT: true, OpGTE: true, OpLT: true, OpLTE: true,
}

// SupportedOperators reports the fixed operator set (deterministic order).
func SupportedOperators() []ConditionOperator {
	return []ConditionOperator{OpEQ, OpNEQ, OpGT, OpGTE, OpLT, OpLTE}
}

// DeclaredConditionFields is the declared condition-field set (WBS 6.10
// examples). Conditions may reference these fields only; unknown fields are
// rejected at construction (T6.10.5).
var DeclaredConditionFields = []string{
	"context_usage",
	"remaining_work_units",
	"all_work_units_done",
	"remaining_phases",
	"validation_passed",
	"project.context_threshold",
}

// DeclaredConditionField reports whether f is in the declared field set.
func DeclaredConditionField(f string) bool {
	for _, d := range DeclaredConditionFields {
		if d == f {
			return true
		}
	}
	return false
}

// ConditionValue is a condition's right-hand side (WBS 6.10 "value_from"
// shape): either a Literal constant or a ContextField reference resolved
// from the transition context at evaluation time. Exactly one must be set.
type ConditionValue struct {
	Literal      any
	ContextField string
}

// ConditionInput carries the caller-supplied fields for NewCondition.
type ConditionInput struct {
	Field    string
	Operator ConditionOperator
	Value    ConditionValue
}

// Condition is a structured, deterministic transition condition (WBS 6.10):
// field operator value, no arbitrary executable code.
type Condition struct {
	Field    string
	Operator ConditionOperator
	Value    ConditionValue
}

// NewCondition validates and constructs a condition (WBS 6.10): the field
// must be declared (T6.10.5), the operator must be in the fixed set
// (T6.10.6), and the value must be exactly one of literal / context-field.
func NewCondition(in ConditionInput) (*Condition, error) {
	if !DeclaredConditionField(in.Field) {
		return nil, fmt.Errorf("state: unknown condition field %q", in.Field)
	}
	if !supportedOperators[in.Operator] {
		return nil, fmt.Errorf("state: unsupported operator %q", in.Operator)
	}
	hasLiteral := in.Value.Literal != nil
	hasRef := in.Value.ContextField != ""
	if hasLiteral == hasRef {
		return nil, fmt.Errorf("state: condition value must be exactly one of literal or context field")
	}
	return &Condition{Field: in.Field, Operator: in.Operator, Value: in.Value}, nil
}

// Evaluate is PURE and DETERMINISTIC (T6.10.7): (condition, values) -> bool
// with no side effects, no I/O, no DB. A missing declared-optional value
// evaluates to false, never a panic.
func (c *Condition) Evaluate(values map[string]any) bool {
	left, ok := values[c.Field]
	if !ok {
		return false
	}
	var right any
	if c.Value.ContextField != "" {
		r, ok := values[c.Value.ContextField]
		if !ok {
			return false
		}
		right = r
	} else {
		right = c.Value.Literal
	}
	return compare(left, right, c.Operator)
}

// compare evaluates two context values under a fixed operator set. Incomparable
// types are deterministically false (no panic).
func compare(left, right any, op ConditionOperator) bool {
	switch op {
	case OpEQ, OpNEQ:
		eq := valuesEqual(left, right)
		if op == OpEQ {
			return eq
		}
		return !eq
	default:
		l, ok := asFloat(left)
		r, ok2 := asFloat(right)
		if !ok || !ok2 {
			return false
		}
		switch op {
		case OpGT:
			return l > r
		case OpGTE:
			return l >= r
		case OpLT:
			return l < r
		case OpLTE:
			return l <= r
		}
		return false
	}
}

// valuesEqual compares booleans, strings, and numbers across integer/float
// representations; mismatched kinds are false.
func valuesEqual(left, right any) bool {
	if lb, ok := left.(bool); ok {
		rb, ok := right.(bool)
		return ok && lb == rb
	}
	if ls, ok := left.(string); ok {
		rs, ok := right.(string)
		return ok && ls == rs
	}
	l, ok := asFloat(left)
	r, ok2 := asFloat(right)
	return ok && ok2 && l == r
}

// asFloat normalizes the numeric representations the domain uses.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
