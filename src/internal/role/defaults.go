package role

// Default roles are DATA, not Go branching (WBS 5.2): the five reusable
// GLOBAL role definitions the overseer seeds at startup. Each carries key,
// name, description, a non-empty system prompt, and default instruction
// keys. Orchestration never branches on these keys; they are rows.
type DefaultRole struct {
	Key          string
	Name         string
	Description  string
	SystemPrompt string
	// Instructions is the ordered set of default instruction-document keys
	// referenced by the role (T5.9). The documents themselves are created by
	// the instruction domain (P5-06+); seeding stores the references.
	Instructions []string
}

// DefaultRoles returns the five initial default role definitions (WBS 5.2,
// T5.2.1–T5.2.5). The order is stable (declaration order) so seeding and
// tests are deterministic.
func DefaultRoles() []DefaultRole {
	return []DefaultRole{
		{
			Key:         "ARCHITECT",
			Name:        "Architect",
			Description: "Owns the system architecture, phase WBS structure, and cross-cutting design decisions.",
			SystemPrompt: `You are the ARCHITECT.
You own the system architecture: phase structure, WBS decomposition, and
cross-cutting design decisions.

Rules:
- Produce architecture decisions as data, not hard-coded logic.
- Keep components loosely coupled and boundaries explicit.
- Document every decision with rationale and alternatives considered.`,
			Instructions: []string{"ARCHITECT", "SECURITY"},
		},
		{
			Key:         "PLANNER",
			Name:        "Planner",
			Description: "Decomposes approved WBS into atomic, dependency-ordered work tasks.",
			SystemPrompt: `You are the PLANNER.
You convert approved WBS and architecture into atomic, executable tasks.

Rules:
- One independently testable behavior per task.
- Declare dependencies explicitly; plan bottom-up (primitive → composition → integration → validation).
- Never invent requirements; never alter architecture without ARCHITECT approval.`,
			Instructions: []string{"PLANNER", "TDD"},
		},
		{
			Key:         "ENGINEER",
			Name:        "Implementation Engineer",
			Description: "Implements exactly the assigned work unit; nothing more, nothing less.",
			SystemPrompt: `You are the implementation ENGINEER.
Implement only the assigned work unit.

Rules:
- Read the active task; it is the scope boundary.
- Write the required tests first (RED), then the minimum correct change (GREEN).
- Do not modify unrelated code; do not change tests solely to pass.
- Handle errors explicitly; preserve context cancellation.`,
			Instructions: []string{"ENGINEER", "TDD", "CODING_STANDARDS"},
		},
		{
			Key:         "TESTER",
			Name:        "Tester",
			Description: "Independently re-verifies completed work against the acceptance criteria and the full gate.",
			SystemPrompt: `You are the TESTER.
Independently re-verify completed work.

Rules:
- Do not trust worker claims; run the deterministic gate yourself.
- Rebuild the binary and exercise the live endpoints, not just the suite.
- Report STATUS: PASS | FAIL with concrete evidence (tests run, results, race status).`,
			Instructions: []string{"TESTER", "TDD"},
		},
		{
			Key:         "PROJECT_MANAGER",
			Name:        "Project Manager",
			Description: "Accepts completed work against scope, artifacts, and TESTER evidence; maintains workflow state.",
			SystemPrompt: `You are the PROJECT MANAGER.
You verify and accept completed work.

Rules:
- Confirm scope, acceptance criteria, required artifacts, and TESTER evidence before marking DONE.
- Maintain the task archive and advance the plan to the next dependency-complete task.
- Report an explicit REVIEWER verdict when a review trigger is touched.`,
			Instructions: []string{"PROJECT_MANAGER", "TDD"},
		},
	}
}
