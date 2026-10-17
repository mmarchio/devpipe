package wbs

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ExecutionPlanStatus is the domain-typed state of a generated execution
// plan.
type ExecutionPlanStatus string

const (
	PlanStatusGENERATING ExecutionPlanStatus = "GENERATING"
	StatusREADY          ExecutionPlanStatus = "READY"
	PlanStatusEXECUTING  ExecutionPlanStatus = "EXECUTING"
	PlanStatusCOMPLETED  ExecutionPlanStatus = "COMPLETED"
	PlanStatusSUPERSEDED ExecutionPlanStatus = "SUPERSEDED"
	PlanStatusFAILED     ExecutionPlanStatus = "FAILED"
)

// planStatuses is the closed set of execution-plan statuses.
var planStatuses = map[ExecutionPlanStatus]bool{
	PlanStatusGENERATING: true,
	StatusREADY:          true,
	PlanStatusEXECUTING:  true,
	PlanStatusCOMPLETED:  true,
	PlanStatusSUPERSEDED: true,
	PlanStatusFAILED:     true,
}

// String returns the canonical status token.
func (s ExecutionPlanStatus) String() string { return string(s) }

// ParsePlanStatus converts a token into an ExecutionPlanStatus, rejecting
// unknown values (T2.11.4).
func ParsePlanStatus(token string) (ExecutionPlanStatus, error) {
	s := ExecutionPlanStatus(strings.ToUpper(strings.TrimSpace(token)))
	if !planStatuses[s] {
		return "", fmt.Errorf("wbs: unknown execution plan status %q", token)
	}
	return s, nil
}

// ExecutionPlan is the generated implementation decomposition for a phase
// (WBS 2.11). It records the WBS version it was derived from, so a later
// WBS revision never silently mutates a historical plan (WBS 2.20). Phase 2
// only persists plans; generation happens in later phases.
type ExecutionPlan struct {
	ID      uuid.UUID
	PhaseID uuid.UUID

	WBSVersion int
	Version    int

	Status ExecutionPlanStatus

	// Phase 16 (P16-58): INITIAL or AMENDMENT, the plan an amendment
	// supersedes, and the hash of the source WBS content it was bound to.
	Kind          string
	AmendsPlanID  *uuid.UUID
	SourceWBSHash string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Plan kinds (P16-58).
const (
	PlanKindInitial   = "INITIAL"
	PlanKindAmendment = "AMENDMENT"
)

// NewExecutionPlan constructs a plan at version 1 in the READY state
// (T2.11.2/T2.11.3). It requires a non-zero phase ID and a positive source
// WBS version (T2.11.1).
func NewExecutionPlan(phaseID uuid.UUID, wbsVersion int) (*ExecutionPlan, error) {
	if phaseID == uuid.Nil {
		return nil, fmt.Errorf("wbs: plan requires a phase ID")
	}
	if wbsVersion <= 0 {
		return nil, fmt.Errorf("wbs: plan requires a positive source WBS version, got %d", wbsVersion)
	}
	now := time.Now().UTC()
	return &ExecutionPlan{
		ID:         uuid.New(),
		PhaseID:    phaseID,
		WBSVersion: wbsVersion,
		Version:    1,
		Status:     StatusREADY,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}
