// P6-39 (WBS 6.49): Phase 6 end-to-end acceptance. The full
// project/phase/WBS/execution-plan/work-unit hierarchy is built, the four
// roles (PLANNER, ENGINEER, TESTER, PROJECT_MANAGER) are configured in
// project scope, and the P6-11 default development-workflow state machine
// is instantiated for the project (CopyComposedToProject; the statefile
// workflow has since become the default). The WBS 6.49 flow is
// then simulated WITHOUT any LLM calls: every step's transition is applied
// through the REAL P6-21 transition-application service (the P6-31 preview
// verifies the selection route first), the INCOMPLETE reports are routed by
// the P6-13 context-threshold resolver (45%/80% -> CONTINUE_CURRENT_SESSION,
// 90%/80% -> RESET_SESSION), the MORE_WORK / MORE_PHASES forks by the
// remaining-work / remaining-phase resolvers, and the run is driven to
// PROJECT_DONE (terminal).
//
// The service is RESTARTED at several checkpoints (the P6-37 pattern: the
// harness process is closed and a fresh process booted against the SAME
// persisted database), and after each restart the persisted runtime state
// reconstructs the exact logical position (project/phase/unit/state/
// runtime version/machine version/history - byte-for-byte, no inference
// from logs or LLM context). The full persisted history records the complete
// applied sequence, and PROJECT_DONE is terminal: further transitions are
// rejected while all data is retained.
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"overseer/internal/service"
	"overseer/internal/state"
)

// p639Position is the full persisted workflow position the acceptance flow
// must reconstruct exactly across each restart.
type p639Position struct {
	ProjectStatus   string
	PhaseStatus     string
	UnitAStatus     string
	UnitBStatus     string
	CurrentStateKey string
	RuntimeVersion  int
	MachineVersion  int
	HistoryCount    int
	MaxSeq          int
}

