// P6-38 (WBS 6.48): historical workflow integrity. A workflow edit during
// project execution appends a NEW machine version; the active runtime stays
// bound to the version it started on, that version remains fully
// reconstructable (rows intact, hash unchanged), the new version is
// available for a fresh binding, and a second edit on the new version leaves
// the original version byte-identical. Test-only task: the behavior is the
// composition of P6-17 (edit appends a version), P6-19 (snapshot/hash),
// P6-20 (runtime persistence), P6-21 (transition application) and P6-22
// (initialize captures the binding) — RED is impossible, matching the
// P6-32..P6-37 workflow-test pattern.
package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// p638RowSig returns a byte-level signature of the runtime_state row for the
// project: the full column set concatenated, so any mutation (state, machine
// binding, machine version, runtime version, timestamps) is caught.
func p638RowSig(t *testing.T, h *Harness, project string) string {
	t.Helper()
	var sig string
	if err := h.DBScan(context.Background(),
		"SELECT project_id::text||'|'||state_machine_id::text||'|'||state_machine_version::text||'|'||"+
			"current_state_id::text||'|'||version::text||'|'||created_at::text||'|'||updated_at::text "+
			"FROM runtime_state WHERE project_id = '"+project+"'", &sig); err != nil {
		t.Fatalf("read runtime_state signature: %v", err)
	}
	return sig
}

// p638MachineSig returns a byte-level signature of one machine version's
// persisted definition: the parent row's key fields plus an ordered
// aggregate over its states and transitions.
func p638MachineSig(t *testing.T, h *Harness, machineID string, version int) string {
	t.Helper()
	ctx := context.Background()
	var mSig, sSig, trSig string
	if err := h.DBScan(ctx,
		"SELECT id::text||'|'||family_id::text||'|'||version::text||'|'||name||'|'||"+
			"coalesce(initial_state_id::text,'')||'|'||enabled::text||'|'||created_at::text||'|'||updated_at::text "+
			"FROM state_machines WHERE family_id = '"+machineID+"' AND version = "+itoa(version), &mSig); err != nil {
		t.Fatalf("read machine row: %v", err)
	}
	if err := h.DBScan(ctx,
		"SELECT coalesce(string_agg(id::text||'|'||key||'|'||kind::text||'|'||enabled::text||'|'||updated_at::text, ',' ORDER BY key), '') "+
			"FROM workflow_states WHERE machine_id = '"+machineID+"' AND version = "+itoa(version), &sSig); err != nil {
		t.Fatalf("read states: %v", err)
	}
	if err := h.DBScan(ctx,
		"SELECT coalesce(string_agg(outcome||'|'||enabled::text||'|'||updated_at::text, ',' ORDER BY outcome), '') "+
			"FROM state_transitions WHERE machine_id = '"+machineID+"' AND version = "+itoa(version), &trSig); err != nil {
		t.Fatalf("read transitions: %v", err)
	}
	return mSig + "||" + sSig + "||" + trSig
}

