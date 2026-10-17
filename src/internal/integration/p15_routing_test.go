package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

func (r *p15Rig) runtimeKey(t *testing.T, execID uuid.UUID) (string, *uuid.UUID) {
	t.Helper()
	rt, err := r.svc().RuntimeForExecution(context.Background(), r.pid, execID)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	return r.stateKey(t, rt.CurrentStateID), rt.WorkUnitID
}

// TestP15114PartialProject — WBS 15.114 / 15.161 / 15.64 / 15.65: A/B DONE,
// C READY, D depends on C → phase 2, unit C, path through
// WORK_UNIT_PREPARE to IMPLEMENT; A/B untouched, D not selected.
func TestP15114PartialProject(t *testing.T) {
	r := p15NewRigWith(t, []p15Phase{{1, "Foundation", "DONE", 1}, {2, "Persistence", "EXECUTING", 5}, {3, "API", "PENDING", 1}}, nil)
	plan := r.plan(t, 2, 5, "EXECUTING")
	a := r.unit(t, plan, 1, "A", "DONE")
	b := r.unit(t, plan, 2, "B", "DONE")
	c := r.unit(t, plan, 3, "C", "READY")
	d := r.unit(t, plan, 4, "D", "PENDING")
	r.depend(t, d, c)
	before := r.boundarySnapshot(t)
	e := r.bootstrap(t).Execution
	if e.InitialPhaseID != r.phases[2] || e.PlanState != execution.PlanCurrent || e.PlanID == nil || *e.PlanID != plan {
		t.Fatalf("phase/plan %+v", e)
	}
	if e.InitialWorkUnitID == nil || *e.InitialWorkUnitID != c || e.FirstActionableStateKey != "IMPLEMENT" || e.InitialRoleKey != "ENGINEER" {
		t.Fatalf("unit / state %+v", e)
	}
	key, unit := r.runtimeKey(t, e.ID)
	if key != "IMPLEMENT" || unit == nil || *unit != c {
		t.Fatalf("runtime %s %v", key, unit)
	}
	var outcomes string
	if err := r.h.DBScan(context.Background(), "SELECT string_agg(outcome, ',' ORDER BY seq) FROM state_transition_history WHERE execution_id = '"+e.ID.String()+"'", &outcomes); err != nil {
		t.Fatal(err)
	}
	if outcomes != "BOOTSTRAP,PLAN_READY,READY" {
		t.Fatalf("path %q", outcomes)
	}
	// ACTIVE_TASK exists for C only.
	if n := r.count(t, "SELECT count(*) FROM state_files f JOIN state_file_types ty ON ty.id = f.type_id WHERE ty.key = 'ACTIVE_TASK' AND f.work_unit_id = '"+c.String()+"'"); n != 1 {
		t.Fatalf("ACTIVE_TASK for C = %d", n)
	}
	if n := r.count(t, "SELECT count(*) FROM state_files f JOIN state_file_types ty ON ty.id = f.type_id WHERE ty.key = 'ACTIVE_TASK' AND f.work_unit_id IN ('"+a.String()+"','"+b.String()+"','"+d.String()+"')"); n != 0 {
		t.Fatal("ACTIVE_TASK fabricated for another unit")
	}
	// TASK_ARCHIVE lists only DONE units (A, B).
	var archive string
	if err := r.h.DBScan(context.Background(), "SELECT v.content FROM state_file_versions v JOIN state_files f ON f.id = v.state_file_id JOIN state_file_types ty ON ty.id = f.type_id WHERE f.project_id = '"+r.pid.String()+"' AND ty.key = 'TASK_ARCHIVE'", &archive); err != nil {
		t.Fatal(err)
	}
	if !containsAll(archive, "A", "B") || containsAny(archive, "Deliver C", "Deliver D") {
		t.Fatalf("archive:\n%s", archive)
	}
	if after := r.boundarySnapshot(t); after != before {
		t.Fatal("A/B/C/D or plan changed")
	}
	r.noWorker(t)
}

