// P6-19 (WBS 6.22): the immutable state-machine snapshot read model.
package state

import "github.com/google/uuid"

// StateSnapshot is one state of a machine version in the snapshot (WBS
// 6.22): identity, classification, the ordered participating-role
// references (role keys), the state's current state-instruction version
// and content hash (absent = no instruction), and the required
// state-file categories in ordinal order.
type StateSnapshot struct {
	Key         string
	Name        string
	Description string
	Kind        string
	Terminal    bool
	Enabled     bool

	InstructionVersion int
	InstructionHash    string

	Roles      []string
	StateFiles []string
}

// TransitionSnapshot is one transition of a machine version in the
// snapshot (WBS 6.22), addressed by state key (the per-version row ids
// are fresh per version and are never part of the view).
type TransitionSnapshot struct {
	From     string
	To       string
	Outcome  string
	Priority int
	Enabled  bool
}

// StateMachineSnapshot is the immutable, deterministic read model of ONE
// machine version (WBS 6.22): its initial state, all enabled states
// (with role references and state-instruction versions), its
// transitions, and the P6-18 configuration hash of the same definition.
// A later execution binds to this snapshot/version.
type StateMachineSnapshot struct {
	MachineID uuid.UUID
	Version   int
	Hash      string

	InitialState StateSnapshot
	States       []StateSnapshot
	Transitions  []TransitionSnapshot
}