// T6.48.1 — Active runtime remains v3 (here: v2). A runtime bound to a
// non-trivial machine version (initialized from a v2 machine, then driven
// through real transitions) is left byte-identical in storage when an
// administrator edit appends a newer version: machine binding, machine
// version, current state, and runtime version are all unchanged.
func TestP638ActiveRuntimeStaysOnItsVersion(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid := p618Seed(t, h) // v1: states A/B/C + instruction/role/file on A
	// The edit creates v2 (P6-17 rule); it changes the definition (a new
	// state D is inserted) so a mutated binding would be observable.
	v2id, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "B",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "TERMINAL"}, {Key: "D", Kind: "ACTION"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "B", Outcome: "DONE", To: "C"},
			{From: "B", Outcome: "FIX", To: "D"},
		},
	})
	if err != nil {
		t.Fatalf("edit to v2: %v", err)
	}
	// Initialize from v2 (T6.30.2: the binding captures the machine's actual
	// version), then drive real transitions to a non-trivial position.
	//
	// NOTE: v2's state rows are fresh per-version ids; stateB/stateC above
	// are v1 ids, so resolve the v2 ids explicitly.
	var v2B, v2C string
	if err := h.DBScan(context.Background(),
		"SELECT id::text FROM workflow_states WHERE machine_id = '"+v2id+"' AND key = 'B'", &v2B); err != nil {
		t.Fatalf("read v2 state B: %v", err)
	}
	if err := h.DBScan(context.Background(),
		"SELECT id::text FROM workflow_states WHERE machine_id = '"+v2id+"' AND key = 'C'", &v2C); err != nil {
		t.Fatalf("read v2 state C: %v", err)
	}
	rtSvc := p622Service(h, true)
	st, err := rtSvc.Initialize(context.Background(), proj, v2id)
	if err != nil {
		t.Fatalf("initialize from v2: %v", err)
	}
	if st.StateMachineVersion != 2 {
		t.Fatalf("initialized machine version = %d, want 2 (binding must capture the actual version)", st.StateMachineVersion)
	}
	applySvc := p621Service(h, true)
	// B self-loop (FIX) then B->C: two real applications, runtime version 3.
	//
	// NOTE: the v2 machine has no B->B row (the edit defines B->D as FIX),
	// but the P6-21 application service applies an already-selected
	// transition without re-validating the edge — same contract as
	// TestP621SequenceIncrements' C->A step.
	out, err := applySvc.ApplyTransition(context.Background(), proj, &service.ApplyTransitionInput{
		FromStateID: v2B, ToStateID: v2B, Outcome: "FIX", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("apply FIX: %v", err)
	}
	if out.State.CurrentStateID != v2B {
		t.Fatalf("state after FIX = %s, want %s", out.State.CurrentStateID, v2B)
	}
	out, err = applySvc.ApplyTransition(context.Background(), proj, &service.ApplyTransitionInput{
		FromStateID: v2B, ToStateID: v2C, Outcome: "DONE", ExpectedVersion: 2,
	})
	if err != nil {
		t.Fatalf("apply DONE: %v", err)
	}
	if out.State.CurrentStateID != v2C || out.State.Version != 3 {
		t.Fatalf("position = (%s, %d), want (%s, 3)", out.State.CurrentStateID, out.State.Version, v2C)
	}
	// The runtime is at a non-trivial position (C, version 3, bound to v2).
	before := p638RowSig(t, h, proj)
	// Administrator edit while execution is active: appends v3.
	if _, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "B",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "TERMINAL"}, {Key: "D", Kind: "ACTION"}, {Key: "E", Kind: "ACTION"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "B", Outcome: "DONE", To: "C"},
			{From: "B", Outcome: "FIX", To: "D"}, {From: "D", Outcome: "GO", To: "E"},
		},
	}); err != nil {
		t.Fatalf("edit to v3: %v", err)
	}
	after := p638RowSig(t, h, proj)
	if before != after {
		t.Fatalf("runtime row changed by the v3 edit:\nbefore=%s\nafter=%s", before, after)
	}
	// Direct ground truth on the binding columns.
	var mID, sID string
	var mVer, ver int
	if err := h.DBScan(context.Background(),
		"SELECT state_machine_id::text, state_machine_version, current_state_id::text, version "+
			"FROM runtime_state WHERE project_id = '"+proj+"'", &mID, &mVer, &sID, &ver); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if mID != mid || mVer != 2 || sID != v2C || ver != 3 {
		t.Fatalf("binding after edit = (%s, %d, %s, %d), want (%s, 2, %s, 3) — the runtime must stay on v2",
			mID, mVer, sID, ver, mid, v2C)
	}
}

