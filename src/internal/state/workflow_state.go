package state

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// StateKind classifies a workflow state by its execution semantics (WBS
// 6.2). Kinds are not roles: "IMPLEMENT" is a state kind, "ENGINEER" is a
// role.
type StateKind string

// The initial state-kind set (WBS 6.2).
const (
	KindAction     StateKind = "ACTION"
	KindDecision   StateKind = "DECISION"
	KindValidation StateKind = "VALIDATION"
	KindControl    StateKind = "CONTROL"
	KindTerminal   StateKind = "TERMINAL"
)

// Known reports whether k is one of the initial state kinds (T6.2.5).
func (k StateKind) Known() bool {
	switch k {
	case KindAction, KindDecision, KindValidation, KindControl, KindTerminal:
		return true
	}
	return false
}

// WorkflowState is a single configurable state within a state machine (WBS
// 6.2). Pure domain: no persistence.
type WorkflowState struct {
	ID             uuid.UUID
	StateMachineID uuid.UUID

	Key         string
	Name        string
	Description string

	// PromptTemplate is the referenced prompt-template key (WBS 6.7).
	// Stored, not composed: composition lands in a later phase.
	PromptTemplate string

	Kind StateKind

	Terminal bool
	Enabled  bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateInput carries the caller-supplied fields for NewState. ID and
// timestamps are assigned by the domain; Key is normalized (T6.2.2); Enabled
// defaults to true unless an explicit value is supplied (nil means "not
// specified").
type StateInput struct {
	StateMachineID uuid.UUID
	Key            string
	Name           string
	Description    string
	PromptTemplate string
	Kind           StateKind
	Terminal       bool
	Enabled        *bool
}

// NewState validates and constructs a new workflow state (WBS 6.2): machine
// association required, normalized non-empty key (T6.2.2), non-empty name,
// known kind (T6.2.5), terminal flag persisted on the value (T6.2.6),
// Enabled true by default, timestamps set (T6.2.1).
func NewState(in StateInput) (*WorkflowState, error) {
	return newState(in, time.Now)
}

func newState(in StateInput, now func() time.Time) (*WorkflowState, error) {
	if in.StateMachineID == uuid.Nil {
		return nil, fmt.Errorf("state: machine association is required")
	}
	key := NormalizeKey(in.Key)
	if key == "" {
		return nil, fmt.Errorf("state: key must not be empty")
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("state: name must not be empty")
	}
	if !in.Kind.Known() {
		return nil, fmt.Errorf("state: unknown kind %q", in.Kind)
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	ts := now().UTC()
	return &WorkflowState{
		ID:             uuid.New(),
		StateMachineID: in.StateMachineID,
		Key:            key,
		Name:           strings.TrimSpace(in.Name),
		Description:    in.Description,
		PromptTemplate: NormalizeKey(in.PromptTemplate),
		Kind:           in.Kind,
		Terminal:       in.Terminal,
		Enabled:        enabled,
		CreatedAt:      ts,
		UpdatedAt:      ts,
	}, nil
}

// NormalizeKey applies the same key-normalization policy as the role domain:
// trim surrounding whitespace, then uppercase.
func NormalizeKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// ExistingKeyInScope reports an error when a candidate state collides with an
// existing state's scoped key (T6.2.3): same normalized key AND same state
// machine. The same key in a different machine is allowed (T6.2.4).
func ExistingKeyInScope(existing *WorkflowState, candidate StateInput) error {
	if existing == nil {
		return nil
	}
	if NormalizeKey(candidate.Key) != existing.Key {
		return nil
	}
	if candidate.StateMachineID == existing.StateMachineID {
		return fmt.Errorf("state: key %q already exists in machine %s", existing.Key, existing.StateMachineID)
	}
	return nil
}

// ValidateTemplate applies the WBS 6.7 template-reference rule: an empty
// prompt-template reference is legal only for control-only kinds (CONTROL and
// TERMINAL); action kinds (ACTION, DECISION, VALIDATION) must carry a
// template. Pure domain check, consumable by the validation service (P6-16).
func (s *WorkflowState) ValidateTemplate() error {
	if s == nil {
		return nil
	}
	if s.PromptTemplate != "" {
		return nil
	}
	if s.Kind == KindControl || s.Kind == KindTerminal {
		return nil
	}
	return fmt.Errorf("state: kind %s requires a prompt-template reference", s.Kind)
}
