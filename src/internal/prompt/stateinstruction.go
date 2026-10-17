package prompt

import (
	"overseer/internal/state"
)

// ComposeStateInstruction resolves the exact state-instruction version
// captured in the StateMachineSnapshot for one state (WBS 8.12). The
// state's StateSnapshot carries the captured instruction version and
// content hash (Phase 6, WBS 6.22 — the snapshot is the read model and
// carries no content, so the caller supplies the instruction content at
// that pinned version); composition reads only those captured values,
// so a later instruction edit is invisible (there is no
// "latest-version" input to this function) and the source hash is
// retained on the resulting STATE_INSTRUCTION component.
//
// A state without a captured instruction (zero version — the normal case
// for control-only states such as EVALUATE) composes to zero
// STATE_INSTRUCTION components: no failure and no implicit fallback to
// any other state's instruction (the WBS 8.12 choice, stated in the
// T8.12.4 test).
func ComposeStateInstruction(snap state.StateMachineSnapshot, stateKey, instructionContent string) (Components, error) {
	var st *state.StateSnapshot
	for i := range snap.States {
		if snap.States[i].Key == stateKey {
			st = &snap.States[i]
			break
		}
	}
	if st == nil || st.InstructionVersion <= 0 {
		return Components{}, nil
	}
	c, err := NewPromptComponent(
		ComponentStateInstruction,
		SourceStateInstruction,
		0,
		instructionContent,
		snap.MachineID,
		st.InstructionVersion,
		st.InstructionHash,
	)
	if err != nil {
		return nil, err
	}
	return Components{c}, nil
}
