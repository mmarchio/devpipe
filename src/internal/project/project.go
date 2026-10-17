package project

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProjectStatus is the domain-typed lifecycle state of a project.
// It is represented by its canonical uppercase token so persistence and
// parsing round-trip losslessly.
type ProjectStatus string

const (
	StatusDRAFT     ProjectStatus = "DRAFT"
	StatusREADY     ProjectStatus = "READY"
	StatusRUNNING   ProjectStatus = "RUNNING"
	StatusPAUSED    ProjectStatus = "PAUSED"
	StatusCOMPLETED ProjectStatus = "COMPLETED"
	StatusFAILED    ProjectStatus = "FAILED"
	StatusCANCELLED ProjectStatus = "CANCELLED"
)

// validStatuses is the closed set of project statuses.
var validStatuses = map[ProjectStatus]bool{
	StatusDRAFT:     true,
	StatusREADY:     true,
	StatusRUNNING:   true,
	StatusPAUSED:    true,
	StatusCOMPLETED: true,
	StatusFAILED:    true,
	StatusCANCELLED: true,
}

// String returns the canonical status token.
func (s ProjectStatus) String() string { return string(s) }

// ParseStatus converts a string token into a ProjectStatus, rejecting
// unknown values (T2.1.5).
func ParseStatus(token string) (ProjectStatus, error) {
	s := ProjectStatus(strings.ToUpper(strings.TrimSpace(token)))
	if !validStatuses[s] {
		return "", fmt.Errorf("project: unknown status %q", token)
	}
	return s, nil
}

// Project is the core persistent domain entity (WBS 2.1).
// ID is immutable after creation; Name and Description are mutable.
type Project struct {
	ID          uuid.UUID
	Name        string
	Description string
	Status      ProjectStatus

	// Version is the optimistic-concurrency counter (WBS 3.24): it
	// increments on every modification and must be echoed back by
	// clients on mutating requests.
	Version int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// New constructs a valid project: unique ID, UTC timestamps, and the DRAFT
// initial status (T2.1.1, T2.1.4). Empty or whitespace-only names are
// rejected (T2.1.2, T2.1.3).
func New(name, description string) (*Project, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("project: name must not be empty")
	}
	now := time.Now().UTC()
	return &Project{
		ID:          uuid.New(),
		Version:     1,
		Name:        name,
		Description: description,
		Status:      StatusDRAFT,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}
