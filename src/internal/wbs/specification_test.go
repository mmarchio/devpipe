package wbs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// T2.13.1 — Objective persists.
func TestT2131ObjectivePersists(t *testing.T) {
	spec, err := NewWorkUnitSpecification(WorkUnitSpecificationInput{
		Objective: "Implement email validation.",
	})
	if err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	if spec.Objective != "Implement email validation." {
		t.Fatalf("objective not persisted: %q", spec.Objective)
	}
	if strings.TrimSpace(spec.Objective) == "" {
		t.Fatal("objective is empty")
	}
}

// T2.13.2 — Multiple acceptance criteria persist in order.
func TestT2132AcceptanceCriteriaOrder(t *testing.T) {
	spec, err := NewWorkUnitSpecification(WorkUnitSpecificationInput{
		Objective: "Implement email validation.",
		AcceptanceCriteria: []string{
			"malformed email rejected",
			"valid email accepted",
			"empty input rejected",
		},
	})
	if err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	want := []string{
		"malformed email rejected",
		"valid email accepted",
		"empty input rejected",
	}
	if !reflect.DeepEqual(spec.AcceptanceCriteria, want) {
		t.Fatalf("acceptance criteria %v, want %v (order lost)", spec.AcceptanceCriteria, want)
	}
}

// T2.13.3 — Multiple test requirements persist in order.
func TestT2133RequiredTestsOrder(t *testing.T) {
	spec, err := NewWorkUnitSpecification(WorkUnitSpecificationInput{
		Objective: "Implement email validation.",
		RequiredTests: []string{
			"malformed input",
			"valid input",
			"unicode input",
		},
	})
	if err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	want := []string{"malformed input", "valid input", "unicode input"}
	if !reflect.DeepEqual(spec.RequiredTests, want) {
		t.Fatalf("required tests %v, want %v (order lost)", spec.RequiredTests, want)
	}
}

// T2.13.4 — Constraints persist.
func TestT2134ConstraintsPersist(t *testing.T) {
	spec, err := NewWorkUnitSpecification(WorkUnitSpecificationInput{
		Objective:                  "Implement email validation.",
		ImplementationInstructions: "Add a ValidateEmail function in the email package.",
		Constraints: []string{
			"don't modify unrelated authentication code",
			"stdlib only",
		},
	})
	if err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	want := []string{
		"don't modify unrelated authentication code",
		"stdlib only",
	}
	if !reflect.DeepEqual(spec.Constraints, want) {
		t.Fatalf("constraints %v, want %v", spec.Constraints, want)
	}
	if spec.ImplementationInstructions != "Add a ValidateEmail function in the email package." {
		t.Fatalf("implementation instructions %q not persisted", spec.ImplementationInstructions)
	}
}

// T2.13.5 — Unicode/code-related content survives round trip.
func TestT2135UnicodeRoundTrip(t *testing.T) {
	orig, err := NewWorkUnitSpecification(WorkUnitSpecificationInput{
		Objective:          "Validate e-mails, including ünïcode like 邮箱@example.com.",
		AcceptanceCriteria: []string{"naïve input ok", "code: s := strings.TrimSpace(x)"},
		RequiredTests:      []string{"test ünïcode", "test 邮箱"},
		Constraints:        []string{"don't touch 認証 code"},
	})
	if err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back WorkUnitSpecification
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, *orig) {
		t.Fatalf("round trip mismatch:\ngot  %+v\nwant %+v", back, *orig)
	}
}
