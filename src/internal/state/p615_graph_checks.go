// P6-15 (WBS 6.16 / 6.17 / 6.18): graph checks over a machine's states and
// transitions — reachability from the initial state, terminal-state
// validation, and zero-progress automatic cycle detection. Pure functions
// over declared inputs — no DB access so the architecture guard stays
// green.
package state

import (
	"sort"

	"github.com/google/uuid"
)

// ReachabilityReport is the result of CheckReachability (WBS 6.16). Valid is
// true when every enabled state is reachable from the initial state over
// enabled transitions; OrphanStates lists every enabled state with no
// incoming path (deterministic order: sorted by key, then ID).
type ReachabilityReport struct {
	Valid        bool
	OrphanStates []*WorkflowState
}

// CheckReachability validates that enabled states are reachable from the
// initial state over ENABLED transitions (WBS 6.16).
//
// Policy (T6.16.3, recorded per the task): disabled transitions are EXCLUDED
// from the reachability graph, so an enabled state reachable only through a
// disabled edge is flagged as an orphan; disabled states are excluded from
// the check entirely (they are inert and never reported). The initial state
// is reachable by definition (T6.16.4) — it seeds the reachability walk and
// never appears in the report.
func CheckReachability(machine *Machine, states []*WorkflowState, transitions []*StateTransition) ReachabilityReport {
	rep := ReachabilityReport{Valid: true}
	if machine == nil {
		return rep
	}
	adj := make(map[uuid.UUID][]uuid.UUID)
	for _, tr := range transitions {
		if tr == nil || !tr.Enabled {
			continue
		}
		adj[tr.FromStateID] = append(adj[tr.FromStateID], tr.ToStateID)
	}
	seen := map[uuid.UUID]bool{}
	stack := []uuid.UUID{machine.InitialStateID}
	seen[machine.InitialStateID] = true
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, next := range adj[top] {
			if !seen[next] {
				seen[next] = true
				stack = append(stack, next)
			}
		}
	}
	for _, s := range states {
		if s == nil || !s.Enabled {
			continue
		}
		if s.ID == machine.InitialStateID || seen[s.ID] {
			continue
		}
		rep.Valid = false
		rep.OrphanStates = append(rep.OrphanStates, s)
	}
	sortStatesByKey(rep.OrphanStates)
	return rep
}

// TerminalReport is the result of CheckTerminalStates (WBS 6.17). Valid is
// true when no terminal state has an outgoing enabled transition and no
// nonterminal enabled state is a dead-end. Both offender lists are in
// deterministic order (sorted by key, then ID).
type TerminalReport struct {
	Valid                bool
	TerminalWithOutgoing []*WorkflowState
	DeadEndStates        []*WorkflowState
}

// CheckTerminalStates validates terminal-state semantics (WBS 6.17):
//
//   - a terminal state with NO outgoing enabled transition is valid
//     (T6.17.1);
//   - a terminal state WITH an outgoing enabled transition is rejected and
//     reported (T6.17.2); disabled outgoing edges are excluded;
//   - a NON-terminal enabled state with no outgoing enabled transition is a
//     dead-end and is detected (T6.17.3); disabled states are exempt.
func CheckTerminalStates(machine *Machine, states []*WorkflowState, transitions []*StateTransition) TerminalReport {
	rep := TerminalReport{Valid: true}
	outgoing := map[uuid.UUID]bool{}
	for _, tr := range transitions {
		if tr == nil || !tr.Enabled {
			continue
		}
		outgoing[tr.FromStateID] = true
	}
	for _, s := range states {
		if s == nil || !s.Enabled {
			continue
		}
		if s.Terminal {
			if outgoing[s.ID] {
				rep.Valid = false
				rep.TerminalWithOutgoing = append(rep.TerminalWithOutgoing, s)
			}
			continue
		}
		if !outgoing[s.ID] {
			rep.Valid = false
			rep.DeadEndStates = append(rep.DeadEndStates, s)
		}
	}
	sortStatesByKey(rep.TerminalWithOutgoing)
	sortStatesByKey(rep.DeadEndStates)
	return rep
}

// ZeroProgressCycle is one detected cycle (WBS 6.18.4): the involved states
// in deterministic order (sorted by key, then ID).
type ZeroProgressCycle struct {
	States []*WorkflowState
}

