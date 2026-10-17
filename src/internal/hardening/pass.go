package hardening

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PassStatus is the lifecycle state of a hardening pass (WBS 16.88).
type PassStatus string

// Pass statuses.
const (
	PassPending          PassStatus = "PENDING"
	PassAnalyzing        PassStatus = "ANALYZING"
	PassPlanning         PassStatus = "PLANNING"
	PassExecuting        PassStatus = "EXECUTING"
	PassValidating       PassStatus = "VALIDATING"
	PassDone             PassStatus = "DONE"
	PassFailed           PassStatus = "FAILED"
	PassRecoveryRequired PassStatus = "RECOVERY_REQUIRED"
)

var allPassStatuses = []PassStatus{PassPending, PassAnalyzing, PassPlanning, PassExecuting, PassValidating, PassDone, PassFailed, PassRecoveryRequired}

// AllPassStatuses returns every status in declaration order.
func AllPassStatuses() []PassStatus { return append([]PassStatus(nil), allPassStatuses...) }

// ParsePassStatus accepts exactly one canonical status.
func ParsePassStatus(s string) (PassStatus, error) {
	for _, p := range allPassStatuses {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("hardening: invalid pass status %q", s)
}

// IsTerminal reports DONE / FAILED.
func (s PassStatus) IsTerminal() bool { return s == PassDone || s == PassFailed }

// ErrIllegalPassTransition reports a disallowed pass edge.
var ErrIllegalPassTransition = errors.New("hardening: illegal pass transition")

// passEdges: analysis → planning → executing → validating, then a rescan
// returns to ANALYZING (next round). Any non-terminal status may require
// recovery or fail; recovery resumes at the interrupted status.
var passEdges = map[PassStatus][]PassStatus{
	PassPending:          {PassAnalyzing, PassDone},
	PassAnalyzing:        {PassPlanning, PassDone},
	PassPlanning:         {PassExecuting, PassDone},
	PassExecuting:        {PassValidating, PassAnalyzing},
	PassValidating:       {PassAnalyzing, PassExecuting, PassDone},
	PassRecoveryRequired: {PassPending, PassAnalyzing, PassPlanning, PassExecuting, PassValidating},
}

// CanTransition reports whether from → to is allowed.
func CanTransition(from, to PassStatus) bool {
	if !from.IsTerminal() && from != PassRecoveryRequired && (to == PassRecoveryRequired || to == PassFailed) {
		return true
	}
	for _, t := range passEdges[from] {
		if t == to {
			return true
		}
	}
	return false
}

// ValidateTransition returns ErrIllegalPassTransition for a bad edge.
func ValidateTransition(from, to PassStatus) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalPassTransition, from, to)
	}
	return nil
}

// Pass is one hardening pass of one finalization generation (WBS 16.88).
// Exactly one non-terminal pass exists per execution and generation
// (WBS 16.89).
type Pass struct {
	ID                   uuid.UUID
	ExecutionID          uuid.UUID
	ProjectID            uuid.UUID
	BaselineCheckpointID *uuid.UUID
	Generation           int
	Status               PassStatus
	// ResumeStatus is the status interrupted by RECOVERY_REQUIRED.
	ResumeStatus *PassStatus
	Round        int
	Version      int
	StartedAt    time.Time
	CompletedAt  *time.Time
}

// NewPass creates a PENDING pass at round 1.
func NewPass(executionID, projectID uuid.UUID, generation int, now time.Time) (*Pass, error) {
	if executionID == uuid.Nil || projectID == uuid.Nil {
		return nil, fmt.Errorf("hardening: pass requires execution and project")
	}
	if generation < 1 {
		return nil, fmt.Errorf("hardening: generation must be ≥ 1")
	}
	return &Pass{ID: uuid.New(), ExecutionID: executionID, ProjectID: projectID, Generation: generation,
		Status: PassPending, Round: 1, Version: 1, StartedAt: now.UTC()}, nil
}
