package role

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ConfigurationHash returns a deterministic aggregate hash representing the
// role configuration (WBS 5.16): role version + system-prompt hash + ordered
// instruction hashes + allowed states + state-file requirements. It is
// deterministic for an unchanged configuration and changes whenever any
// relevant component changes (T5.16.1–T5.16.6). It will later allow
// prompt-cache invalidation without comparing every field.
//
// The ordering sensitivity is deliberate: instructions are hashed in their
// snapshot (assignment) order (T5.16.4), while the state lists are hashed in
// sorted order so an equivalent-but-reordered set of states/ files hashes
// identically (their set-ness, not list order, is what matters to the
// config). This keeps the hash stable across storage re-orderings of the
// deduplicated state sets while remaining sensitive to instruction order.
func (s *RoleSnapshot) ConfigurationHash() string {
	var b strings.Builder
	fmt.Fprintf(&b, "role:%s@%d\n", s.RoleKey, s.RoleVersion)
	fmt.Fprintf(&b, "system:%d:%s\n", s.SystemPrompt.Version, s.SystemPrompt.ContentHash)
	for _, ins := range s.Instructions {
		fmt.Fprintf(&b, "instruction:%s@%d:%s\n", ins.Key, ins.Version, ins.ContentHash)
	}
	states := append([]string(nil), s.AllowedStates...)
	sort.Strings(states)
	fmt.Fprintf(&b, "states:%v\n", states)
	files := append([]string(nil), s.StateFiles...)
	sort.Strings(files)
	fmt.Fprintf(&b, "statefiles:%v\n", files)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
