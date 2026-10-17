package prompt

// canonicalRank is the WBS 8.9 canonical composition order (11 positions)
// projected onto the P8-06 accepted component-type set. WBS positions 6
// (Architecture Context) and 10 (State-specific Prompt) have no type in the
// accepted set, so the known types occupy the remaining positions with the
// same relative order:
//
//	1  System Prompt              -> ComponentSystem
//	2  Role Instructions          -> ComponentRoleInstruction
//	3  State Instructions         -> ComponentStateInstruction
//	4  Project Context            -> ComponentProjectContext
//	5  Phase Context              -> ComponentPhaseContext
//	7  Task Context               -> ComponentTaskContext
//	8  Test Context               -> ComponentTestContext
//	9  Continuation Context       -> ComponentContinuationContext
//	11 Response Protocol          -> ComponentResponseProtocol
//
// The exact layout can evolve through template versions (WBS 8.9); this
// table is the fixed deterministic rank used by CanonicalOrder.
var canonicalRank = map[PromptComponentType]int{
	ComponentSystem:              1,
	ComponentRoleInstruction:     2,
	ComponentStateInstruction:    3,
	ComponentProjectContext:      4,
	ComponentPhaseContext:        5,
	ComponentTaskContext:         7,
	ComponentTestContext:         8,
	ComponentContinuationContext: 9,
	ComponentResponseProtocol:    11,
}

// CanonicalOrder returns the components arranged in the WBS 8.9 canonical
// composition order: components are grouped by their canonical position and
// components within the same type keep their input (configured) relative
// order, so identical inputs always produce a byte-identical output
// (T8.9.1-T8.9.6). Absent component types are simply absent — the order is
// over positions, not a fixed content list. The input slice is not mutated.
func CanonicalOrder(in Components) Components {
	out := make(Components, 0, len(in))
	// Stable insertion by rank: for rank 1..11, append same-type components
	// in the order they appear in the input.
	for rank := 1; rank <= 11; rank++ {
		for i := 0; i < len(in); i++ {
			if canonicalRank[in[i].Type] == rank {
				out = append(out, in[i])
			}
		}
	}
	return out
}
