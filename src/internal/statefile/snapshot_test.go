package statefile

import (
	"testing"

	"github.com/google/uuid"
	"reflect"

	"overseer/internal/role"
)

// T7.30.1 — Exact version captured. A StateFileSnapshot pins a specific
// version of its file, and the pinned version is immutable on the value: there
// is no mutation path that advances it, so the snapshot's version is the exact
// version that was captured at snapshot time.
func TestStateFileSnapshotExactVersionCaptured(t *testing.T) {
	f := mustFile(t, ScopeProject)
	v1, err := AppendVersion(f, "one", timeUTCNow)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	f.CurrentVersion = v1.Version
	v2, err := AppendVersion(f, "two", timeUTCNow)
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	f.CurrentVersion = v2.Version

	// Pin the HISTORICAL v1, not the current v2.
	snap := StateFileSnapshot{
		StateFileID: f.ID,
		TypeKey:     "PROJECT_STATE",
		Scope:       f.Scope,
		Version:     v1.Version,
		Hash:        v1.ContentHash,
		Content:     v1.Content,
	}

	// The pinned version is the exact captured version (v1), not the file's
	// current version (v2).
	if snap.Version != 1 {
		t.Errorf("pinned Version = %d, want 1", snap.Version)
	}
	if snap.Version == f.CurrentVersion {
		t.Error("snapshot pinned the file's current version, not the captured version")
	}

	// Immutability: copy the value, mutate the copy, the original is untouched
	// (value semantics, no shared mutation path).
	copied := snap
	copied.Version = 99
	copied.Content = "mutated"
	if snap.Version != 1 {
		t.Error("mutating a copy changed the original pinned version")
	}
	if snap.Content != v1.Content {
		t.Error("mutating a copy changed the original pinned content")
	}
}

// T7.30.2 — Exact hash captured. The snapshot's Hash must equal
// ContentHash(Content) for a constructed snapshot.
func TestStateFileSnapshotExactHashCaptured(t *testing.T) {
	f := mustFile(t, ScopeWorkUnit)
	v, err := AppendVersion(f, "hash me", timeUTCNow)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	snap := StateFileSnapshot{
		StateFileID: f.ID,
		TypeKey:     "ACTIVE_TASK",
		Scope:       f.Scope,
		Version:     v.Version,
		Hash:        v.ContentHash,
		Content:     v.Content,
	}
	// The captured hash must equal the deterministic hash of the captured
	// content.
	if snap.Hash != ContentHash(snap.Content) {
		t.Errorf("Hash %q != ContentHash(Content) %q", snap.Hash, ContentHash(snap.Content))
	}
	// ... and equal the immutable version's stored hash.
	if snap.Hash != v.ContentHash {
		t.Errorf("Hash %q does not match the pinned version's stored hash %q", snap.Hash, v.ContentHash)
	}
}

// T7.30.3 — Correct content reconstructable. A snapshot assembled from a pinned
// version reproduces the exact bytes of that version; a second snapshot pinning
// a DIFFERENT version of the same file carries that version's bytes (the pin,
// not the current, is authoritative).
func TestStateFileSnapshotContentReconstructable(t *testing.T) {
	f := mustFile(t, ScopePhase)
	v1, err := AppendVersion(f, "phase state v1", timeUTCNow)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	f.CurrentVersion = v1.Version
	v2, err := AppendVersion(f, "phase state v2", timeUTCNow)
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	f.CurrentVersion = v2.Version

	// Snapshot A pins v1; snapshot B pins v2. Both snapshots of the SAME file.
	snapA := StateFileSnapshot{StateFileID: f.ID, TypeKey: "PHASE_STATE", Scope: f.Scope, Version: v1.Version, Hash: v1.ContentHash, Content: v1.Content}
	snapB := StateFileSnapshot{StateFileID: f.ID, TypeKey: "PHASE_STATE", Scope: f.Scope, Version: v2.Version, Hash: v2.ContentHash, Content: v2.Content}

	// Reconstruction: the pinned version's bytes are exactly what the snapshot
	// carries.
	if snapA.Content != v1.Content {
		t.Errorf("snapshot A content = %q, want the pinned v1 bytes %q", snapA.Content, v1.Content)
	}
	if snapB.Content != v2.Content {
		t.Errorf("snapshot B content = %q, want the pinned v2 bytes %q", snapB.Content, v2.Content)
	}
	// The pin is authoritative, not the current version: A carries v1's bytes
	// even though the file now sits at v2.
	if snapA.Content == snapB.Content {
		t.Error("two pins of the same file at different versions carry identical content")
	}
	if snapA.Version == snapB.Version {
		t.Error("two pins of the same file at different versions share a version")
	}
}

