package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P6-20 (WBS 6.23 + 6.24): runtime-state persistence with optimistic
// concurrency. The RuntimeStateService persists the project's current
// logical workflow position and protects it with a versioned update so
// concurrent overseer processes cannot lose a transition. Pure
// persistence + concurrency guard — no transition evaluation.

// p620Service builds the runtime-state service over the harness pool.
// usePool selects the transactional vs sequential (nil-pool) path.
func p620Service(h *Harness, usePool bool) *service.RuntimeStateService {
	pool := h.App.DB().Raw()
	svc := service.NewRuntimeStateService(dbrepo.NewRuntimeStateRepository(pool))
	if usePool {
		svc = svc.WithTxPool(pool)
	}
	return svc
}

// p620StateIDs returns the persisted ids of the machine v1 states.
func p620StateIDs(t *testing.T, h *Harness, machineID, key string) string {
	t.Helper()
	var id string
	if err := h.DBScan(context.Background(),
		"SELECT id::text FROM workflow_states WHERE machine_id = '"+machineID+"' AND key = '"+key+"'", &id); err != nil {
		t.Fatalf("read state %s: %v", key, err)
	}
	return id
}

// p620Fixture seeds a project, a machine (v1, states A/B/C with
// A->B and B->C transitions), a phase, and a work unit, and returns all
// the ids the tests need.
func p620Fixture(t *testing.T, h *Harness) (project, machine, stateA, stateB, stateC, phase, unit string) {
	t.Helper()
	ctx := context.Background()
	project = p605Project(t, h)
	machine = p617SeedMachine(t, h, project, "m-runtime",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"},
			{Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"},
			{From: "B", Outcome: "DONE", To: "C"},
		},
		"A",
	)
	stateA = p620StateIDs(t, h, machine, "A")
	stateB = p620StateIDs(t, h, machine, "B")
	stateC = p620StateIDs(t, h, machine, "C")
	if _, err := h.DBExec(ctx, "INSERT INTO phases (id, project_id, name, description, phase_order, status, created_at, updated_at) VALUES (gen_random_uuid(), '"+project+"', 'P', '', 1, 'PENDING', now(), now())"); err != nil {
		t.Fatalf("seed phase: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT id::text FROM phases WHERE project_id = '"+project+"'", &phase); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if _, err := h.DBExec(ctx, "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES (gen_random_uuid(), '"+phase+"', 1, 1, 'READY')"); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	var planID string
	if err := h.DBScan(ctx, "SELECT id::text FROM execution_plans WHERE phase_id = '"+phase+"'", &planID); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if _, err := h.DBExec(ctx, "INSERT INTO work_units (id, execution_plan_id, sequence, name, kind, status, objective, created_at) VALUES (gen_random_uuid(), '"+planID+"', 1, 'W', 'ATOMIC', 'PENDING', 'obj', now())"); err != nil {
		t.Fatalf("seed work unit: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT id::text FROM work_units WHERE execution_plan_id = '"+planID+"'", &unit); err != nil {
		t.Fatalf("read work unit: %v", err)
	}
	return project, machine, stateA, stateB, stateC, phase, unit
}

// P6-20.1 — Migration 00030 applies: the max schema version advances to
// 30 and the runtime_state table exists. (PLAN.md pre-declared 00029;
// 00029 is machine versioning — accepted shift.)
func TestP620MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'runtime_state'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("runtime_state table present = %d, want 1", n)
	}
}

// T6.23.1 — Runtime state persisted: after a transition the current
// position (machine + version, current state, entry time, version) is
// stored and read back exactly.
func TestP620RuntimeStatePersisted(t *testing.T) {
	h := NewHarness(t)
	project, machine, _, stateB, _, _, _ := p620Fixture(t, h)
	svc := p620Service(h, true)
	st, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateB, ExpectedVersion: 0})
	if err != nil {
		t.Fatalf("initial transition: %v", err)
	}
	if st.CurrentStateID != stateB {
		t.Fatalf("current state = %s, want %s", st.CurrentStateID, stateB)
	}
	if st.StateMachineID != machine || st.StateMachineVersion != 1 {
		t.Fatalf("machine binding = (%s, %d), want (%s, 1)", st.StateMachineID, st.StateMachineVersion, machine)
	}
	if st.Version != 1 {
		t.Fatalf("version = %d, want 1 (first write starts at 1)", st.Version)
	}
	got, err := svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CurrentStateID != stateB || got.StateMachineID != machine || got.Version != 1 {
		t.Fatalf("stored = (%s, %s, %d), want (%s, %s, 1)",
			got.CurrentStateID, got.StateMachineID, got.Version, stateB, machine)
	}
	if got.EnteredAt.IsZero() {
		t.Fatal("entered_at is zero; must be set on persistence")
	}
	if got.PhaseID != nil || got.WorkUnitID != nil {
		t.Fatalf("nullable associations leaked: phase=%v unit=%v", got.PhaseID, got.WorkUnitID)
	}
}