// CycleReport is the result of DetectZeroProgressCycles (WBS 6.18). Valid is
// true when no zero-progress control cycle exists; Cycles lists every
// detected cycle (deterministic order: each cycle's state set sorted by key,
// then ID; cycles ordered by their first state's ID).
type CycleReport struct {
	Valid  bool
	Cycles []ZeroProgressCycle
}

// DetectZeroProgressCycles finds cycles that consist ONLY of control-only
// states and can loop without external progress (WBS 6.18).
//
// Classification rule (recorded per the task): a cycle is zero-progress when
// EVERY state in it has Kind CONTROL — automatic control states carry no
// worker execution, so a loop among them can spin forever without progress.
// A cycle involving at least one state of a non-CONTROL kind (ACTION,
// DECISION, VALIDATION) is an intentional execution loop and is allowed
// (T6.18.2). A cycle whose transitions carry a condition is likewise allowed:
// the condition is a context/progress guard, so continuation depends on
// external progress (T6.18.3). Self-loops are cycles: a CONTROL self-loop is
// zero-progress and rejected; a non-CONTROL self-loop is an execution loop
// and allowed.
//
// Detection: iterative DFS over the graph restricted to CONTROL-kind states
// and enabled, condition-free transitions; a back-edge delimits the
// offending cycle and the involved states are reported (T6.18.4).
func DetectZeroProgressCycles(machine *Machine, states []*WorkflowState, transitions []*StateTransition) CycleReport {
	rep := CycleReport{Valid: true}
	kind := map[uuid.UUID]StateKind{}
	for _, s := range states {
		if s == nil {
			continue
		}
		kind[s.ID] = s.Kind
	}
	adj := make(map[uuid.UUID][]uuid.UUID)
	for _, tr := range transitions {
		if tr == nil || !tr.Enabled || tr.HasCondition {
			continue
		}
		if kind[tr.FromStateID] != KindControl || kind[tr.ToStateID] != KindControl {
			continue
		}
		adj[tr.FromStateID] = append(adj[tr.FromStateID], tr.ToStateID)
	}
	var nodes []uuid.UUID
	for id := range adj {
		if !containsUUID(nodes, id) {
			nodes = append(nodes, id)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].String() < nodes[j].String() })

	reported := map[uuid.UUID]bool{}
	for _, root := range nodes {
		if reported[root] {
			continue
		}
		onStack := map[uuid.UUID]int{}
		seen := map[uuid.UUID]bool{}
		var stack []uuid.UUID
		var cycle []uuid.UUID
		var dfs func(n uuid.UUID)
		dfs = func(n uuid.UUID) {
			seen[n] = true
			onStack[n] = len(stack)
			stack = append(stack, n)
			out := make([]uuid.UUID, len(adj[n]))
			copy(out, adj[n])
			sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
			for _, m := range out {
				if _, active := onStack[m]; active {
					cycle = append(cycle, m)
					for i := onStack[m] + 1; i < len(stack); i++ {
						cycle = append(cycle, stack[i])
					}
					return
				}
				if !seen[m] {
					dfs(m)
					if len(cycle) > 0 {
						return
					}
				}
			}
			stack = stack[:len(stack)-1]
			delete(onStack, n)
		}
		dfs(root)
		if len(cycle) == 0 {
			continue
		}
		seenInCycle := map[uuid.UUID]bool{}
		for _, id := range cycle {
			seenInCycle[id] = true
		}
		var cstates []*WorkflowState
		for _, s := range states {
			if s != nil && seenInCycle[s.ID] {
				cstates = append(cstates, s)
			}
		}
		sortStatesByKey(cstates)
		for _, s := range cstates {
			reported[s.ID] = true
		}
		rep.Valid = false
		rep.Cycles = append(rep.Cycles, ZeroProgressCycle{States: cstates})
	}
	sort.Slice(rep.Cycles, func(i, j int) bool {
		return rep.Cycles[i].States[0].ID.String() < rep.Cycles[j].States[0].ID.String()
	})
	return rep
}

// sortStatesByKey orders a state slice deterministically (key, then ID).
func sortStatesByKey(ss []*WorkflowState) {
	sort.Slice(ss, func(i, j int) bool {
		if ss[i].Key != ss[j].Key {
			return ss[i].Key < ss[j].Key
		}
		return ss[i].ID.String() < ss[j].ID.String()
	})
}

func containsUUID(list []uuid.UUID, id uuid.UUID) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}
