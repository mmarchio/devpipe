package state

import (
	"testing"

	"github.com/google/uuid"
)

// P6-10 (WBS 6.7, T6.7.2/T6.7.3): the state prompt-template reference rule —
// an empty template is legal only for control-only kinds (CONTROL/TERMINAL);
// action kinds (ACTION/DECISION/VALIDATION) require a template.

func p610State(t *testing.T, kind StateKind, template string) *WorkflowState {
	t.Helper()
	s, err := NewState(StateInput{
		StateMachineID: uuid.New(),
		Key:            "S",
		Name:           "S",
		Kind:           kind,
		PromptTemplate: template,
	})
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}
	return s
}

// T6.7.1 (domain half) — A supplied template key is stored on the state.
func TestP610StateStoresTemplate(t *testing.T) {
	s := p610State(t, KindAction, "plan_phase")
	if s.PromptTemplate != "PLAN_PHASE" {
		t.Fatalf("PromptTemplate = %q, want normalized PLAN_PHASE", s.PromptTemplate)
	}
}

// T6.7.2 — An empty template is allowed for control-only kinds.
func TestP610ControlOnlyEmptyTemplateAllowed(t *testing.T) {
	for _, kind := range []StateKind{KindControl, KindTerminal} {
		s := p610State(t, kind, "")
		if err := s.ValidateTemplate(); err != nil {
			t.Errorf("kind %s empty template: %v, want nil", kind, err)
		}
	}
}

// T6.7.3 — An action-kind state without a template fails the check.
func TestP610ActionKindEmptyTemplateFails(t *testing.T) {
	for _, kind := range []StateKind{KindAction, KindDecision, KindValidation} {
		s := p610State(t, kind, "")
		if err := s.ValidateTemplate(); err == nil {
			t.Errorf("kind %s empty template: nil, want validation failure", kind)
		}
	}
}

// A non-empty template is legal for every kind.
func TestP610NonEmptyTemplateAlwaysAllowed(t *testing.T) {
	for _, kind := range []StateKind{KindAction, KindDecision, KindValidation, KindControl, KindTerminal} {
		s := p610State(t, kind, "PLAN_PHASE")
		if err := s.ValidateTemplate(); err != nil {
			t.Errorf("kind %s template PLAN_PHASE: %v, want nil", kind, err)
		}
	}
}