// T6.48.2 — v3 (here: v2) remains reconstructable after an edit to a newer
// version: the original version's rows are intact (byte-identical
// definition signature) and a snapshot read of the original version returns
// the SAME configuration hash as before the edit (and matches the P6-18
// reference over its persisted definition).
func TestP638OriginalVersionReconstructable(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h) // v1 with instruction/role/file — a non-trivial hash
	sigV1 := p638MachineSig(t, h, mid, 1)
	hashV1 := p618Hash(t, h, mid, 1)
	if hashV1 == "" {
		t.Fatal("pre-edit hash is empty — the fixture must carry a non-trivial definition")
	}
	// An edit that changes the definition (new state, new initial state).
	if _, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "C",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"}, {Key: "C", Kind: "TERMINAL"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "B", Outcome: "DONE", To: "C"},
			{From: "C", Outcome: "REOPEN", To: "A"},
		},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// v1's rows are intact, byte-identical.
	if got := p638MachineSig(t, h, mid, 1); got != sigV1 {
		t.Fatalf("v1 definition changed by the edit:\nbefore=%s\nafter=%s", sigV1, got)
	}
	// v1 is addressable by (family, version) — the new version row's id is
	// NOT the family id, and GetVersion(family, 1) must resolve to the v1
	// anchor row.
	machines := dbrepo.NewStateMachineRepository(h.App.DB().Raw())
	v1, err := machines.GetVersion(context.Background(), uuid.MustParse(mid), 1)
	if err != nil {
		t.Fatalf("get v1 by family+version: %v", err)
	}
	if v1.ID.String() != mid {
		t.Fatalf("GetVersion(family,1) resolved id %s, want the v1 anchor %s", v1.ID, mid)
	}
	// The snapshot read of v1 returns the SAME hash as before the edit.
	post := p618Hash(t, h, mid, 1)
	if post != hashV1 {
		t.Fatalf("v1 hash after edit = %s, want the pre-edit hash %s", post, hashV1)
	}
	// And it matches the P6-19 snapshot service's hash (same definition,
	// same hash — the P6-19 parity contract).
	snap := p619Snapshot(t, h, mid, 1, true)
	if snap.Hash != hashV1 {
		t.Fatalf("snapshot hash = %s, want %s", snap.Hash, hashV1)
	}
	if snap.Version != 1 {
		t.Fatalf("snapshot version = %d, want 1", snap.Version)
	}
}

// T6.48.3 — v4 (here: v3) available for future execution: the version
// appended by an edit is bindable — a FRESH runtime (a separate project)
// initialized from the new version stores the new version's number in its
// binding.
func TestP638NewVersionAvailableForFutureExecution(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	// Two edits: v2 and v3.
	if _, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "A",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"}, {Key: "C", Kind: "TERMINAL"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "B", Outcome: "DONE", To: "C"},
		},
	}); err != nil {
		t.Fatalf("edit to v2: %v", err)
	}
	v3id, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "C",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"}, {Key: "C", Kind: "TERMINAL"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "B", Outcome: "DONE", To: "C"},
		},
	})
	if err != nil {
		t.Fatalf("edit to v3: %v", err)
	}
	// The machine family now has exactly versions 1..3 (addressed by the
	// family id — the v3 row's id is NOT the family id).
	machines := dbrepo.NewStateMachineRepository(h.App.DB().Raw())
	vers, err := machines.ListVersions(context.Background(), uuid.MustParse(mid))
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(vers) != 3 || vers[2].Version != 3 {
		t.Fatalf("versions = %v, want [1 2 3]", vers)
	}
	if vers[2].ID.String() == mid {
		t.Fatal("v3 row id equals the family id — addressing must be by family+version, not row id")
	}
	if vers[2].ID.String() != v3id {
		t.Fatalf("v3 row id = %s, want the edit-returned id %s", vers[2].ID, v3id)
	}
	// Future execution: a fresh project, a fresh runtime, bound to v3.
	fresh := p605Project(t, h)
	st, err := p622Service(h, true).Initialize(context.Background(), fresh, v3id)
	if err != nil {
		t.Fatalf("initialize fresh runtime from v3: %v", err)
	}
	if st.StateMachineVersion != 3 {
		t.Fatalf("fresh binding machine version = %d, want 3 (the new version must be bindable)", st.StateMachineVersion)
	}
	var mID, sID string
	var mVer int
	if err := h.DBScan(context.Background(),
		"SELECT state_machine_id::text, state_machine_version, current_state_id::text "+
			"FROM runtime_state WHERE project_id = '"+fresh+"'", &mID, &mVer, &sID); err != nil {
		t.Fatalf("read fresh binding: %v", err)
	}
	if mID != mid || mVer != 3 || sID == "" {
		t.Fatalf("fresh binding = (%s, %d, %q), want (%s, 3, non-empty state)", mID, mVer, sID, mid)
	}
	// The original project's runtime (if any) is untouched: here the first
	// project was never initialized, so exactly one runtime row exists.
	var n int
	if err := h.DBScan(context.Background(), "SELECT count(*) FROM runtime_state", &n); err != nil {
		t.Fatalf("count runtimes: %v", err)
	}
	if n != 1 {
		t.Fatalf("runtime rows = %d, want 1 (the fresh init must not touch other projects)", n)
	}
}

