package state

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Machine is the top-level state-machine definition (WBS 6.1). A project may
// retain historical machine versions, but only one version is active for new
// execution at a time (activation is a persistence concern, P6-05+).
type Machine struct {
	ID        uuid.UUID
	ProjectID uuid.UUID

	Name        string
	Description string

	Version int
	Enabled bool

	InitialStateID uuid.UUID

	CreatedAt time.Time
	UpdatedAt time.Time
}

// MachineInput carries the caller-supplied fields for NewMachine. Version,
// timestamps, and ID are assigned by the domain; Enabled defaults to true
// unless an explicit value is supplied via Enabled (nil means "not
// specified").
type MachineInput struct {
	ProjectID   uuid.UUID
	Name        string
	Description string
	Enabled     *bool
}

// NewMachine validates and constructs a new state machine (WBS 6.1): project
// association required (zero ProjectID rejected, T6.1.2/T6.1.6), non-empty
// name (T6.1.3), initial Version 1 (T6.1.4), Enabled true by default and
// persisted on the value (T6.1.5), timestamps set (T6.1.1).
func NewMachine(in MachineInput) (*Machine, error) {
	return newMachine(in, time.Now)
}

func newMachine(in MachineInput, now func() time.Time) (*Machine, error) {
	if in.ProjectID == uuid.Nil {
		return nil, fmt.Errorf("state: project association is required")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("state: name must not be empty")
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	ts := now().UTC()
	return &Machine{
		ID:          uuid.New(),
		ProjectID:   in.ProjectID,
		Name:        name,
		Description: in.Description,
		Version:     1,
		Enabled:     enabled,
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}, nil
}