// TestP15117AllUnitsDone — WBS 15.117 / 15.66: routes to PHASE_VALIDATE, no
// implementation task fabricated.
func TestP15117AllUnitsDone(t *testing.T) {
	r := p15NewRigWith(t, []p15Phase{{1, "Engine", "EXECUTING", 2}}, nil)
	plan := r.plan(t, 1, 2, "EXECUTING")
	r.unit(t, plan, 1, "A", "DONE")
	r.unit(t, plan, 2, "B", "DONE")
	e := r.bootstrap(t).Execution
	if e.FirstActionableStateKey != "PHASE_VALIDATE" || e.InitialWorkUnitID != nil || e.InitialRoleKey != "TESTER" {
		t.Fatalf("%+v", e)
	}
	if n := r.count(t, "SELECT count(*) FROM state_files f JOIN state_file_types ty ON ty.id = f.type_id WHERE f.project_id = '"+r.pid.String()+"' AND ty.key = 'ACTIVE_TASK'"); n != 0 {
		t.Fatal("ACTIVE_TASK fabricated")
	}
}

// TestP15118AppendedPhase — WBS 15.118 / 15.68.
func TestP15118AppendedPhase(t *testing.T) {
	r := p15NewRigWith(t, []p15Phase{{1, "A", "DONE", 1}, {2, "B", "DONE", 1}, {3, "C", "DONE", 1}, {4, "Appended", "PENDING", 1}}, nil)
	e := r.bootstrap(t).Execution
	if e.InitialPhaseID != r.phases[4] {
		t.Fatal("appended phase not current")
	}
}

// TestP15119CompletedProject — WBS 15.119 / 15.23: completion-eligible, no
// execution, no worker activity.
func TestP15119CompletedProject(t *testing.T) {
	r := p15NewRigWith(t, []p15Phase{{1, "A", "DONE", 1}, {2, "B", "DONE", 1}}, nil)
	res, be := r.bootstrapErr(t)
	if be.Code != execution.CodeNoPhase || res.Readiness == nil || !res.Readiness.CompletionEligible {
		t.Fatalf("%+v %+v", be, res.Readiness)
	}
	if n := r.count(t, "SELECT count(*) FROM executions WHERE "+r.where()); n != 0 {
		t.Fatal("execution created")
	}
	if n := r.count(t, "SELECT count(*) FROM runtime_state WHERE "+r.where()); n != 0 {
		t.Fatal("runtime created")
	}
	r.noWorker(t)
}

// TestP15120CustomWorkflow — WBS 15.120 / 15.30: a custom machine
// BOOT → INSPECT → PLAN boots through its own control states.
func TestP15120CustomWorkflow(t *testing.T) {
	r := p15NewRig(t)
	ctx := context.Background()
	defs := []serviceMachineState{{"BOOT", "CONTROL"}, {"INSPECT", "CONTROL"}, {"PLAN", "ACTION"}, {"END", "TERMINAL"}}
	mid := r.customMachine(t, "custom", defs, [][3]string{{"BOOT", "PROCEED", "INSPECT"}, {"INSPECT", "PROCEED", "PLAN"}, {"PLAN", "SUBMIT", "END"}}, "BOOT", map[string]string{"PLAN": "PLAN_PHASE"})
	_ = mid
	e := r.bootstrap(t).Execution
	if e.InitialStateKey != "BOOT" || e.FirstActionableStateKey != "PLAN" {
		t.Fatalf("%+v", e)
	}
	var outcomes string
	if err := r.h.DBScan(ctx, "SELECT string_agg(outcome, ',' ORDER BY seq) FROM state_transition_history WHERE execution_id = '"+e.ID.String()+"'", &outcomes); err != nil {
		t.Fatal(err)
	}
	if outcomes != "BOOTSTRAP,PROCEED,PROCEED" {
		t.Fatalf("path %q", outcomes)
	}
	_ = state.KindControl
}

