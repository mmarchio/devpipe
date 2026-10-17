package precompute

import (
	"sort"
)

// StateNode is one state of a pinned state-machine version.
type StateNode struct {
	Key         string
	Kind        string
	Enabled     bool
	Terminal    bool
	TemplateKey string
}

// Edge is one transition of a pinned state-machine version.
type Edge struct {
	From     string
	To       string
	Outcome  string
	Priority int
	Enabled  bool
}

// Graph is the versioned state-machine graph (WBS 11.5): only the states
// and transitions of one machine version.
type Graph struct {
	MachineVersion int
	States         map[string]StateNode
	Edges          []Edge
}

// Reach is one reachable future state with the transition path that leads
// to it ("OUTCOME>STATE" steps) and its depth (1 = direct successor).
type Reach struct {
	StateKey string
	Depth    int
	Path     []string
}

// Reachable returns every state reachable from start within depth steps
// (WBS 11.5/11.6): only enabled transitions between enabled states of this
// machine version are followed — no transition is invented. Each state is
// reported once, at its shortest depth (BFS), so cycles terminate; ties
// within a depth follow (from, priority, outcome, to) order. The start
// state itself is never reported.
func (g Graph) Reachable(start string, depth int) []Reach {
	if depth < 1 {
		return nil
	}
	if s, ok := g.States[start]; !ok || !s.Enabled {
		return nil
	}
	edges := append([]Edge(nil), g.Edges...)
	sort.SliceStable(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Outcome != b.Outcome {
			return a.Outcome < b.Outcome
		}
		return a.To < b.To
	})
	seen := map[string]bool{start: true}
	frontier := []Reach{{StateKey: start}}
	var out []Reach
	for d := 1; d <= depth && len(frontier) > 0; d++ {
		var next []Reach
		for _, cur := range frontier {
			for _, e := range edges {
				if e.From != cur.StateKey || !e.Enabled {
					continue
				}
				to, ok := g.States[e.To]
				if !ok || !to.Enabled || seen[e.To] {
					continue
				}
				seen[e.To] = true
				path := append(append([]string(nil), cur.Path...), e.Outcome+">"+e.To)
				r := Reach{StateKey: e.To, Depth: d, Path: path}
				out = append(out, r)
				next = append(next, r)
			}
		}
		frontier = next
	}
	return out
}

// HasPath reports whether the path (from start) consists of existing,
// enabled transitions of this version (projection validation, WBS 11.21).
func (g Graph) HasPath(start string, path []string) bool {
	cur := start
	for _, step := range path {
		outcome, to := splitStep(step)
		ok := false
		for _, e := range g.Edges {
			if e.From == cur && e.To == to && e.Outcome == outcome && e.Enabled {
				if s, found := g.States[to]; found && s.Enabled {
					ok = true
					break
				}
			}
		}
		if !ok {
			return false
		}
		cur = to
	}
	return true
}

func splitStep(step string) (string, string) {
	for i := len(step) - 1; i >= 0; i-- {
		if step[i] == '>' {
			return step[:i], step[i+1:]
		}
	}
	return "", step
}
