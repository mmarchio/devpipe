package wbs

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// WorkUnitKind is the generic, language-independent classification of a
// work unit (WBS 2.12).
type WorkUnitKind string

const (
	WorkUnitKindATOMIC      WorkUnitKind = "ATOMIC"
	WorkUnitKindCOMPOSITION WorkUnitKind = "COMPOSITION"
	WorkUnitKindINTEGRATION WorkUnitKind = "INTEGRATION"
	WorkUnitKindVALIDATION  WorkUnitKind = "VALIDATION"
)

// workUnitKinds is the closed set of work-unit kinds.
var workUnitKinds = map[WorkUnitKind]bool{
	WorkUnitKindATOMIC:      true,
	WorkUnitKindCOMPOSITION: true,
	WorkUnitKindINTEGRATION: true,
	WorkUnitKindVALIDATION:  true,
}

func (k WorkUnitKind) String() string { return string(k) }

// ParseWorkUnitKind converts a token into a WorkUnitKind, rejecting unknown
// values (T2.12.4).
func ParseWorkUnitKind(token string) (WorkUnitKind, error) {
	k := WorkUnitKind(strings.ToUpper(strings.TrimSpace(token)))
	if !workUnitKinds[k] {
		return "", fmt.Errorf("wbs: unknown work unit kind %q", token)
	}
	return k, nil
}

// WorkUnitStatus is the domain-typed state of a work unit (WBS 2.12).
type WorkUnitStatus string

const (
	WorkUnitStatusPENDING    WorkUnitStatus = "PENDING"
	WorkUnitStatusREADY      WorkUnitStatus = "READY"
	WorkUnitStatusEXECUTING  WorkUnitStatus = "EXECUTING"
	WorkUnitStatusINCOMPLETE WorkUnitStatus = "INCOMPLETE"
	WorkUnitStatusVALIDATING WorkUnitStatus = "VALIDATING"
	WorkUnitStatusDONE       WorkUnitStatus = "DONE"
	WorkUnitStatusFAILED     WorkUnitStatus = "FAILED"
	WorkUnitStatusBLOCKED    WorkUnitStatus = "BLOCKED"
)

// workUnitStatuses is the closed set of work-unit statuses.
var workUnitStatuses = map[WorkUnitStatus]bool{
	WorkUnitStatusPENDING:    true,
	WorkUnitStatusREADY:      true,
	WorkUnitStatusEXECUTING:  true,
	WorkUnitStatusINCOMPLETE: true,
	WorkUnitStatusVALIDATING: true,
	WorkUnitStatusDONE:       true,
	WorkUnitStatusFAILED:     true,
	WorkUnitStatusBLOCKED:    true,
}

func (s WorkUnitStatus) String() string { return string(s) }

// ParseWorkUnitStatus converts a token into a WorkUnitStatus, rejecting
// unknown values (T2.12.5).
func ParseWorkUnitStatus(token string) (WorkUnitStatus, error) {
	s := WorkUnitStatus(strings.ToUpper(strings.TrimSpace(token)))
	if !workUnitStatuses[s] {
		return "", fmt.Errorf("wbs: unknown work unit status %q", token)
	}
	return s, nil
}

// WorkUnit is one ordered unit of work inside an execution plan (WBS 2.12).
// It is deliberately generic, not Go-function-specific.
type WorkUnit struct {
	ID              uuid.UUID
	ExecutionPlanID uuid.UUID

	// ProjectID is the project the unit belongs to (the plan's phase's
	// project), backfilled at persistence time (P6-30: the runtime-state
	// read resolves the association project-scoped).
	ProjectID uuid.UUID

	Sequence int

	Name        string
	Description string

	Kind WorkUnitKind

	Status WorkUnitStatus

	// Specification holds the work unit's objective and structured
	// acceptance content (WBS 2.13). Optional at the domain level.
	Specification *WorkUnitSpecification

	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time

	// Phase 16 execution bindings (P16-58): the unit an amendment copied
	// (nil = original), the execution holding the claim, and the PASS
	// validation run that justified DONE.
	CarriedFromUnitID   *uuid.UUID
	ClaimedExecutionID  *uuid.UUID
	DoneValidationRunID *uuid.UUID
}

// NewWorkUnit constructs a work unit in the PENDING state (T2.12.6) with a
// freshly generated ID and UTC CreatedAt. It requires a non-zero execution
// plan ID (T2.12.2), a positive sequence (T2.12.3), and a parseable kind.
func NewWorkUnit(planID uuid.UUID, sequence int, name string, kind WorkUnitKind) (*WorkUnit, error) {
	if planID == uuid.Nil {
		return nil, fmt.Errorf("wbs: work unit requires an execution plan ID")
	}
	if sequence <= 0 {
		return nil, fmt.Errorf("wbs: work unit sequence must be positive, got %d", sequence)
	}
	if !workUnitKinds[kind] {
		return nil, fmt.Errorf("wbs: unknown work unit kind %q", kind)
	}
	now := time.Now().UTC()
	return &WorkUnit{
		ID:              uuid.New(),
		ExecutionPlanID: planID,
		Sequence:        sequence,
		Name:            name,
		Kind:            kind,
		Status:          WorkUnitStatusPENDING,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}
