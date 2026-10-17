// P6-18 (WBS 6.21): deterministic machine configuration hash.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// StateConfigDef is one state of a machine definition for hashing (WBS
// 6.21): identity, prompt-template reference, kind, enabled flag, the
// current state-instruction version + content hash (absent = no
// instruction), the ordered participating-role list, and the required
// state-file categories in ordinal order.
type StateConfigDef struct {
	Key            string
	Name           string
	Description    string
	PromptTemplate string
	Kind           string
	Terminal       bool
	Enabled        bool

	InstructionVersion int
	InstructionHash    string

	Roles      []string
	StateFiles []string
}

// TransitionConfigDef is one transition of a machine definition for
// hashing (WBS 6.21): endpoints by state key, outcome, priority, and
// enabled flag.
type TransitionConfigDef struct {
	From     string
	To       string
	Outcome  string
	Priority int
	Enabled  bool
}

// MachineDefinition is the full persisted definition of ONE machine
// version, in the form consumed by MachineConfigurationHash (WBS 6.21):
// states (with instructions, role assignments, state-file requirements),
// transitions, and the initial state. It is the semantic content of the
// version — never the row UUIDs, which are fresh per version.
type MachineDefinition struct {
	Name        string
	Description string
	Version     int
	Enabled     bool
	InitialKey  string

	States      []StateConfigDef
	Transitions []TransitionConfigDef
}

// MachineConfigurationHash returns a deterministic hash over the machine
// definition (WBS 6.21): the same definition always yields the same hash
// (T6.21.1); a state-instruction edit (T6.21.2), a transition edit
// (T6.21.3), or a role-assignment change (T6.21.4) changes it; and a
// reordering of semantically equal states/transitions does NOT (T6.21.5)
// — states are canonicalized by key and transitions by
// (from, to, outcome), so the hash is a pure function of the persisted
// content of one version. It will later feed prompt caching and execution
// auditing.
func MachineConfigurationHash(def *MachineDefinition) string {
	var b strings.Builder
	fmt.Fprintf(&b, "machine:%s@%d\n", def.Name, def.Version)
	if def.Description != "" {
		fmt.Fprintf(&b, "description:%s\n", def.Description)
	}
	fmt.Fprintf(&b, "enabled:%t\n", def.Enabled)
	fmt.Fprintf(&b, "initial:%s\n", def.InitialKey)

	states := append([]StateConfigDef(nil), def.States...)
	sort.Slice(states, func(i, j int) bool { return states[i].Key < states[j].Key })
	for _, st := range states {
		fmt.Fprintf(&b, "state:%s:%s:%s:%s:%s:%t:%t\n",
			st.Key, st.Name, st.Description, st.PromptTemplate, st.Kind,
			st.Terminal, st.Enabled)
		if st.InstructionVersion > 0 {
			fmt.Fprintf(&b, "state-instruction:%s@%d:%s\n", st.Key, st.InstructionVersion, st.InstructionHash)
		}
		if len(st.Roles) > 0 {
			fmt.Fprintf(&b, "roles:%s:%v\n", st.Key, st.Roles)
		}
		if len(st.StateFiles) > 0 {
			fmt.Fprintf(&b, "statefiles:%s:%v\n", st.Key, st.StateFiles)
		}
	}

	byKey := make(map[string]StateConfigDef, len(states))
	for _, st := range states {
		byKey[st.Key] = st
	}
	transitions := append([]TransitionConfigDef(nil), def.Transitions...)
	sort.Slice(transitions, func(i, j int) bool {
		a, c := transitions[i], transitions[j]
		if a.From != c.From {
			return a.From < c.From
		}
		if a.To != c.To {
			return a.To < c.To
		}
		return a.Outcome < c.Outcome
	})
	for _, tr := range transitions {
		fmt.Fprintf(&b, "transition:%s->%s:%s:%d:%t\n", tr.From, tr.To, tr.Outcome, tr.Priority, tr.Enabled)
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
