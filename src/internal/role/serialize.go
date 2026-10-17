package role

import (
	"fmt"
	"sort"
	"strings"
)

// Serialize returns a deterministic canonical text encoding of the snapshot
// (WBS 5.30): given an unchanged role configuration the output is
// byte-for-byte stable across calls and processes, so it can be used for
// hashing, prompt caching, debugging, and reproducibility. Field order is
// fixed; state-file and allowed-state entries are emitted in sorted order so
// map/iteration order never alters the output (set-ness, matching
// ConfigurationHash semantics); instructions are emitted in their snapshot
// (assignment) order, which is deliberately preserved. String values are
// quoted with %q so content containing newlines or other special characters
// round-trips unambiguously.
func (s *RoleSnapshot) Serialize() string {
	var b strings.Builder
	fmt.Fprintf(&b, "role:%s@%d\n", s.RoleKey, s.RoleVersion)
	fmt.Fprintf(&b, "prompt:%d|%s|%s\n", s.SystemPrompt.Version, s.SystemPrompt.Classification, s.SystemPrompt.ContentHash)
	fmt.Fprintf(&b, "prompt:%s\n", s.SystemPrompt.Content)
	for _, ins := range s.Instructions {
		fmt.Fprintf(&b, "instruction:%s@%d|%s|%s|%s\n", ins.Key, ins.Version, ins.Name, ins.Classification, ins.ContentHash)
		fmt.Fprintf(&b, "instruction:%s\n", ins.Content)
	}
	states := append([]string(nil), s.AllowedStates...)
	sort.Strings(states)
	fmt.Fprintf(&b, "states:%s\n", quoteList(states))
	files := append([]string(nil), s.StateFiles...)
	sort.Strings(files)
	fmt.Fprintf(&b, "statefiles:%s\n", quoteList(files))
	return b.String()
}

// quoteList renders a list of strings as space-separated %q-quoted values —
// deterministic and unambiguous for any content.
func quoteList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, " ")
}