// T6.24.1 + T6.24.2 — Current version updates and increments: each
// successful versioned transition moves the stored version up by exactly
// one, and the caller receives the new version.
func TestP620VersionUpdatesAndIncrements(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, _, _ := p620Fixture(t, h)
	svc := p620Service(h, true)
	if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 0}); err != nil {
		t.Fatalf("init: %v", err)
	}
	st, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateB, ExpectedVersion: 1})
	if err != nil {
		t.Fatalf("A->B: %v", err)
	}
	if st.CurrentStateID != stateB || st.Version != 2 {
		t.Fatalf("after A->B = (%s, %d), want (%s, 2)", st.CurrentStateID, st.Version, stateB)
	}
	st, err = svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateC, ExpectedVersion: 2})
	if err != nil {
		t.Fatalf("B->C: %v", err)
	}
	if st.CurrentStateID != stateC || st.Version != 3 {
		t.Fatalf("after B->C = (%s, %d), want (%s, 3)", st.CurrentStateID, st.Version, stateC)
	}
	got, err := svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != 3 {
		t.Fatalf("stored version = %d, want 3", got.Version)
	}
}

// T6.24.3 — Stale update rejected: a transition carrying an outdated
// expected version is rejected and the stored state is untouched.
func TestP620StaleUpdateRejected(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, _, _ := p620Fixture(t, h)
	svc := p620Service(h, true)
	if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 0}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateB, ExpectedVersion: 1}); err != nil {
		t.Fatalf("A->B: %v", err)
	}
	// Stale: claims to move from version 1, but the stored version is 2.
	_, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateC, ExpectedVersion: 1})
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("stale transition err = %v, want service.ErrConflict", err)
	}
	got, err := svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CurrentStateID != stateB || got.Version != 2 {
		t.Fatalf("stored after stale attempt = (%s, %d), want (%s, 2) — the stale write must be a no-op",
			got.CurrentStateID, got.Version, stateB)
	}
}

// T6.24.4 — No lost state transition: N goroutines racing a transition
// from the same version — every writer that commits wins the version,
// the committed state is a valid single transition (version = 2), and
// the losers are rejected, not silently dropped.
func TestP620NoLostTransitionConcurrent(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, _, _ := p620Fixture(t, h)
	svc := p620Service(h, true)
	if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 0}); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Every racer targets a DIFFERENT destination from version 1, so any
	// committed state must be one of the targets (a lost transition would
	// leave the stored state at A).
	targets := []string{stateB, stateC, stateB, stateC, stateB, stateC}
	results := make([]error, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: targets[i], ExpectedVersion: 1})
		}(i)
	}
	wg.Wait()
	var committed int
	for _, err := range results {
		if err == nil {
			committed++
		} else if !errors.Is(err, service.ErrConflict) {
			t.Fatalf("racer err = %v, want nil or ErrConflict", err)
		}
	}
	if committed == 0 {
		t.Fatal("no racer committed; at least one version-1 transition must win")
	}
	got, err := svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("final version = %d, want 2 (a lost transition would leave version 1)", got.Version)
	}
	valid := map[string]bool{stateB: true, stateC: true}
	if !valid[got.CurrentStateID] {
		t.Fatalf("final state = %s, want one of the racers' targets", got.CurrentStateID)
	}
}

// T6.24.5 — Conflict detectable by caller: the service returns
// service.ErrConflict (not an infrastructure error) for a stale version,
// on both assembly paths.
func TestP620ConflictDetectable(t *testing.T) {
	for _, usePool := range []bool{false, true} {
		h := NewHarness(t)
		project, machine, stateA, stateB, _, _, _ := p620Fixture(t, h)
		svc := p620Service(h, usePool)
		if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 0}); err != nil {
			t.Fatalf("init: %v", err)
		}
		if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateB, ExpectedVersion: 1}); err != nil {
			t.Fatalf("A->B: %v", err)
		}
		_, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 99})
		if !errors.Is(err, service.ErrConflict) {
			t.Fatalf("pool=%v stale err = %v, want service.ErrConflict", usePool, err)
		}
		// A concurrent writer at version 2 succeeds in between; the caller's
		// stale version-1 attempt must now conflict (detectable, not lost).
		if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 2}); err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
		if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 1}); !errors.Is(err, service.ErrConflict) {
			t.Fatalf("stale err = %v, want service.ErrConflict", err)
		}
	}
}

