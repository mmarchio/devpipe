package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"overseer/internal/service"
)

// P6-37 (WBS 6.47): restart recovery test. The runtime is persisted at a
// known multi-field position (project RUNNING, phase order 7, work-unit
// sequence 13, state IMPLEMENT, runtime version 82, machine version 1)
// REACHED BY APPLYING REAL TRANSITIONS, the first process is closed, a
// fresh process boots against the SAME persisted database, and every
// position field is read back from the fresh process / persisted rows and
// compared byte-for-byte against the captured pre-stop values. No
// inference from logs or LLM context: all assertions compare persisted
// values. The stop/start cycle is repeated for determinism, and the
// second boot must write no new rows (no transition applied on boot).

// p637Position is the full persisted workflow position the acceptance
// criteria verify across the restart.
type p637Position struct {
	ProjectID       string
	ProjectStatus   string
	PhaseOrder      int
	UnitSequence    int
	CurrentStateKey string
	RuntimeVersion  int
	MachineVersion  int
	HistoryCount    int
	MaxSeq          int
	StateEntrySeq   int
}

// p637Capture reads the position from the given instance: runtime state
// through the service (fresh-process reads on the second boot) and
// project/phase/unit values through direct SQL — persisted rows only.
func p637Capture(t *testing.T, h *Harness, project, phase, unit string) *p637Position {
	t.Helper()
	ctx := context.Background()
	pos := &p637Position{ProjectID: project}
	st, err := p620Service(h, true).Get(ctx, project)
	if err != nil {
		t.Fatalf("get runtime state: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT key FROM workflow_states WHERE id = '"+st.CurrentStateID+"'", &pos.CurrentStateKey); err != nil {
		t.Fatalf("state key: %v", err)
	}
	pos.RuntimeVersion = st.Version
	pos.MachineVersion = st.StateMachineVersion
	if err := h.DBScan(ctx, "SELECT status FROM projects WHERE id = '"+project+"'", &pos.ProjectStatus); err != nil {
		t.Fatalf("project status: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT phase_order FROM phases WHERE id = '"+phase+"'", &pos.PhaseOrder); err != nil {
		t.Fatalf("phase order: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT sequence FROM work_units WHERE id = '"+unit+"'", &pos.UnitSequence); err != nil {
		t.Fatalf("unit sequence: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_transition_history WHERE project_id = '"+project+"'", &n); err != nil {
		t.Fatalf("history count: %v", err)
	}
	pos.HistoryCount = n
	if err := h.DBScan(ctx, "SELECT coalesce(max(seq),0) FROM state_transition_history WHERE project_id = '"+project+"'", &pos.MaxSeq); err != nil {
		t.Fatalf("history max seq: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT coalesce(state_entry_seq,0) FROM projects WHERE id = '"+project+"'", &pos.StateEntrySeq); err != nil {
		t.Fatalf("state entry seq: %v", err)
	}
	return pos
}

// p637AssertEqual compares a post-restart position field-by-field against
// the captured pre-stop position.
func p637AssertEqual(t *testing.T, before, after *p637Position) {
	t.Helper()
	if after.ProjectStatus != before.ProjectStatus || after.ProjectStatus != "RUNNING" {
		t.Fatalf("project status after restart = %q, want %q (RUNNING)", after.ProjectStatus, before.ProjectStatus)
	}
	if after.PhaseOrder != before.PhaseOrder || after.PhaseOrder != 7 {
		t.Fatalf("phase order after restart = %d, want %d (7)", after.PhaseOrder, before.PhaseOrder)
	}
	if after.UnitSequence != before.UnitSequence || after.UnitSequence != 13 {
		t.Fatalf("work-unit sequence after restart = %d, want %d (13)", after.UnitSequence, before.UnitSequence)
	}
	if after.CurrentStateKey != before.CurrentStateKey || after.CurrentStateKey != "IMPLEMENT" {
		t.Fatalf("state after restart = %q, want %q (IMPLEMENT)", after.CurrentStateKey, before.CurrentStateKey)
	}
	if after.RuntimeVersion != before.RuntimeVersion || after.RuntimeVersion != 82 {
		t.Fatalf("runtime version after restart = %d, want %d (82)", after.RuntimeVersion, before.RuntimeVersion)
	}
	if after.MachineVersion != before.MachineVersion {
		t.Fatalf("machine version after restart = %d, want %d (same as pre-stop)", after.MachineVersion, before.MachineVersion)
	}
	if after.HistoryCount != before.HistoryCount {
		t.Fatalf("history count after restart = %d, want %d (no new rows on boot)", after.HistoryCount, before.HistoryCount)
	}
	if after.MaxSeq != before.MaxSeq {
		t.Fatalf("history max seq after restart = %d, want %d", after.MaxSeq, before.MaxSeq)
	}
	if after.StateEntrySeq != before.StateEntrySeq {
		t.Fatalf("state entry seq after restart = %d, want %d", after.StateEntrySeq, before.StateEntrySeq)
	}
}

func TestP637RestartRecovery(t *testing.T) {
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

	// First boot (migrates). Build the position: project RUNNING (direct
	// SQL — the lifecycle REST flow is not the subject of this test),
	// phase order 7, work-unit sequence 13, machine at state IMPLEMENT.
	h1 := NewHarnessOn(t, base, dbName)
	t.Cleanup(h1.Close)
	project := p605Project(t, h1)
	// Machine with a self-loop on IMPLEMENT so the position is reached by
	// applying REAL transitions (INIT -> IMPLEMENT, then 81 loop
	// applications) to runtime version 82.
	machine := p617SeedMachine(t, h1, project, "m-restart",
		[]service.MachineStateDef{
			{Key: "INIT", Kind: "CONTROL"},
			{Key: "IMPLEMENT", Kind: "ACTION"},
		},
		[]service.MachineTransitionDef{
			{From: "INIT", Outcome: "GO", To: "IMPLEMENT"},
			{From: "IMPLEMENT", Outcome: "STEP", To: "IMPLEMENT"},
		},
		"INIT",
	)
	var stateInit, stateImpl, phase, unit string
	if err := h1.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+machine+"' AND key = 'INIT'", &stateInit); err != nil {
		t.Fatalf("read INIT state: %v", err)
	}
	if err := h1.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+machine+"' AND key = 'IMPLEMENT'", &stateImpl); err != nil {
		t.Fatalf("read IMPLEMENT state: %v", err)
	}
	if _, err := h1.DBExec(ctx, "INSERT INTO phases (id, project_id, name, description, phase_order, status, created_at, updated_at) VALUES (gen_random_uuid(), '"+project+"', 'Phase 7', '', 7, 'EXECUTING', now(), now())"); err != nil {
		t.Fatalf("seed phase: %v", err)
	}
	if err := h1.DBScan(ctx, "SELECT id::text FROM phases WHERE project_id = '"+project+"'", &phase); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if _, err := h1.DBExec(ctx, "INSERT INTO execution_plans (id, phase_id, wbs_version, version, status) VALUES (gen_random_uuid(), '"+phase+"', 1, 1, 'READY')"); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	var planID string
	if err := h1.DBScan(ctx, "SELECT id::text FROM execution_plans WHERE phase_id = '"+phase+"'", &planID); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if _, err := h1.DBExec(ctx, "INSERT INTO work_units (id, execution_plan_id, sequence, name, kind, status, objective, created_at) VALUES (gen_random_uuid(), '"+planID+"', 13, 'W7.13', 'ATOMIC', 'EXECUTING', 'obj', now())"); err != nil {
		t.Fatalf("seed work unit: %v", err)
	}
	if err := h1.DBScan(ctx, "SELECT id::text FROM work_units WHERE execution_plan_id = '"+planID+"'", &unit); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	// Project status RUNNING — direct-SQL bookkeeping (same pattern as the
	// prior lifecycle tests): the machine-driven state under test is the
	// runtime position, not the lifecycle flow.
	if _, err := h1.DBExec(ctx, "UPDATE projects SET status = 'RUNNING' WHERE id = '"+project+"'"); err != nil {
		t.Fatalf("set project RUNNING: %v", err)
	}

	// Reach the WBS position by applying real transitions: seed the
	// runtime at INIT (version 1), INIT->IMPLEMENT (version 2), then 80
	// STEP loops to runtime version 82 — the IMPLEMENT loop is the
	// non-trivial version, and the phase/unit associations are bound on
	// the second transition.
	rtsvc := p620Service(h1, true)
	if _, err := rtsvc.Transition(ctx, project, &service.TransitionInput{StateMachineID: machine, StateMachineVersion: 1, CurrentStateID: stateInit, ExpectedVersion: 0}); err != nil {
		t.Fatalf("init: %v", err)
	}
	ph, un := phase, unit
	appsvc := p621Service(h1, true)
	if _, err := appsvc.ApplyTransition(ctx, project, &service.ApplyTransitionInput{FromStateID: stateInit, ToStateID: stateImpl, Outcome: "GO", PhaseID: &ph, WorkUnitID: &un, ExpectedVersion: 1}); err != nil {
		t.Fatalf("INIT->IMPLEMENT: %v", err)
	}
	for v := 2; v < 82; v++ {
		if _, err := appsvc.ApplyTransition(ctx, project, &service.ApplyTransitionInput{FromStateID: stateImpl, ToStateID: stateImpl, Outcome: "STEP", ExpectedVersion: v}); err != nil {
			t.Fatalf("STEP at version %d: %v", v, err)
		}
	}
	before := p637Capture(t, h1, project, phase, unit)
	if before.RuntimeVersion != 82 {
		t.Fatalf("pre-stop runtime version = %d, want 82 (fixture, not assertion)", before.RuntimeVersion)
	}

	h1.Close()

	// Second boot: fresh process against the SAME persisted database —
	// the position must be reconstructed from persisted state only.
	h2 := NewHarnessOn(t, base, dbName)
	t.Cleanup(h2.Close)
	after := p637Capture(t, h2, project, phase, unit)
	p637AssertEqual(t, before, after)

	// No side effects: the second boot wrote no new rows.
	if after.HistoryCount != before.HistoryCount {
		t.Fatalf("history count after second boot = %d, want %d", after.HistoryCount, before.HistoryCount)
	}

	// Determinism: repeat the stop/start cycle — the same persisted
	// state must reconstruct to the same position a second time.
	h2.Close()
	h3 := NewHarnessOn(t, base, dbName)
	t.Cleanup(h3.Close)
	third := p637Capture(t, h3, project, phase, unit)
	p637AssertEqual(t, before, third)
}
