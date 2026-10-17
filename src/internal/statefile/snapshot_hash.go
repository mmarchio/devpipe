package statefile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// SnapshotHash returns the deterministic aggregate hash of the snapshot's
// full semantic definition (WBS 7.34): project, phase, work unit, runtime
// state id + version, state machine id + version, state key, the role
// configuration hash, and the ordered state-file (type key, version, hash)
// tuples. It is the stable identity input for Phase 9/10 prompt caching:
// identical semantic snapshots hash identically, and any component change
// moves the hash (T7.34.1-T7.34.5).
//
// The hash is DERIVED, not persisted (the snapshot row's persisted bytes
// are the hash's inputs; no hash column exists). Excluded from the hash:
// the snapshot's own row id and created_at (identity/audit fields, not
// semantic content). The state-file tuples are hashed in canonical
// type-key-sorted order so the aggregate is order-independent (T7.34.5)
// while remaining sensitive to each tuple's (key, version, hash) content.
// The role enters through its own ConfigurationHash (WBS 5.16), so a role
// edit that does not move the configuration hash does not move the
// snapshot hash.
func (s *ExecutionContextSnapshot) SnapshotHash() string {
	var b strings.Builder
	fmt.Fprintf(&b, "project:%s\n", s.ProjectID)
	if s.PhaseID != nil {
		fmt.Fprintf(&b, "phase:%s\n", *s.PhaseID)
	}
	if s.WorkUnitID != nil {
		fmt.Fprintf(&b, "unit:%s\n", *s.WorkUnitID)
	}
	fmt.Fprintf(&b, "runtime:%s@%d\n", s.RuntimeStateID, s.RuntimeStateVersion)
	fmt.Fprintf(&b, "machine:%s@%d\n", s.StateMachineID, s.StateMachineVersion)
	fmt.Fprintf(&b, "state:%s\n", s.StateKey)
	fmt.Fprintf(&b, "role:%s\n", s.RoleSnapshot.ConfigurationHash())
	files := make([]StateFileSnapshot, len(s.StateFiles))
	copy(files, s.StateFiles)
	sort.Slice(files, func(i, j int) bool { return files[i].TypeKey < files[j].TypeKey })
	for _, f := range files {
		fmt.Fprintf(&b, "file:%s:%d:%s\n", f.TypeKey, f.Version, f.Hash)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// SemanticHash is the snapshot's prompt-semantic identity (Phase 11,
// approved PLAN Decision 1): the SnapshotHash inputs WITHOUT the runtime
// state's row id and version. The runtime version advances on every
// workflow transition yet carries no prompt semantics, so a cache key built
// from it could never be projected ahead of a transition. Everything that
// shapes the prompt — project, phase, work unit, state machine id +
// version, state key, role configuration and the ordered state-file
// (type, version, hash) pins — stays in.
func (s *ExecutionContextSnapshot) SemanticHash() string {
	var b strings.Builder
	fmt.Fprintf(&b, "semantic-snapshot:v1\nproject:%s\n", s.ProjectID)
	if s.PhaseID != nil {
		fmt.Fprintf(&b, "phase:%s\n", *s.PhaseID)
	}
	if s.WorkUnitID != nil {
		fmt.Fprintf(&b, "unit:%s\n", *s.WorkUnitID)
	}
	fmt.Fprintf(&b, "machine:%s@%d\n", s.StateMachineID, s.StateMachineVersion)
	fmt.Fprintf(&b, "state:%s\n", s.StateKey)
	fmt.Fprintf(&b, "role:%s\n", s.RoleSnapshot.ConfigurationHash())
	files := make([]StateFileSnapshot, len(s.StateFiles))
	copy(files, s.StateFiles)
	sort.Slice(files, func(i, j int) bool { return files[i].TypeKey < files[j].TypeKey })
	for _, f := range files {
		fmt.Fprintf(&b, "file:%s:%d:%s\n", f.TypeKey, f.Version, f.Hash)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