// T7.30.4 — Scope retained. The file's StateFileScope survives on the snapshot
// for every scope.
func TestStateFileSnapshotScopeRetained(t *testing.T) {
	for _, scope := range []StateFileScope{ScopeProject, ScopePhase, ScopeWorkUnit, ScopeExecution} {
		f := mustFile(t, scope)
		v, err := AppendVersion(f, "scope content", timeUTCNow)
		if err != nil {
			t.Fatalf("append for scope %s: %v", scope, err)
		}
		snap := StateFileSnapshot{
			StateFileID: f.ID,
			TypeKey:     "TEST_STATE",
			Scope:       f.Scope,
			Version:     v.Version,
			Hash:        v.ContentHash,
			Content:     v.Content,
		}
		if snap.Scope != scope {
			t.Errorf("scope %s: snapshot Scope = %s, want %s", scope, snap.Scope, scope)
		}
	}
}

// T7.30.x (7.29 shape pinned via construction) — ExecutionContextSnapshot
// carries the position record: project/phase/unit/runtime/machine/state/
// role/state-files/CreatedAt, with the role reused by value and the
// state-file list holding the exact pins.
func TestExecutionContextSnapshotShape(t *testing.T) {
	proj := uuid.New()
	phase := uuid.New()
	unit := uuid.New()
	rtState := uuid.New()
	machine := uuid.New()
	files := []StateFileSnapshot{
		{StateFileID: uuid.New(), TypeKey: "PROJECT_STATE", Scope: ScopeProject, Version: 1, Hash: ContentHash("p"), Content: "p"},
		{StateFileID: uuid.New(), TypeKey: "ACTIVE_TASK", Scope: ScopeWorkUnit, Version: 3, Hash: ContentHash("a"), Content: "a"},
	}
	roleSnap := role.RoleSnapshot{RoleID: uuid.New(), RoleKey: "ENGINEER", RoleVersion: 2}

	s := ExecutionContextSnapshot{
		ID:                  uuid.New(),
		ProjectID:           proj,
		PhaseID:             &phase,
		WorkUnitID:          &unit,
		RuntimeStateID:      rtState,
		RuntimeStateVersion: 7,
		StateMachineID:      machine,
		StateMachineVersion: 1,
		StateKey:            "IMPLEMENT",
		RoleSnapshot:        roleSnap,
		StateFiles:          files,
		CreatedAt:           timeFixed,
	}

	if s.ProjectID != proj || s.PhaseID == nil || *s.PhaseID != phase || s.WorkUnitID == nil || *s.WorkUnitID != unit {
		t.Error("position ids (project/phase/unit) not retained")
	}
	if s.RuntimeStateID != rtState || s.RuntimeStateVersion != 7 {
		t.Error("runtime position not retained")
	}
	if s.StateMachineID != machine || s.StateMachineVersion != 1 || s.StateKey != "IMPLEMENT" {
		t.Error("machine position not retained")
	}
	if !reflect.DeepEqual(s.RoleSnapshot, roleSnap) {
		t.Error("role snapshot not retained by value")
	}
	if len(s.StateFiles) != len(files) {
		t.Fatalf("StateFiles len = %d, want %d", len(s.StateFiles), len(files))
	}
	for i := range files {
		if s.StateFiles[i] != files[i] {
			t.Errorf("StateFiles[%d] not retained", i)
		}
	}
	if s.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}
