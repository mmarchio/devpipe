package wbs

import (
	"sort"

	"github.com/google/uuid"
)

// ErrDependencyCycle is returned by CheckDependencyGraph when the dependency
// graph contains a cycle (WBS 2.15). Path lists the cycle's units so that
// Path[i] depends on Path[i+1] (with wrap-around: the last depends on the first).
type ErrDependencyCycle struct {
	Path []uuid.UUID
}

func (e *ErrDependencyCycle) Error() string {
	ids := make([]string, len(e.Path))
	for i, u := range e.Path {
		ids[i] = u.String()
	}
	return "wbs: dependency cycle: " + ids[0] + " <- ... <- " + ids[len(ids)-1]
}

// CheckDependencyGraph validates a dependency graph before accepting a
// finalized execution plan (WBS 2.15). deps maps unit ID to the IDs it
// depends on. Database constraints are not sufficient: direct (A ↔ B),
// indirect (A → B → C → A), and self (A → A) cycles are all rejected
// (T2.15.1–T2.15.5). The traversal is deterministic: nodes and edges are
// visited in sorted order, so identical input always yields identical
// results, including the cycle path.
func CheckDependencyGraph(deps map[uuid.UUID][]uuid.UUID) error {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS path
		black = 2 // fully explored
	)
	color := map[uuid.UUID]int{}
	var stack []uuid.UUID
	var cycle []uuid.UUID

	nodes := make([]uuid.UUID, 0, len(deps))
	for id := range deps {
		nodes = append(nodes, id)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].String() < nodes[j].String() })

	var visit func(u uuid.UUID) bool
	visit = func(u uuid.UUID) bool {
		color[u] = gray
		stack = append(stack, u)
		out := append([]uuid.UUID(nil), deps[u]...)
		sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
		for _, v := range out {
			switch color[v] {
			case white:
				if visit(v) {
					return true
				}
			case gray:
				// Back edge: the cycle is the stack segment from v to u.
				idx := 0
				for i, n := range stack {
					if n == v {
						idx = i
						break
					}
				}
				cycle = append([]uuid.UUID{}, stack[idx:]...)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return false
	}

	for _, n := range nodes {
		if color[n] == white {
			if visit(n) {
				return &ErrDependencyCycle{Path: cycle}
			}
		}
	}
	return nil
}