// TestP15115And116Plans — WBS 15.115 / 15.116 / 15.162 / 15.26 / 15.27:
// missing plan → PHASE_PLAN; stale plan (WBS v8, plan from v7) →
// PHASE_PLAN with WBS v8 captured, plan untouched; current plan reused.
func TestP15115And116Plans(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		r := p15NewRigWith(t, []p15Phase{{1, "Phase", "EXECUTING", 8}}, nil)
		plan := r.plan(t, 1, 7, "READY")
		r.unit(t, plan, 1, "A", "READY")
		before := r.boundarySnapshot(t)
		e := r.bootstrap(t).Execution
		if e.PlanState != execution.PlanStale || e.FirstActionableStateKey != "PHASE_PLAN" || e.WBSVersion != 8 || e.InitialWorkUnitID != nil {
			t.Fatalf("%+v", e)
		}
		if after := r.boundarySnapshot(t); after != before {
			t.Fatal("plan mutated")
		}
	})
	t.Run("current", func(t *testing.T) {
		r := p15NewRigWith(t, []p15Phase{{1, "Phase", "EXECUTING", 3}}, nil)
		plan := r.plan(t, 1, 3, "READY")
		u := r.unit(t, plan, 1, "A", "PENDING")
		e := r.bootstrap(t).Execution
		if e.PlanState != execution.PlanCurrent || e.FirstActionableStateKey != "IMPLEMENT" || *e.InitialWorkUnitID != u {
			t.Fatalf("%+v", e)
		}
	})
}

func containsAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

type serviceMachineState struct{ key, kind string }

// customMachine seeds a project machine at version 2 (so it is the
// project's current machine over the copied default v1), with templates,
// protocol references and PLANNER assignments for prompted states.
func (r *p15Rig) customMachine(t *testing.T, name string, states []serviceMachineState, trs [][3]string, initial string, tmpl map[string]string) uuid.UUID {
	return r.customMachineOpt(t, name, states, trs, initial, tmpl, true)
}

// customMachineOpt: withRoles=false leaves role configuration untouched.
func (r *p15Rig) customMachineOpt(t *testing.T, name string, states []serviceMachineState, trs [][3]string, initial string, tmpl map[string]string, withRoles bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	raw := r.h.App.DB().Raw()
	now := time.Now().UTC()
	mid := uuid.New()
	ids := map[string]uuid.UUID{}
	if err := dbrepo.NewStateMachineRepository(raw).Create(ctx, &persistence.MachineRecord{ID: mid, ProjectID: r.pid, FamilyID: mid, Name: name,
		Version: 2, Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		ids[s.key] = uuid.New()
		if err := dbrepo.NewWorkflowStateRepository(raw).Create(ctx, &persistence.WorkflowStateRecord{ID: ids[s.key], MachineID: mid, Version: 2,
			Key: s.key, Name: s.key, Kind: s.kind, Terminal: s.kind == "TERMINAL", PromptTemplate: tmpl[s.key], Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("state %s: %v", s.key, err)
		}
	}
	if err := dbrepo.NewStateMachineRepository(raw).SetInitialState(ctx, mid, ids[initial]); err != nil {
		t.Fatal(err)
	}
	for _, tr := range trs {
		if err := dbrepo.NewStateTransitionRepository(raw).Create(ctx, &persistence.StateTransitionRecord{ID: uuid.New(), MachineID: mid, Version: 2,
			FromStateID: ids[tr[0]], ToStateID: ids[tr[2]], Outcome: tr[1], Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := r.h.App.Results().EnsureProtocol(ctx, r.pid)
	if err != nil {
		t.Fatal(err)
	}
	for key := range tmpl {
		r.states[key] = ids[key]
		if _, err := dbrepo.NewResponseProtocolRepository(raw).CreateStateReference(ctx, ids[key], ref.ProtocolID, ref.Version); err != nil {
			t.Fatal(err)
		}
		if withRoles {
			r.assign(t, key, "PLANNER")
		}
	}
	if !withRoles {
		return mid
	}
	keys := []string{"PHASE_PLAN"}
	for key := range tmpl {
		keys = append(keys, key)
	}
	if _, err := p718RoleSvc(t, r.h).SetAllowedStates(ctx, r.roles["PLANNER"].String(), keys); err != nil {
		t.Fatal(err)
	}
	return mid
}
