package wbs

import (
	"fmt"
	"strings"
)

// WorkUnitSpecification captures the content of a work unit — objective,
// implementation instructions, acceptance criteria, required tests, and
// constraints (WBS 2.13). It is deliberately language-independent: plain
// text and ordered string lists, safe for JSON (de)serialization.
type WorkUnitSpecification struct {
	Objective                  string   `json:"objective"`
	ImplementationInstructions string   `json:"implementation_instructions,omitempty"`
	AcceptanceCriteria         []string `json:"acceptance_criteria,omitempty"`
	RequiredTests              []string `json:"required_tests,omitempty"`
	Constraints                []string `json:"constraints,omitempty"`

	// Phase 16 (P16-16): plan-local key, deterministic validation
	// commands, typed acceptance checks and declared repository paths. All
	// optional; specifications written before Phase 16 decode unchanged.
	Key              string                `json:"key,omitempty"`
	Validation       []ValidationSpec      `json:"validation,omitempty"`
	AcceptanceChecks []AcceptanceCheckSpec `json:"acceptance_checks,omitempty"`
	Paths            []string              `json:"paths,omitempty"`
}

// ValidationSpec is one deterministic validation command declared for a
// work unit (WBS 16.43): a program and its argument list — never a shell
// string.
type ValidationSpec struct {
	Name           string   `json:"name,omitempty"`
	Program        string   `json:"program"`
	Args           []string `json:"args,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// Acceptance verification types (WBS 16.48).
const (
	VerifyCommand = "COMMAND"
	VerifyTest    = "TEST"
	VerifyManual  = "MANUAL"
)

// AcceptanceCheckSpec is one acceptance criterion with its declared
// verification type (WBS 16.48). COMMAND checks carry a command; TEST
// checks name required tests; MANUAL checks need an operator approval.
type AcceptanceCheckSpec struct {
	Text         string          `json:"text"`
	Verification string          `json:"verification"`
	Command      *ValidationSpec `json:"command,omitempty"`
	Tests        []string        `json:"tests,omitempty"`
}

// WorkUnitSpecificationInput is the constructor input for
// WorkUnitSpecification.
type WorkUnitSpecificationInput = WorkUnitSpecification

// NewWorkUnitSpecification validates and normalizes a work unit
// specification. The objective is required (WBS 2.13); all list fields keep
// caller order.
func NewWorkUnitSpecification(in WorkUnitSpecificationInput) (*WorkUnitSpecification, error) {
	if strings.TrimSpace(in.Objective) == "" {
		return nil, fmt.Errorf("wbs: work unit specification requires a non-empty objective")
	}
	return &in, nil
}