// T6.23.3 + T6.23.4 — Phase and work-unit associations are retained:
// binding them in a transition persists them and they survive reads.
func TestP620PhaseAndUnitAssociationsRetained(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, phase, unit := p620Fixture(t, h)
	svc := p620Service(h, true)
	// Bind the phase on the initial write.
	ph := phase
	_, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, PhaseID: &ph, ExpectedVersion: 0})
	if err != nil {
		t.Fatalf("init with phase: %v", err)
	}
	got, err := svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PhaseID == nil || *got.PhaseID != phase {
		t.Fatalf("phase association = %v, want %s", got.PhaseID, phase)
	}
	// Bind the work unit on a later transition.
	un := unit
	_, err = svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateB, PhaseID: &ph, WorkUnitID: &un, ExpectedVersion: 1})
	if err != nil {
		t.Fatalf("A->B with unit: %v", err)
	}
	got, err = svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PhaseID == nil || *got.PhaseID != phase {
		t.Fatalf("phase association after second transition = %v, want %s", got.PhaseID, phase)
	}
	if got.WorkUnitID == nil || *got.WorkUnitID != unit {
		t.Fatalf("work-unit association = %v, want %s", got.WorkUnitID, unit)
	}
}

// T6.23.5 — The state-machine version binding is retained: the stored
// machine version is the one the transition declared and it is read back
// unchanged.
func TestP620MachineVersionRetained(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, _, _ := p620Fixture(t, h)
	svc := p620Service(h, true)
	if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 0}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateB, ExpectedVersion: 1}); err != nil {
		t.Fatalf("A->B: %v", err)
	}
	got, err := svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.StateMachineID != machine {
		t.Fatalf("machine id = %s, want %s", got.StateMachineID, machine)
	}
	if got.StateMachineVersion != 1 {
		t.Fatalf("machine version = %d, want 1", got.StateMachineVersion)
	}
	// A transition re-declaring the same binding keeps it.
	if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 2}); err != nil {
		t.Fatalf("B->A: %v", err)
	}
	got, err = svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.StateMachineVersion != 1 || got.StateMachineID != machine {
		t.Fatalf("binding changed: (%s, %d)", got.StateMachineID, got.StateMachineVersion)
	}
}

// T6.23.2 — Current state restored after restart: the runtime state
// (current state, machine + version binding, phase and work-unit
// associations, version, entered_at) survives a full app restart
// (two-boot e2e pattern over a caller-owned DB).
func TestP620RestoredAfterRestart(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	h1 := NewHarnessOn(t, base, dbName)
	t.Cleanup(h1.Close)
	project, machine, stateA, stateB, _, phase, unit := p620Fixture(t, h1)
	svc := p620Service(h1, true)
	if _, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateA, ExpectedVersion: 0}); err != nil {
		t.Fatalf("init: %v", err)
	}
	ph, un := phase, unit
	st, err := svc.Transition(context.Background(), project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateB, PhaseID: &ph, WorkUnitID: &un, ExpectedVersion: 1})
	if err != nil {
		t.Fatalf("A->B: %v", err)
	}
	before, err := svc.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("pre-restart get: %v", err)
	}
	if st.Version != before.Version {
		t.Fatalf("service return version %d != stored %d", st.Version, before.Version)
	}

	h2 := NewHarnessOn(t, base, dbName)
	t.Cleanup(h2.Close)
	svc2 := p620Service(h2, true)
	after, err := svc2.Get(context.Background(), project)
	if err != nil {
		t.Fatalf("post-restart get: %v", err)
	}
	if after.CurrentStateID != before.CurrentStateID {
		t.Fatalf("current state after restart = %s, want %s", after.CurrentStateID, before.CurrentStateID)
	}
	if after.StateMachineID != before.StateMachineID || after.StateMachineVersion != before.StateMachineVersion {
		t.Fatalf("machine binding after restart = (%s,%d), want (%s,%d)",
			after.StateMachineID, after.StateMachineVersion, before.StateMachineID, before.StateMachineVersion)
	}
	if after.PhaseID == nil || *after.PhaseID != phase {
		t.Fatalf("phase after restart = %v, want %s", after.PhaseID, phase)
	}
	if after.WorkUnitID == nil || *after.WorkUnitID != unit {
		t.Fatalf("unit after restart = %v, want %s", after.WorkUnitID, unit)
	}
	if after.Version != before.Version {
		t.Fatalf("version after restart = %d, want %d", after.Version, before.Version)
	}
	if !after.EnteredAt.Equal(before.EnteredAt) {
		t.Fatalf("entered_at after restart = %s, want %s", after.EnteredAt, before.EnteredAt)
	}
}
