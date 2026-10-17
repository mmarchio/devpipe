package statefile

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/role"
)

// P7-19 (WBS 7.34): deterministic aggregate snapshot hash.
//
// SnapshotHash is a pure function over the snapshot's full semantic
// definition (project/phase/unit, runtime id+version, machine id+version,
// state key, role configuration hash, ordered state-file (key, version,
// hash) tuples). It is the stable identity input for Phase 9/10 prompt
// caching. Identical semantic snapshots hash identically; any component
// change moves the hash; the state-file tuples hash in canonical sorted
// order (order-independent, content-sensitive).

var (
	p719ProjectID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	p719PhaseID   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	p719UnitID    = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	p719RuntimeID = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	p719MachineID = uuid.MustParse("55555555-5555-5555-5555-555555555555")
	p719FileAID   = uuid.MustParse("66666666-6666-6666-6666-666666666666")
	p719FileBID   = uuid.MustParse("77777777-7777-7777-7777-777777777777")
)

// p719SnapshotFixture returns a fully-populated snapshot with fixed
// semantic ids (so two fixtures are genuinely identical in content) and a
// fresh row id + created_at (the identity fields the hash excludes).
func p719SnapshotFixture() *ExecutionContextSnapshot {
	phase, unit := p719PhaseID, p719UnitID
	return &ExecutionContextSnapshot{
		ID:                  uuid.New(),
		ProjectID:           p719ProjectID,
		PhaseID:             &phase,
		WorkUnitID:          &unit,
		RuntimeStateID:      p719RuntimeID,
		RuntimeStateVersion: 3,
		StateMachineID:      p719MachineID,
		StateMachineVersion: 2,
		StateKey:            "IMPL",
		RoleSnapshot: role.RoleSnapshot{
			RoleKey:     "ENGINEER",
			RoleVersion: 5,
			SystemPrompt: role.PromptSnapshot{
				Version: 4, ContentHash: "abc123",
			},
			AllowedStates: []string{"A", "B"},
			StateFiles:    []string{"A", "B"},
		},
		StateFiles: []StateFileSnapshot{
			{StateFileID: p719FileAID, TypeKey: "A", Scope: ScopeProject, Version: 4, Hash: "h-a"},
			{StateFileID: p719FileBID, TypeKey: "B", Scope: ScopePhase, Version: 7, Hash: "h-b"},
		},
		CreatedAt: time.Now().UTC(),
	}
}

// T7.34.1 — The same semantic snapshot produces the same hash: two
// instances with byte-identical semantic content (fresh row ids and
// created_at, the fields the hash excludes) hash identically.
func TestP719IdenticalSnapshotSameHash(t *testing.T) {
	a, b := p719SnapshotFixture(), p719SnapshotFixture()
	if a.ID == b.ID {
		t.Fatal("fixture sanity: row ids must be distinct (proof the id is excluded from the hash)")
	}
	ga, gb := a.SnapshotHash(), b.SnapshotHash()
	if ga == "" || ga != gb {
		t.Fatalf("hashes %q vs %q: identical semantic snapshots must hash identically (and be non-empty)", ga, gb)
	}
	// Repeated invocation on the SAME instance is stable.
	if got := a.SnapshotHash(); got != ga {
		t.Fatalf("recomputed hash %q != %q", got, ga)
	}
	// The excluded identity fields must genuinely be excluded: setting
	// them must not move the hash.
	c := *a
	c.ID = uuid.New()
	c.CreatedAt = time.Now().UTC().Add(2 * time.Hour)
	if got := c.SnapshotHash(); got != ga {
		t.Fatalf("row id/created_at change moved the hash: %q -> %q", ga, got)
	}
}

// T7.34.2 — A state-file change changes the hash: version, hash, and
// membership each move it.
func TestP719StateFileChangeChangesHash(t *testing.T) {
	base := p719SnapshotFixture()
	w := base.SnapshotHash()
	cases := map[string]func(*ExecutionContextSnapshot){
		"file version": func(s *ExecutionContextSnapshot) { s.StateFiles[0].Version = 5 },
		"file hash":    func(s *ExecutionContextSnapshot) { s.StateFiles[0].Hash = "h-a2" },
		"file added": func(s *ExecutionContextSnapshot) {
			s.StateFiles = append(s.StateFiles, StateFileSnapshot{StateFileID: uuid.New(), TypeKey: "C", Scope: ScopeProject, Version: 1, Hash: "h-c"})
		},
		"file removed": func(s *ExecutionContextSnapshot) { s.StateFiles = s.StateFiles[:1] },
	}
	for name, mutate := range cases {
		s := *base
		mutate(&s)
		if got := s.SnapshotHash(); got == w {
			t.Fatalf("%s: hash unchanged (%q) — the file component must affect the hash", name, got)
		}
	}
}

// T7.34.3 — A role change changes the hash: any role-component edit that
// moves the configuration hash moves the snapshot hash.
func TestP719RoleChangeChangesHash(t *testing.T) {
	base := p719SnapshotFixture()
	w := base.SnapshotHash()
	// RoleSnapshot is a value field; copy the whole snapshot per case.
	cases := map[string]func(s *ExecutionContextSnapshot){
		"role version": func(s *ExecutionContextSnapshot) { s.RoleSnapshot.RoleVersion = 6 },
		"prompt hash":  func(s *ExecutionContextSnapshot) { s.RoleSnapshot.SystemPrompt.ContentHash = "xyz" },
		"instruction": func(s *ExecutionContextSnapshot) {
			s.RoleSnapshot.Instructions = []role.InstructionSnapshot{{Key: "K", Version: 1, ContentHash: "i1"}}
		},
	}
	for name, f := range cases {
		s := *base
		f(&s)
		if got := s.SnapshotHash(); got == w {
			t.Fatalf("%s: hash unchanged (%q) — the role component must affect the hash", name, got)
		}
	}
	// A no-op role state (identical configuration hash) must NOT change
	// the snapshot hash: the role enters the aggregate through its
	// configuration hash, so an unchanged role is a stable input.
	s := *base
	if got := s.SnapshotHash(); got != w {
		t.Fatalf("untouched role moved the hash: %q -> %q", w, got)
	}
}

