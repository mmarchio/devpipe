package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
	"overseer/internal/state"
)

// P6-19 (WBS 6.22): StateMachineSnapshot read model. The snapshot is an
// immutable, deterministic view of ONE machine version: its enabled states
// (with role references and state-instruction versions), its transitions,
// its initial state, and the P6-18 configuration hash — assembled in one
// transaction (nil-pool sequential fallback).

// p619Snapshot returns the snapshot of (machine, version) through the
// snapshot service; usePool selects the transactional (wired pool) vs the
// sequential (nil pool) assembly path.
func p619Snapshot(t *testing.T, h *Harness, machineID string, version int, usePool bool) *state.StateMachineSnapshot {
	t.Helper()
	pool := h.App.DB().Raw()
	svc := service.NewStateMachineSnapshotService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateInstructionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewStateFileRequirementRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
		dbrepo.NewTransitionConditionRepository(pool),
	)
	if usePool {
		svc = svc.WithTxPool(pool)
	}
	snap, err := svc.Snapshot(context.Background(), machineID, version)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return snap
}

func p619StateKeys(snap *state.StateMachineSnapshot) []string {
	keys := make([]string, 0, len(snap.States))
	for _, s := range snap.States {
		keys = append(keys, s.Key)
	}
	return keys
}

func p619StateByKey(snap *state.StateMachineSnapshot, key string) *state.StateSnapshot {
	for i := range snap.States {
		if snap.States[i].Key == key {
			return &snap.States[i]
		}
	}
	return nil
}

// T6.22.1 — The snapshot includes all enabled states (and only them): the
// p618 fixture has three states; disabling one leaves it out of the
// snapshot.
func TestP619SnapshotEnabledStates(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "UPDATE workflow_states SET enabled = false WHERE machine_id = '"+mid+"' AND key = 'C'"); err != nil {
		t.Fatalf("disable state C: %v", err)
	}
	snap := p619Snapshot(t, h, mid, 1, false)
	keys := p619StateKeys(snap)
	if len(keys) != 2 || keys[0] != "A" || keys[1] != "B" {
		t.Fatalf("enabled state keys = %v, want [A B]", keys)
	}
	if snap.Version != 1 {
		t.Fatalf("snapshot version = %d, want 1", snap.Version)
	}
	if snap.MachineID.String() != mid {
		t.Fatalf("snapshot machine id = %s, want %s", snap.MachineID.String(), mid)
	}
}

// T6.22.2 — The snapshot includes the version's transitions, addressed by
// state key (the per-version row ids are never exposed).
func TestP619SnapshotTransitions(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	snap := p619Snapshot(t, h, mid, 1, false)
	if len(snap.Transitions) != 2 {
		t.Fatalf("transition count = %d, want 2", len(snap.Transitions))
	}
	byKey := func() map[[3]string]bool {
		m := map[[3]string]bool{}
		for _, tr := range snap.Transitions {
			m[[3]string{tr.From, tr.To, tr.Outcome}] = true
		}
		return m
	}()
	for _, want := range [][3]string{{"A", "B", "GO"}, {"B", "C", "DONE"}} {
		if !byKey[[3]string(want)] {
			t.Fatalf("transition %+v missing from snapshot", want)
		}
	}
	if snap.InitialState.Key != "A" {
		t.Fatalf("initial state key = %q, want A", snap.InitialState.Key)
	}
}

// T6.22.3 — The snapshot includes the states' role references (role keys,
// in the assignment's ordinal order).
func TestP619SnapshotRoleReferences(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	snap := p619Snapshot(t, h, mid, 1, false)
	a := p619StateByKey(snap, "A")
	if a == nil {
		t.Fatal("state A missing from snapshot")
	}
	if len(a.Roles) != 1 || a.Roles[0] != "P618-ROLE" {
		t.Fatalf("state A roles = %v, want [P618-ROLE]", a.Roles)
	}
	b := p619StateByKey(snap, "B")
	if b == nil {
		t.Fatal("state B missing from snapshot")
	}
	if len(b.Roles) != 0 {
		t.Fatalf("state B roles = %v, want none", b.Roles)
	}
}

// T6.22.4 — The snapshot includes each state's current instruction version
// (and its content hash); a state without an instruction reports none.
func TestP619SnapshotInstructionVersions(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	ctx := context.Background()
	var aID string
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'A'", &aID); err != nil {
		t.Fatalf("read state A: %v", err)
	}
	instr := dbrepo.NewStateInstructionRepository(h.App.DB().Raw())
	if _, err := instr.AppendVersion(ctx, uuid.MustParse(aID), "second version"); err != nil {
		t.Fatalf("append instruction v2: %v", err)
	}
	wantHash, err := instr.GetVersion(ctx, uuid.MustParse(aID), 2)
	if err != nil {
		t.Fatalf("read instruction v2: %v", err)
	}
	snap := p619Snapshot(t, h, mid, 1, false)
	a := p619StateByKey(snap, "A")
	if a == nil {
		t.Fatal("state A missing from snapshot")
	}
	if a.InstructionVersion != 2 {
		t.Fatalf("state A instruction version = %d, want 2", a.InstructionVersion)
	}
	if a.InstructionHash != wantHash.ContentHash {
		t.Fatalf("state A instruction hash = %s, want %s", a.InstructionHash, wantHash.ContentHash)
	}
	b := p619StateByKey(snap, "B")
	if b == nil {
		t.Fatal("state B missing from snapshot")
	}
	if b.InstructionVersion != 0 || b.InstructionHash != "" {
		t.Fatalf("state B instruction = (%d, %q), want none", b.InstructionVersion, b.InstructionHash)
	}
}

// T6.22.5 — The snapshot is deterministic: the same persisted definition
// yields byte-identical snapshots on repeated calls, on both assembly
// paths (sequential and single-transaction), and the snapshot's hash
// matches the P6-18 configuration hash of the same version.
func TestP619SnapshotDeterministic(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	seq := p619Snapshot(t, h, mid, 1, false)
	txv := p619Snapshot(t, h, mid, 1, true)
	s1, err := json.Marshal(seq)
	if err != nil {
		t.Fatalf("marshal sequential: %v", err)
	}
	s2, err := json.Marshal(txv)
	if err != nil {
		t.Fatalf("marshal transactional: %v", err)
	}
	if string(s1) != string(s2) {
		t.Fatalf("snapshots differ across assembly paths: %s vs %s", s1, s2)
	}
	ref := p618Hash(t, h, mid, 1)
	if seq.Hash != ref {
		t.Fatalf("snapshot hash = %s, want P6-18 reference %s", seq.Hash, ref)
	}
	if seq.Hash == "" {
		t.Fatal("snapshot hash is empty")
	}
}
