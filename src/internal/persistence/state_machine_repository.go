package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MachineRecord is the domain-facing view of a stored state machine (WBS
// 6.1). A project may retain historical machine versions; activation is a
// later concern.
type MachineRecord struct {
	ID        uuid.UUID
	ProjectID uuid.UUID

	// FamilyID is the machine family's stable identity (the v1 row's id).
	// Versions of one machine share it; GetVersion/ListVersions address the
	// family (WBS 6.20).
	FamilyID uuid.UUID

	Name        string
	Description string

	Version int
	Enabled bool

	InitialStateID *uuid.UUID

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateMachineRepository is the domain-facing persistence contract for
// state-machine definitions (WBS 6.1).
type StateMachineRepository interface {
	// Create inserts a new machine; a missing project row yields
	// ErrInvalidReference (T6.1.6).
	Create(ctx context.Context, m *MachineRecord) error
	// Get returns the machine with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id uuid.UUID) (*MachineRecord, error)
	// ListByProject lists all of a project's machines, ordered by version.
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]*MachineRecord, error)
	// SetInitialState points the machine's initial_state_id at an existing
	// state (WBS 6.3: the seed sets it after the states are created).
	SetInitialState(ctx context.Context, id uuid.UUID, stateID uuid.UUID) error
	// GetVersion returns the machine row for the given (machine, version)
	// binding (WBS 6.20); unknown versions yield ErrNotFound.
	GetVersion(ctx context.Context, id uuid.UUID, version int) (*MachineRecord, error)
	// ListVersions returns all stored versions of a machine in ascending
	// version order (WBS 6.20).
	ListVersions(ctx context.Context, id uuid.UUID) ([]*MachineRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) StateMachineRepository
}

// WorkflowStateRecord is the domain-facing view of a stored workflow state
// (WBS 6.2).
type WorkflowStateRecord struct {
	ID        uuid.UUID
	MachineID uuid.UUID

	Version int

	Key         string
	Name        string
	Description string

	// PromptTemplate is the referenced prompt-template key (WBS 6.7).
	PromptTemplate string

	Kind string

	Terminal bool
	Enabled  bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// WorkflowStateRepository is the domain-facing persistence contract for
// workflow states (WBS 6.2). A key is unique within one machine (T6.2.3);
// the same key may exist in a different machine (T6.2.4).
type WorkflowStateRepository interface {
	// Create inserts a new state; a duplicate (machine_id, key) yields
	// ErrConflict and a missing machine row yields ErrInvalidReference.
	Create(ctx context.Context, s *WorkflowStateRecord) error
	// Get returns the state with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id uuid.UUID) (*WorkflowStateRecord, error)
	// ListByMachine lists one machine-version's states, ordered by key
	// (WBS 6.20: states are version-scoped).
	ListByMachine(ctx context.Context, machineID uuid.UUID, version int) ([]*WorkflowStateRecord, error)
	// Update mutates the state's editable definition fields (name,
	// description, prompt template, kind, terminal, enabled) — the
	// state-configuration surface (WBS 6.36). Identity fields (machine,
	// version, key) are immutable. ErrNotFound when the row is missing.
	Update(ctx context.Context, rec *WorkflowStateRecord) (*WorkflowStateRecord, error)
	// Delete removes the state row (WBS 6.36 destructive delete); the
	// service layer protects historically referenced states first
	// (T6.36.5). Child rows cascade at the DB level.
	Delete(ctx context.Context, id uuid.UUID) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) WorkflowStateRepository
}

// TransitionCondition is the persisted structured condition of one
// transition (WBS 6.37, T6.37.2): the P6-04 domain shape (field, operator,
// value) with the value being exactly one of a literal or a context-field
// reference.
type TransitionCondition struct {
	Field string

	Operator string

	// Literal is the constant right-hand side (nil when ContextField is
	// set). ContextField is the transition-context field name resolved at
	// evaluation time (empty when Literal is set).
	Literal      any
	ContextField string
}

// StateTransitionRecord is the domain-facing view of a stored state
// transition (WBS 6.8). The outcome is stored as given (the documented case
// policy, T6.9.4); duplicate ambiguity is per (machine, from_state, outcome).
type StateTransitionRecord struct {
	ID        uuid.UUID
	MachineID uuid.UUID

	Version int

	FromStateID uuid.UUID
	ToStateID   uuid.UUID

	Outcome  string
	Priority int

	Enabled bool

	// Condition is the persisted structured condition, or nil when the
	// transition has none (WBS 6.37, T6.37.2).
	Condition *TransitionCondition

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateTransitionRepository is the domain-facing persistence contract for
// state transitions (WBS 6.8). A duplicate (machine, from_state, outcome)
// yields ErrConflict; a transition whose endpoints do not both belong to
// the machine is rejected at the DB level (T6.8.4).
type StateTransitionRepository interface {
	// Create inserts a new transition; a duplicate (machine, from_state,
	// outcome) yields ErrConflict and a same-machine violation yields a
	// translated infrastructure error.
	Create(ctx context.Context, t *StateTransitionRecord) error
	// Get returns the transition with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id uuid.UUID) (*StateTransitionRecord, error)
	// ListByMachine lists one machine-version's transitions, ordered by
	// (from_state, outcome) (WBS 6.20: transitions are version-scoped).
	ListByMachine(ctx context.Context, machineID uuid.UUID, version int) ([]*StateTransitionRecord, error)
	// Update mutates the transition's editable configuration (outcome,
	// priority, enabled) for the transition-configuration surface (WBS
	// 6.37); identity fields (machine, version, from/to state) are
	// immutable. ErrNotFound when the row is missing.
	Update(ctx context.Context, t *StateTransitionRecord) (*StateTransitionRecord, error)
	// ReferencedByStates reports the set of state ids referenced by any
	// transition of the given machine version (from- or to-endpoint)
	// (WBS 6.36, T6.36.5 protection check).
	ReferencedByStates(ctx context.Context, machineID uuid.UUID, version int) (map[uuid.UUID]bool, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) StateTransitionRepository
}

// TransitionConditionRepository persists the structured condition attached
// to a transition (WBS 6.37, T6.37.2): one condition per transition.
type TransitionConditionRepository interface {
	// Create stores the transition's condition, replacing any prior one.
	// A missing transition row yields ErrInvalidReference.
	Create(ctx context.Context, transitionID uuid.UUID, c *TransitionCondition) error
	// Get returns the transition's condition; nil, nil when it has none,
	// ErrNotFound when the transition row is missing.
	Get(ctx context.Context, transitionID uuid.UUID) (*TransitionCondition, bool, error)
	// Remove deletes the transition's condition (clears it).
	Remove(ctx context.Context, transitionID uuid.UUID) error
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) TransitionConditionRepository
}