// T7.34.4 — A work-unit change changes the hash (as do the other
// position components).
func TestP719WorkUnitChangeChangesHash(t *testing.T) {
	base := p719SnapshotFixture()
	w := base.SnapshotHash()
	s := *base
	other := uuid.New()
	s.WorkUnitID = &other
	if got := s.SnapshotHash(); got == w {
		t.Fatalf("work-unit change left the hash %q — it must move", got)
	}
	s2 := *base
	other2 := uuid.New()
	s2.PhaseID = &other2
	if got := s2.SnapshotHash(); got == w {
		t.Fatalf("phase change left the hash %q — it must move", got)
	}
	s3 := *base
	s3.ProjectID = uuid.New()
	if got := s3.SnapshotHash(); got == w {
		t.Fatalf("project change left the hash %q — it must move", got)
	}
	s4 := *base
	s4.RuntimeStateVersion = 9
	if got := s4.SnapshotHash(); got == w {
		t.Fatalf("runtime version change left the hash %q", got)
	}
	s5 := *base
	s5.StateMachineVersion = 3
	if got := s5.SnapshotHash(); got == w {
		t.Fatalf("machine version change left the hash %q", got)
	}
	s6 := *base
	s6.StateKey = "VALIDATE"
	if got := s6.SnapshotHash(); got == w {
		t.Fatalf("state key change left the hash %q", got)
	}
}

// T7.34.5 — Deterministic ordering maintained: the state-file tuples
// hash in canonical type-key-sorted order. The same tuple set supplied
// in a different input order yields the SAME hash; a genuine
// tuple-content change still moves it.
func TestP719DeterministicOrdering(t *testing.T) {
	a := p719SnapshotFixture()
	ga := a.SnapshotHash()
	b := *a
	b.ID = uuid.New()
	b.CreatedAt = time.Now().UTC().Add(time.Hour)
	// b carries the SAME file tuple set in reversed order.
	b.StateFiles = []StateFileSnapshot{
		{StateFileID: p719FileBID, TypeKey: "B", Scope: ScopePhase, Version: 7, Hash: "h-b"},
		{StateFileID: p719FileAID, TypeKey: "A", Scope: ScopeProject, Version: 4, Hash: "h-a"},
	}
	if gb := b.SnapshotHash(); gb != ga {
		t.Fatalf("reordered file tuples changed the hash (%q vs %q): the tuple set must hash order-independently (canonical sorted order)", ga, gb)
	}
	// A genuine tuple-set change (not just reordering) still moves it.
	c := *a
	c.StateFiles = append([]StateFileSnapshot(nil), c.StateFiles...)
	c.StateFiles[1].Version = 8 // B v7 -> v8: a real change
	if got := c.SnapshotHash(); got == ga {
		t.Fatalf("tuple content change masked by ordering normalization: %q", got)
	}
}

// Phase 11 P11-01: the semantic hash ignores the runtime-state row id and
// version and moves on every prompt-semantic input.
func TestP1101SemanticHashIgnoresRuntimeVersion(t *testing.T) {
	unit := uuid.New()
	base := ExecutionContextSnapshot{
		ProjectID: uuid.New(), WorkUnitID: &unit, RuntimeStateID: uuid.New(), RuntimeStateVersion: 3,
		StateMachineID: uuid.New(), StateMachineVersion: 1, StateKey: "IMPLEMENT",
		StateFiles: []StateFileSnapshot{{TypeKey: "ACTIVE_TASK", Version: 2, Hash: "a2"}, {TypeKey: "ARCHITECTURE", Version: 7, Hash: "r7"}},
	}
	h := base.SemanticHash()
	later := base
	later.RuntimeStateVersion, later.RuntimeStateID = 9, uuid.New()
	if later.SemanticHash() != h {
		t.Fatal("runtime version / id moved the semantic hash")
	}
	if later.SnapshotHash() == base.SnapshotHash() {
		t.Fatal("the full snapshot hash must still include the runtime version")
	}
	for name, mut := range map[string]func(*ExecutionContextSnapshot){
		"file": func(s *ExecutionContextSnapshot) {
			s.StateFiles = []StateFileSnapshot{{TypeKey: "ACTIVE_TASK", Version: 3, Hash: "a3"}, s.StateFiles[1]}
		},
		"unit":    func(s *ExecutionContextSnapshot) { u := uuid.New(); s.WorkUnitID = &u },
		"state":   func(s *ExecutionContextSnapshot) { s.StateKey = "VALIDATE" },
		"machine": func(s *ExecutionContextSnapshot) { s.StateMachineVersion = 2 },
		"role":    func(s *ExecutionContextSnapshot) { s.RoleSnapshot.RoleVersion = 2 },
	} {
		c := base
		c.StateFiles = append([]StateFileSnapshot(nil), base.StateFiles...)
		mut(&c)
		if c.SemanticHash() == h {
			t.Fatalf("%s change did not move the semantic hash", name)
		}
	}
}