// T6.48.4 — Editing v4 cannot mutate v3 (here: a second edit on v3 creates
// v4, and v2 — the version a running execution is bound to — remains
// byte-identical in rows AND configuration hash).
func TestP638EditingNewVersionCannotMutateOld(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid := p618Seed(t, h) // v1
	// Edit 1 -> v2; the running execution binds to v2 and drives it.
	v2id, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "B",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"}, {Key: "C", Kind: "TERMINAL"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "B", Outcome: "DONE", To: "C"},
		},
	})
	if err != nil {
		t.Fatalf("edit to v2: %v", err)
	}
	var v2B string
	if err := h.DBScan(context.Background(),
		"SELECT id::text FROM workflow_states WHERE machine_id = '"+v2id+"' AND key = 'B'", &v2B); err != nil {
		t.Fatalf("read v2 state B: %v", err)
	}
	if _, err := p622Service(h, true).Initialize(context.Background(), proj, v2id); err != nil {
		t.Fatalf("initialize from v2: %v", err)
	}
	// One real application so the runtime is active at a moved position.
	//
	// NOTE: v2 has no B->B edge; the P6-21 application contract (apply an
	// already-selected transition) permits the self-loop step, as in
	// T6.48.1's fixture and TestP621SequenceIncrements.
	if _, err := p621Service(h, true).ApplyTransition(context.Background(), proj, &service.ApplyTransitionInput{
		FromStateID: v2B, ToStateID: v2B, Outcome: "FIX", ExpectedVersion: 1,
	}); err != nil {
		t.Fatalf("apply FIX: %v", err)
	}
	sigV2 := p638MachineSig(t, h, mid, 2)
	hashV2 := p618Hash(t, h, mid, 2)
	// Edit 2 -> v3 (a distinct definition: new state D).
	if _, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "B",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "TERMINAL"}, {Key: "D", Kind: "ACTION"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "B", Outcome: "DONE", To: "C"},
			{From: "B", Outcome: "FIX", To: "D"},
		},
	}); err != nil {
		t.Fatalf("edit to v3: %v", err)
	}
	// Edit 3 -> v4 (a SECOND edit on the new version lineage: state E).
	if _, err := p617Service(h).Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "B",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "TERMINAL"}, {Key: "D", Kind: "ACTION"}, {Key: "E", Kind: "ACTION"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "B", Outcome: "DONE", To: "C"},
			{From: "B", Outcome: "FIX", To: "D"}, {From: "D", Outcome: "GO", To: "E"},
		},
	}); err != nil {
		t.Fatalf("edit to v4: %v", err)
	}
	// v2 — the version the running execution is bound to — is byte-identical
	// in its persisted rows AND in its configuration hash.
	if got := p638MachineSig(t, h, mid, 2); got != sigV2 {
		t.Fatalf("v2 definition mutated by edits to v3/v4:\nbefore=%s\nafter=%s", sigV2, got)
	}
	if got := p618Hash(t, h, mid, 2); got != hashV2 {
		t.Fatalf("v2 hash after edits to v3/v4 = %s, want the pre-edit hash %s", got, hashV2)
	}
	// The runtime binding is still v2 (full-row signature).
	var mID string
	var mVer, ver int
	if err := h.DBScan(context.Background(),
		"SELECT state_machine_id::text, state_machine_version, version FROM runtime_state WHERE project_id = '"+proj+"'",
		&mID, &mVer, &ver); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if mID != mid || mVer != 2 || ver != 2 {
		t.Fatalf("binding after two edits on newer versions = (%s, %d, %d), want (%s, 2, 2)", mID, mVer, ver, mid)
	}
	// The family advanced to exactly four versions, in order.
	vers, err := dbrepo.NewStateMachineRepository(h.App.DB().Raw()).
		ListVersions(context.Background(), uuid.MustParse(mid))
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(vers) != 4 {
		t.Fatalf("versions = %d, want 4", len(vers))
	}
	for i, want := range []int{1, 2, 3, 4} {
		if vers[i].Version != want {
			t.Fatalf("versions = %v, want [1 2 3 4]", vers)
		}
	}
}
