package phase

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PhaseStatus is the domain-typed lifecycle state of a phase.
type PhaseStatus string

const (
	StatusPENDING    PhaseStatus = "PENDING"
	StatusPLANNING   PhaseStatus = "PLANNING"
	StatusREADY      PhaseStatus = "READY"
	StatusEXECUTING  PhaseStatus = "EXECUTING"
	StatusVALIDATING PhaseStatus = "VALIDATING"
	StatusDONE       PhaseStatus = "DONE"
	StatusFAILED     PhaseStatus = "FAILED"
)

// validStatuses is the closed set of phase statuses.
var validStatuses = map[PhaseStatus]bool{
	StatusPENDING:    true,
	StatusPLANNING:   true,
	StatusREADY:      true,
	StatusEXECUTING:  true,
	StatusVALIDATING: true,
	StatusDONE:       true,
	StatusFAILED:     true,
}

// String returns the canonical status token.
func (s PhaseStatus) String() string { return string(s) }

// ParseStatus converts a token into a PhaseStatus, rejecting unknown values
// (T2.5.4).
func ParseStatus(token string) (PhaseStatus, error) {
	s := PhaseStatus(strings.ToUpper(strings.TrimSpace(token)))
	if !validStatuses[s] {
		return "", fmt.Errorf("phase: unknown status %q", token)
	}
	return s, nil
}

// Phase is the ordered execution stage of a project (WBS 2.5). Order is
// assigned at creation and is immutable; StartedAt/CompletedAt are nil
// until the corresponding transition occurs.
type Phase struct {
	ID        uuid.UUID
	ProjectID uuid.UUID

	Order int

	Name        string
	Description string

	Status PhaseStatus

	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time

	// FailureReason/FailedAt are set once the phase is marked failed
	// (WBS 4.23) and retained for later orchestration/auditing; nil
	// until the failure transition occurs.
	FailureReason *string
	FailedAt      *time.Time
}

// New constructs a valid phase (T2.5.1): unique ID, UTC CreatedAt, and the
// PENDING initial status (T2.5.5). It requires a non-zero project ID
// (T2.5.2) and a positive order (T2.5.3).
func New(projectID uuid.UUID, order int, name, description string) (*Phase, error) {
	if projectID == uuid.Nil {
		return nil, fmt.Errorf("phase: requires a project ID")
	}
	if order <= 0 {
		return nil, fmt.Errorf("phase: order must be positive, got %d", order)
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("phase: name must not be empty")
	}
	now := time.Now().UTC()
	return &Phase{
		ID:          uuid.New(),
		ProjectID:   projectID,
		Order:       order,
		Name:        name,
		Description: description,
		Status:      StatusPENDING,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// MarkStarted records the start transition: status becomes EXECUTING and
// StartedAt is set in UTC (T2.5.6). It is idempotent-safe: starting an
// already-started phase keeps the original timestamp.
func (p *Phase) MarkStarted() error {
	if p.StartedAt == nil {
		now := time.Now().UTC()
		p.StartedAt = &now
	}
	p.Status = StatusEXECUTING
	return nil
}

// MarkCompleted records the completion transition: status becomes DONE and
// CompletedAt is set in UTC (T2.5.6).
func (p *Phase) MarkCompleted() error {
	now := time.Now().UTC()
	p.CompletedAt = &now
	p.Status = StatusDONE
	return nil
}

// MarkFailed records the failure transition (WBS 4.23, T4.23.1-T4.23.5):
// status becomes FAILED, the failure reason is stored, and FailedAt is set
// once in UTC. The reason must not be empty.
func (p *Phase) MarkFailed(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("phase: failure reason must not be empty")
	}
	if p.FailedAt == nil {
		now := time.Now().UTC()
		p.FailedAt = &now
	}
	r := reason
	p.FailureReason = &r
	p.Status = StatusFAILED
	return nil
}
