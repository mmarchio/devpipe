package state

// ResolveStateFileRequirements is the union-dedup resolution helper for
// state-file requirements (WBS 6.6, T6.6.5). It combines the role-level
// requirements (WBS 5.12, in the role's stored position order — first) with
// the state-level requirements (in the state's stored ordinal order) into a
// deduplicated, deterministic union: each category appears once, in
// first-seen order (a role requirement takes precedence over a state one with
// the same category). This is a pure resolution over the two stored lists —
// Phase 6 stores requirements only, and the union is not persisted.
func ResolveStateFileRequirements(roleRequirements, stateRequirements []string) []string {
	out := make([]string, 0, len(roleRequirements)+len(stateRequirements))
	seen := make(map[string]bool, len(roleRequirements)+len(stateRequirements))
	for _, list := range [][]string{roleRequirements, stateRequirements} {
		for _, category := range list {
			if !seen[category] {
				seen[category] = true
				out = append(out, category)
			}
		}
	}
	return out
}