func p639Capture(t *testing.T, h *Harness, project, phase, unitA, unitB string) *p639Position {
	t.Helper()
	ctx := context.Background()
	pos := &p639Position{}
	var stID string
	if err := h.DBScan(ctx, "SELECT current_state_id::text FROM runtime_state WHERE project_id = '"+project+"'", &stID); err != nil {
		t.Fatalf("read runtime state id: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT key FROM workflow_states WHERE id = '"+stID+"'", &pos.CurrentStateKey); err != nil {
		t.Fatalf("read state key: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT version, state_machine_version FROM runtime_state WHERE project_id = '"+project+"'", &pos.RuntimeVersion, &pos.MachineVersion); err != nil {
		t.Fatalf("read runtime versions: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM projects WHERE id = '"+project+"'", &pos.ProjectStatus); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+phase+"'", &pos.PhaseStatus); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unitA+"'", &pos.UnitAStatus); err != nil {
		t.Fatalf("read unitA: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+unitB+"'", &pos.UnitBStatus); err != nil {
		t.Fatalf("read unitB: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_transition_history WHERE project_id = '"+project+"'", &n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	pos.HistoryCount = n
	if err := h.DBScan(ctx, "SELECT coalesce(max(seq),0) FROM state_transition_history WHERE project_id = '"+project+"'", &pos.MaxSeq); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	return pos
}

func p639AssertEqual(t *testing.T, checkpoint string, before, after *p639Position) {
	t.Helper()
	if before.ProjectStatus != after.ProjectStatus {
		t.Fatalf("[%s] project status after restart = %q, want %q", checkpoint, after.ProjectStatus, before.ProjectStatus)
	}
	if before.PhaseStatus != after.PhaseStatus {
		t.Fatalf("[%s] phase status after restart = %q, want %q", checkpoint, after.PhaseStatus, before.PhaseStatus)
	}
	if before.UnitAStatus != after.UnitAStatus {
		t.Fatalf("[%s] unitA status after restart = %q, want %q", checkpoint, after.UnitAStatus, before.UnitAStatus)
	}
	if before.UnitBStatus != after.UnitBStatus {
		t.Fatalf("[%s] unitB status after restart = %q, want %q", checkpoint, after.UnitBStatus, before.UnitBStatus)
	}
	if before.CurrentStateKey != after.CurrentStateKey {
		t.Fatalf("[%s] state after restart = %q, want %q", checkpoint, after.CurrentStateKey, before.CurrentStateKey)
	}
	if before.RuntimeVersion != after.RuntimeVersion {
		t.Fatalf("[%s] runtime version after restart = %d, want %d", checkpoint, after.RuntimeVersion, before.RuntimeVersion)
	}
	if before.MachineVersion != after.MachineVersion {
		t.Fatalf("[%s] machine version after restart = %d, want %d", checkpoint, after.MachineVersion, before.MachineVersion)
	}
	if before.HistoryCount != after.HistoryCount {
		t.Fatalf("[%s] history count after restart = %d, want %d (no new rows on boot)", checkpoint, after.HistoryCount, before.HistoryCount)
	}
	if before.MaxSeq != after.MaxSeq {
		t.Fatalf("[%s] history max seq after restart = %d, want %d", checkpoint, after.MaxSeq, before.MaxSeq)
	}
}

// p639Fixture builds the WBS 6.49 hierarchy (project, phase 1 with a WBS +
// execution plan, Unit A seq 1 + Unit B seq 2), configures the four roles in
// project scope, and instantiates the P6-11 default state machine for the
// project. The runtime is initialized at PROJECT_INIT (version 1) and bound
// to the phase + Unit A. It returns project, machine, and the two unit ids.
func p639Fixture(t *testing.T, h *Harness) (project, machine, unitA, unitB string) {
	t.Helper()
	ctx := context.Background()
	project = p605Project(t, h)

	// Configure the four roles in project scope through the real role
	// service (WBS 6.49 "Configure roles"): InitializeProjectRoles copies
	// the global defaults into project scope (PLANNER, ENGINEER, TESTER,
	// PROJECT_MANAGER, + ARCHITECT).
	if _, err := snapshotSvc(t, h).InitializeProjectRoles(ctx, project); err != nil {
		t.Fatalf("initialize project roles: %v", err)
	}
	present := map[string]bool{}
	for _, want := range []string{"PLANNER", "ENGINEER", "TESTER", "PROJECT_MANAGER"} {
		var n int
		if err := h.DBScan(ctx, "SELECT count(*) FROM roles WHERE project_id = ('"+project+"')::uuid AND key = '"+want+"'", &n); err != nil {
			t.Fatalf("count role %s: %v", want, err)
		}
		if n == 0 {
			t.Fatalf("role %s not configured in project scope", want)
		}
		present[want] = n > 0
	}
	if len(present) != 4 {
		t.Fatalf("configured roles = %v, want the four named roles", present)
	}

	// Phase 1 with a WBS version + execution plan, Unit A (seq 1) and
	// Unit B (seq 2).
	phase := seedPhaseStatus(t, h, p633UUID(t, project), 1, "Phase 1", "PENDING").String()
	plan := seedPlan(t, h, p633UUID(t, phase), 1, 1, "READY")
	seedUnit(t, h, plan, 1, "A", "PENDING")
	seedUnit(t, h, plan, 2, "B", "PENDING")
	unitA = p633UnitBySeq(t, h, plan, 1)
	unitB = p633UnitBySeq(t, h, plan, 2)

	// Instantiate the P6-11 composed-prompt state machine for the project
	// (the WBS 6.49 flow runs on it, not on the statefile default).
	var err error
	machine, err = p611Svc(t, h).CopyComposedToProject(ctx, project)
	if err != nil {
		t.Fatalf("copy default machine: %v", err)
	}

	// Initialize the runtime at the machine's initial state (PROJECT_INIT),
	// version 1, bound to the phase + Unit A.
	if _, err := p622Service(h, true).Initialize(ctx, project, machine); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET phase_id = '"+phase+"', work_unit_id = '"+unitA+"' WHERE project_id = '"+project+"'"); err != nil {
		t.Fatalf("bind phase/unit: %v", err)
	}
	return
}

// p639Walk drives ONE step through the P6-31 preview (asserting the
// selection) and the real P6-21 apply op. expectedVersion is the
// optimistic-concurrency guard (advances with every applied step).
func p639Walk(t *testing.T, h *Harness, project, toID, outcome string, expectedVersion int) {
	t.Helper()
	ctx := context.Background()
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: outcome})
	if err != nil {
		t.Fatalf("preview(%s): %v", outcome, err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != toID {
		t.Fatalf("preview(%s) selected %+v, want destination %s", outcome, prev.Selected, toID)
	}
	out, err := p621Service(h, true).ApplyTransition(ctx, project, &service.ApplyTransitionInput{
		FromStateID: prev.CurrentState, ToStateID: toID, Outcome: outcome, ExpectedVersion: expectedVersion,
	})
	if err != nil {
		t.Fatalf("apply(%s): %v", outcome, err)
	}
	if out.State.CurrentStateID != toID {
		t.Fatalf("applied current state = %s, want %s", out.State.CurrentStateID, toID)
	}
	stID, ver := p632Runtime(t, h, project)
	if stID != toID || ver != expectedVersion+1 {
		t.Fatalf("runtime after apply = (%s, %d), want (%s, %d)", stID, ver, toID, expectedVersion+1)
	}
}

// p639Fork drives a MORE_WORK / MORE_PHASES fork step that carries an
// EXPLICIT rebind (the only legal association move): the preview selects the
// destination for the outcome, and the apply op lands the rebind.
func p639Fork(t *testing.T, h *Harness, project, toID, outcome string, rebind *string, expectedVersion int) {
	t.Helper()
	ctx := context.Background()
	prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: outcome})
	if err != nil {
		t.Fatalf("preview(%s): %v", outcome, err)
	}
	if prev.Selected == nil || prev.Selected.ToStateID != toID {
		t.Fatalf("preview(%s) selected %+v, want destination %s", outcome, prev.Selected, toID)
	}
	in := &service.ApplyTransitionInput{
		FromStateID: prev.CurrentState, ToStateID: toID, Outcome: outcome, ExpectedVersion: expectedVersion,
	}
	if rebind != nil {
		in.WorkUnitID = rebind
	}
	if _, err := p621Service(h, true).ApplyTransition(ctx, project, in); err != nil {
		t.Fatalf("apply(%s): %v", outcome, err)
	}
	stID, ver := p632Runtime(t, h, project)
	if stID != toID || ver != expectedVersion+1 {
		t.Fatalf("runtime after fork = (%s, %d), want (%s, %d)", stID, ver, toID, expectedVersion+1)
	}
}

// p639Restart simulates a service restart (P6-37 pattern): the harness
// process is closed, a fresh process boots against the SAME persisted
// database, and the position is compared byte-for-byte.
func p639Restart(t *testing.T, base, dbName, checkpoint string, old *Harness, project, phase, unitA, unitB string, before *p639Position) *Harness {
	t.Helper()
	old.Close()
	nh := NewHarnessOn(t, base, dbName)
	t.Cleanup(nh.Close)
	after := p639Capture(t, nh, project, phase, unitA, unitB)
	p639AssertEqual(t, checkpoint, before, after)
	return nh
}

// p639PhaseID reads the project's phase id (ground truth by direct SQL).
func p639PhaseID(t *testing.T, h *Harness, project string) string {
	t.Helper()
	var id string
	if err := h.DBScan(context.Background(), "SELECT id::text FROM phases WHERE project_id = '"+project+"'", &id); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	return id
}

func TestP639EndToEndAcceptance(t *testing.T) {
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

	h := NewHarnessOn(t, base, dbName)
	project, machine, unitA, unitB := p639Fixture(t, h)
	phase := p639PhaseID(t, h, project)

	// The instantiated machine is the P6-11 default at version 1.
	var machineVersion int
	if err := h.DBScan(ctx, "SELECT version FROM state_machines WHERE id = ('"+machine+"')::uuid", &machineVersion); err != nil {
		t.Fatalf("read machine: %v", err)
	}
	if machineVersion != 1 {
		t.Fatalf("instantiated machine version = %d, want 1", machineVersion)
	}

	sid := func(key string) string { return p620StateIDs(t, h, machine, key) }
	wantHistory := []string{}

	// ---- Phase planning: PROJECT_INIT -> ... -> IMPLEMENT (Unit A) ----
	p639Walk(t, h, project, sid("PHASE_PLAN"), "PROCEED", 1)
	p639Walk(t, h, project, sid("PLAN_VALIDATE"), "SUBMIT", 2)
	p639Walk(t, h, project, sid("WORK_UNIT_PREPARE"), "ACCEPTED", 3)
	p639Walk(t, h, project, sid("IMPLEMENT"), "READY", 4)
	wantHistory = append(wantHistory, "PROCEED", "SUBMIT", "ACCEPTED", "READY")

	// ---- INCOMPLETE @ 45% / threshold 80% -> CONTINUE_CURRENT_SESSION ----
	if tk, e := state.ResolveContextThreshold(45, 80); e != nil || tk != state.OutcomeContinueCurrentSession {
		t.Fatalf("resolver(45,80) = (%q, %v), want CONTINUE_CURRENT_SESSION", tk, e)
	}
	p639Walk(t, h, project, sid("RESPONSE_EVALUATE"), "EVALUATE", 5)
	p639Walk(t, h, project, sid("CONTINUE_DECISION"), "INCOMPLETE", 6)
	p639Walk(t, h, project, sid("SAME_SESSION"), "SAME", 7)
	p639Walk(t, h, project, sid("CONTINUE"), "CONTINUE", 8)
	p639Walk(t, h, project, sid("IMPLEMENT"), "PROCEED", 9)
	wantHistory = append(wantHistory, "EVALUATE", "INCOMPLETE", "SAME", "CONTINUE", "PROCEED")

	// ---- INCOMPLETE @ 90% -> RESET_SESSION, then CONTINUE -> IMPLEMENT ----
	if tk, e := state.ResolveContextThreshold(90, 80); e != nil || tk != state.OutcomeResetSession {
		t.Fatalf("resolver(90,80) = (%q, %v), want RESET_SESSION", tk, e)
	}
	p639Walk(t, h, project, sid("RESPONSE_EVALUATE"), "EVALUATE", 10)
	p639Walk(t, h, project, sid("CONTINUE_DECISION"), "INCOMPLETE", 11)
	p639Walk(t, h, project, sid("RESET_SESSION"), "RESET", 12)
	p639Walk(t, h, project, sid("CONTINUE"), "CONTINUE", 13)
	p639Walk(t, h, project, sid("IMPLEMENT"), "PROCEED", 14)
	wantHistory = append(wantHistory, "EVALUATE", "INCOMPLETE", "RESET", "CONTINUE", "PROCEED")

	// ---- RESTART CHECKPOINT 1: at IMPLEMENT (mid Unit A) ----
	pos := p639Capture(t, h, project, phase, unitA, unitB)
	if pos.CurrentStateKey != "IMPLEMENT" {
		t.Fatalf("pre-restart state = %q, want IMPLEMENT", pos.CurrentStateKey)
	}
	h = p639Restart(t, base, dbName, "IMPLEMENT", h, project, phase, unitA, unitB, pos)

	// ---- Complete Unit A: COMPLETE -> VALIDATE PASS -> UNIT_DONE ----
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'EXECUTING' WHERE id = '"+unitA+"'"); err != nil {
		t.Fatalf("bookkeeping: %v", err)
	}
	p639Walk(t, h, project, sid("RESPONSE_EVALUATE"), "EVALUATE", 15)
	p639Walk(t, h, project, sid("VALIDATE"), "COMPLETE", 16)
	p639Walk(t, h, project, sid("VALIDATION_RESULT"), "EVALUATE", 17)
	p639Walk(t, h, project, sid("PASS"), "PASS", 18)
	p639Walk(t, h, project, sid("UNIT_DONE"), "PROCEED", 19)
	p639Walk(t, h, project, sid("MORE_WORK"), "EVALUATE", 20)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'DONE' WHERE id = '"+unitA+"'"); err != nil {
		t.Fatalf("bookkeeping: %v", err)
	}
	if got := state.ResolveRemainingWork(p613UnitFacts(t, h, p633UUID(t, project))); got != state.OutcomeNextUnit {
		t.Fatalf("remaining-work resolver = %q, want NEXT_UNIT", got)
	}
	// The template encodes the MORE_WORK fork with "YES"/"NO" tokens (WBS
	// 6.3 data) for the resolver's NEXT_UNIT / PHASE_VALIDATE outcomes;
	// the explicit rebind to Unit B rides on the fork step.
	rebindB := unitB
	p639Fork(t, h, project, sid("NEXT_UNIT"), "YES", &rebindB, 21)
	p639Walk(t, h, project, sid("WORK_UNIT_PREPARE"), "PROCEED", 22)
	wantHistory = append(wantHistory, "EVALUATE", "COMPLETE", "EVALUATE", "PASS", "PROCEED", "EVALUATE", "YES", "PROCEED")

	// ---- Unit B: prepare -> implement -> complete -> unit done ----
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'EXECUTING' WHERE id = '"+unitB+"'"); err != nil {
		t.Fatalf("bookkeeping: %v", err)
	}
	p639Walk(t, h, project, sid("IMPLEMENT"), "READY", 23)
	p639Walk(t, h, project, sid("RESPONSE_EVALUATE"), "EVALUATE", 24)
	p639Walk(t, h, project, sid("VALIDATE"), "COMPLETE", 25)
	p639Walk(t, h, project, sid("VALIDATION_RESULT"), "EVALUATE", 26)
	p639Walk(t, h, project, sid("PASS"), "PASS", 27)
	p639Walk(t, h, project, sid("UNIT_DONE"), "PROCEED", 28)
	p639Walk(t, h, project, sid("MORE_WORK"), "EVALUATE", 29)
	if _, err := h.DBExec(ctx, "UPDATE work_units SET status = 'DONE' WHERE id = '"+unitB+"'"); err != nil {
		t.Fatalf("bookkeeping: %v", err)
	}
	if got := state.ResolveRemainingWork(p613UnitFacts(t, h, p633UUID(t, project))); got != state.OutcomePhaseValidate {
		t.Fatalf("remaining-work resolver = %q, want PHASE_VALIDATE", got)
	}
	p639Fork(t, h, project, sid("PHASE_VALIDATE"), "NO", nil, 30)
	wantHistory = append(wantHistory, "READY", "EVALUATE", "COMPLETE", "EVALUATE", "PASS", "PROCEED", "EVALUATE", "NO")

	// ---- RESTART CHECKPOINT 2: at PHASE_VALIDATE ----
	pos = p639Capture(t, h, project, phase, unitA, unitB)
	if pos.CurrentStateKey != "PHASE_VALIDATE" {
		t.Fatalf("pre-restart state = %q, want PHASE_VALIDATE", pos.CurrentStateKey)
	}
	h = p639Restart(t, base, dbName, "PHASE_VALIDATE", h, project, phase, unitA, unitB, pos)

	// ---- PHASE_VALIDATE PASS -> PHASE_DONE -> MORE_PHASES NO -> PROJECT_DONE ----
	p639Walk(t, h, project, sid("PHASE_DONE"), "COMPLETE", 31)
	p639Walk(t, h, project, sid("MORE_PHASES"), "EVALUATE", 32)
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'DONE' WHERE project_id = '"+project+"'"); err != nil {
		t.Fatalf("bookkeeping: %v", err)
	}
	if got := state.ResolveRemainingPhases(p613PhaseFacts(t, h, p633UUID(t, project))); got != state.OutcomeProjectDone {
		t.Fatalf("remaining-phase resolver = %q, want PROJECT_DONE", got)
	}
	// P16-78: the last phase leads through final hardening, final
	// validation and the final checkpoint before PROJECT_DONE.
	p639Fork(t, h, project, sid("FINAL_HARDENING"), "NO", nil, 33)
	p639Walk(t, h, project, sid("FINAL_VALIDATE"), "COMPLETE", 34)
	p639Walk(t, h, project, sid("FINAL_CHECKPOINT"), "PASS", 35)
	p639Walk(t, h, project, sid("PROJECT_DONE"), "PROCEED", 36)
	wantHistory = append(wantHistory, "COMPLETE", "EVALUATE", "NO", "COMPLETE", "PASS", "PROCEED")

	// ---- RESTART CHECKPOINT 3: at PROJECT_DONE (terminal) ----
	pos = p639Capture(t, h, project, phase, unitA, unitB)
	if pos.CurrentStateKey != "PROJECT_DONE" {
		t.Fatalf("pre-restart state = %q, want PROJECT_DONE", pos.CurrentStateKey)
	}
	h = p639Restart(t, base, dbName, "PROJECT_DONE", h, project, phase, unitA, unitB, pos)

	// Terminal: no outgoing transitions from PROJECT_DONE (direct SQL).
	var outCount int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_transitions WHERE from_state_id = ('"+sid("PROJECT_DONE")+"')::uuid AND enabled", &outCount); err != nil {
		t.Fatalf("count outgoing: %v", err)
	}
	if outCount != 0 {
		t.Fatalf("PROJECT_DONE outgoing transitions = %d, want 0 (terminal)", outCount)
	}

	// ---- Persisted history: the full applied sequence, in order ----
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_transition_history WHERE project_id = '"+project+"'", &n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if n != len(wantHistory) {
		t.Fatalf("history rows = %d, want %d", n, len(wantHistory))
	}
	for i, want := range wantHistory {
		var got string
		q := "SELECT outcome FROM (SELECT outcome FROM state_transition_history WHERE project_id = '" + project + "' ORDER BY seq) x LIMIT 1 OFFSET " + fmt.Sprint(i)
		if err := h.DBScan(ctx, q, &got); err != nil {
			t.Fatalf("read history row %d: %v", i+1, err)
		}
		if got != want {
			t.Fatalf("history row %d outcome = %q, want %q", i+1, got, want)
		}
	}

	// ---- Terminal: further transitions rejected, nothing written ----
	for _, outcome := range []string{"PROCEED", "YES", "NO", "EVALUATE", "CONTINUE"} {
		prev, err := p632Preview(h).Preview(ctx, project, &service.PreviewTransitionInput{Outcome: outcome})
		if err != nil {
			t.Fatalf("preview(%s): %v", outcome, err)
		}
		if prev.Selected != nil {
			t.Fatalf("outcome %q selected a transition from PROJECT_DONE: %+v", outcome, prev.Selected)
		}
	}
	stID, ver := p632Runtime(t, h, project)
	if stID != sid("PROJECT_DONE") || ver != 37 {
		t.Fatalf("runtime after rejected attempts = (%s, %d), want unchanged (%s, 37)", stID, ver, sid("PROJECT_DONE"))
	}
	if got := p632HistoryCount(t, h, project); got != len(wantHistory) {
		t.Fatalf("history advanced after rejected attempts (%d -> %d)", len(wantHistory), got)
	}

	// ---- All data retained at the terminal state ----
	final := p639Capture(t, h, project, phase, unitA, unitB)
	if final.PhaseStatus != "DONE" || final.UnitAStatus != "DONE" || final.UnitBStatus != "DONE" {
		t.Fatalf("retained data = phase %q unitA %q unitB %q, want all DONE", final.PhaseStatus, final.UnitAStatus, final.UnitBStatus)
	}
	if final.HistoryCount != len(wantHistory) || final.MaxSeq != len(wantHistory) {
		t.Fatalf("retained history = (%d, %d), want (%d, %d)", final.HistoryCount, final.MaxSeq, len(wantHistory), len(wantHistory))
	}
}
