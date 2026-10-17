// P6-29 (WBS 6.39): the state-machine visualization read model — the
// read model suitable for the future web state-machine editor. Every
// state of the current machine is a NODE; every transition is an EDGE.
package state

import "github.com/google/uuid"

// VizRole is the node role section (WBS 6.39): the assigned role's key
// plus the CURRENT version of its system prompt from the
// role_system_prompts append-only history (0 when the role has none).
// A state with several role assignments carries several roles, in
// ordinal order.
type VizRole struct {
	Key                 string
	SystemPromptVersion int
}

// VizNode is one state of the current machine as a visualization node
// (WBS 6.39): STATE, ROLE, SYSTEM PROMPT VERSION, STATE INSTRUCTION
// VERSION, STATE FILE REQUIREMENTS, PROMPT TEMPLATE, TERMINAL?. Absent
// data is a nil/empty section, never an error (a state with no roles,
// no instruction, no file requirements has empty sections and a nil
// instruction version).
type VizNode struct {
	Key            string
	Terminal       bool
	PromptTemplate string
	Roles          []VizRole
	// InstructionVersion is the state's current state-instruction
	// version from the instruction_versions history; nil when the state
	// has no instruction.
	InstructionVersion *int
	StateFiles         []string
}

// VizEdgeCondition is the edge CONDITION section (WBS 6.39), the
// structured condition shape (field / operator / value).
type VizEdgeCondition struct {
	Field        string
	Operator     string
	Literal      any
	ContextField string
}

// VizEdge is one transition of the current machine as a visualization
// edge (WBS 6.39): OUTCOME, CONDITION, PRIORITY, DESTINATION —
// addressed by state key, never by row id.
type VizEdge struct {
	From        string
	Outcome     string
	Condition   *VizEdgeCondition
	Priority    int
	Destination string
}

// StateMachineVisualization is the deterministic read model of the
// CURRENT machine (WBS 6.39): all of its states as nodes (node order:
// state key ascending) and all of its transitions as edges (edge
// order: outcome, then priority, then from key, then destination key —
// byte-stable across repeated reads).
type StateMachineVisualization struct {
	MachineID uuid.UUID
	Version   int
	Nodes     []VizNode
	Edges     []VizEdge
}
