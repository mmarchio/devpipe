package role

import "github.com/google/uuid"

// Classification is the source/type tag for a snapshot component (WBS 5.29,
// the prompt-injection boundary). Phase 5 defines the role-related
// classifications only; task/state classifications land with the later
// phases that introduce them.
type Classification string

// Role-related classifications (WBS 5.29, Phase 5).
const (
	// ClassificationSystem marks the role's system prompt.
	ClassificationSystem Classification = "SYSTEM"
	// ClassificationRoleInstruction marks a role instruction.
	ClassificationRoleInstruction Classification = "ROLE_INSTRUCTION"
	// ClassificationStateInstruction marks a state instruction (WBS 7.36,
	// the classification set the Phase 7 snapshot components use; Phase 5
	// defined the role-related classes, these complete the set).
	ClassificationStateInstruction Classification = "STATE_INSTRUCTION"
	// ClassificationProjectContext marks project-scoped state-file context.
	ClassificationProjectContext Classification = "PROJECT_CONTEXT"
	// ClassificationPhaseContext marks phase-scoped state-file context.
	ClassificationPhaseContext Classification = "PHASE_CONTEXT"
	// ClassificationTaskContext marks task (active-work-unit) context.
	ClassificationTaskContext Classification = "TASK_CONTEXT"
	// ClassificationTestContext marks test-state context.
	ClassificationTestContext Classification = "TEST_CONTEXT"
)

// PromptSnapshot is the immutable read-model view of a role's system prompt
// at an exact version (WBS 5.14).
type PromptSnapshot struct {
	Version        int
	Content        string
	ContentHash    string
	Classification Classification
}

// InstructionSnapshot is the immutable read-model view of one assigned
// instruction at its resolved version, in assignment order (WBS 5.14/5.29).
type InstructionSnapshot struct {
	Key            string
	Name           string
	Version        int
	Content        string
	ContentHash    string
	Classification Classification
}

// RoleSnapshot is the immutable read model representing exactly what later
// prompt composition needs (WBS 5.14): the role, its exact system-prompt
// version, its assigned instructions in order, its allowed states, and its
// state-file requirements.
type RoleSnapshot struct {
	RoleID      uuid.UUID
	RoleKey     string
	RoleVersion int

	SystemPrompt PromptSnapshot
	Instructions []InstructionSnapshot

	AllowedStates []string
	StateFiles    []string
}
