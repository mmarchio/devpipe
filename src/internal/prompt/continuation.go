package prompt

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ContinuationStatus is the captured worker status that a same-session or
// reset-session continuation prompt is built from (WBS 8.20). It is derived
// later from worker response handling; the model here is the persisted shape
// so a continuation prompt can be reconstructed from persisted state instead
// of conversation history.
type ContinuationStatus struct {
	WorkUnitID uuid.UUID

	Summary string

	Completed []string
	Remaining []string
	Blocking  []string

	NextAction string

	CapturedAt time.Time
}

// ContinuationStatusInput carries the caller-supplied fields for
// NewContinuationStatus.
type ContinuationStatusInput struct {
	WorkUnitID uuid.UUID

	Summary string

	Completed []string
	Remaining []string
	Blocking  []string

	NextAction string

	CapturedAt time.Time
}

// NewContinuationStatus validates and constructs a continuation status (WBS
// 8.20). It must be addressable to a work unit (T8.20.1/T8.20.2): a zero
// WorkUnitID is rejected. The ordered lists are copied so the stored value is
// independent of the caller's backing arrays and retains their order exactly
// (T8.20.3).
func NewContinuationStatus(in ContinuationStatusInput) (*ContinuationStatus, error) {
	if in.WorkUnitID == uuid.Nil {
		return nil, fmt.Errorf("prompt: continuation status must be associated with a work unit")
	}
	return &ContinuationStatus{
		WorkUnitID: in.WorkUnitID,
		Summary:    in.Summary,
		Completed:  copyStrings(in.Completed),
		Remaining:  copyStrings(in.Remaining),
		Blocking:   copyStrings(in.Blocking),
		NextAction: in.NextAction,
		CapturedAt: in.CapturedAt,
	}, nil
}

// AssociatedTo reports whether the status is associated with the given work
// unit (T8.20.2).
func (c *ContinuationStatus) AssociatedTo(workUnitID uuid.UUID) bool {
	return c.WorkUnitID == workUnitID
}

// ValidateAssociation rejects a cross-work-unit association (T8.20.4): a
// continuation status built from one work unit must not be applied to a
// different work unit.
func (c *ContinuationStatus) ValidateAssociation(workUnitID uuid.UUID) error {
	if !c.AssociatedTo(workUnitID) {
		return fmt.Errorf("prompt: continuation status for work unit %s applied to work unit %s", c.WorkUnitID, workUnitID)
	}
	return nil
}

func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
