// P6-16 (WBS 6.19): the state-machine validation service driven against
// REAL persisted machine/state/transition/role rows. The pure check logic
// lives in internal/state (P6-14/P6-15); the service aggregates every WBS
// 6.19 check over the persisted definition, returns ALL practical errors at
// once with structured codes, and never mutates the machine.
package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
	"overseer/internal/state"
)

// p616Svc wires the validation service over the harness pool (the nil-txPool
// sequential path; the op is read-only).
func p616Svc(t *testing.T, h *Harness) *service.StateMachineValidationService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewStateMachineValidationService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewRoleAllowedStateRepository(pool),
	)
}

// p616ValidMachine builds a VALID dev-workflow machine (START -> IMPLEMENT
// -> VALIDATE -> DONE): initial set, unique keys, a valid enabled
// same-project role assignment whose allowed states cover the state,
// template references on every action/validation state, distinct
// priorities, no terminal outgoing, full reachability, and no
// zero-progress cycles.
func p616ValidMachine(t *testing.T, h *Harness, proj, name, roleKey string) string {
	t.Helper()
	mid, _, st := p615DevMachineFixture(t, h, proj, name)
	role := p607Role(t, h, proj, roleKey)
	if _, err := h.DBExec(context.Background(), "INSERT INTO role_allowed_states (id, role_id, state_key, created_at) VALUES (gen_random_uuid(), '"+role+"', 'IMPLEMENT', now())"); err != nil {
		t.Fatalf("seed allowed state: %v", err)
	}
	if _, err := h.DBExec(context.Background(), "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+st["IMPLEMENT"].ID.String()+"', '"+role+"', 0, now(), now())"); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}
	for _, k := range []string{"IMPLEMENT", "VALIDATE"} {
		if _, err := h.DBExec(context.Background(), "UPDATE workflow_states SET prompt_template = '"+k+"' WHERE machine_id = '"+mid.String()+"' AND key = '"+k+"'"); err != nil {
			t.Fatalf("seed template: %v", err)
		}
	}
	if _, err := h.DBExec(context.Background(), "UPDATE state_machines SET initial_state_id = '"+st["START"].ID.String()+"' WHERE id = '"+mid.String()+"'"); err != nil {
		t.Fatalf("set initial: %v", err)
	}
	return mid.String()
}

// p616MachineSnapshot captures the full persisted definition (machine row +
// every state + every transition) for the non-mutation assertion.
func p616MachineSnapshot(t *testing.T, h *Harness, machineID string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	var ver, enabled int
	var initial string
	if err := h.DBScan(ctx, "SELECT version, enabled::int, coalesce(initial_state_id::text, '') FROM state_machines WHERE id = '"+machineID+"'", &ver, &enabled, &initial); err != nil {
		t.Fatalf("snapshot machine: %v", err)
	}
	out["machine"] = itoa(ver) + "|" + itoa(enabled) + "|" + initial
	var ns int
	if err := h.DBScan(ctx, "SELECT count(*) FROM workflow_states WHERE machine_id = '"+machineID+"'", &ns); err != nil {
		t.Fatalf("count states: %v", err)
	}
	out["state_count"] = itoa(ns)
	var agg string
	if err := h.DBScan(ctx, "SELECT coalesce(string_agg(key||'|'||prompt_template||'|'||enabled::text, ',' ORDER BY key), '') FROM workflow_states WHERE machine_id = '"+machineID+"'", &agg); err != nil {
		t.Fatalf("aggregate states: %v", err)
	}
	out["states"] = agg
	var nt int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_transitions WHERE machine_id = '"+machineID+"'", &nt); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	out["transition_count"] = itoa(nt)
	if err := h.DBScan(ctx, "SELECT coalesce(string_agg(outcome||'|'||priority::text||'|'||enabled::text, ',' ORDER BY ctid), '') FROM state_transitions WHERE machine_id = '"+machineID+"'", &agg); err != nil {
		t.Fatalf("aggregate transitions: %v", err)
	}
	out["transitions"] = agg
	return out
}

func p616AssertSnapshotEqual(t *testing.T, h *Harness, machineID string, before map[string]string) {
	t.Helper()
	after := p616MachineSnapshot(t, h, machineID)
	for k := range before {
		if before[k] != after[k] {
			t.Fatalf("validation mutated machine: field %q before=%q after=%q", k, before[k], after[k])
		}
	}
}

// T6.19.1 — A valid machine passes: the report is clean and deterministic
// (two calls return identical reports).
func TestP616ValidMachinePasses(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid := p616ValidMachine(t, h, proj, "p616-valid", "P616-VALID-ROLE")
	svc := p616Svc(t, h)
	rep, err := svc.Validate(context.Background(), mid)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !rep.Valid {
		t.Fatalf("valid machine reported invalid: %+v", rep.Errors)
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("expected zero errors, got %+v", rep.Errors)
	}
	rep2, err := svc.Validate(context.Background(), mid)
	if err != nil {
		t.Fatalf("validate again: %v", err)
	}
	if len(rep2.Errors) != len(rep.Errors) {
		t.Fatalf("report length changed between calls: %d vs %d", len(rep2.Errors), len(rep.Errors))
	}
	for i := range rep.Errors {
		if rep.Errors[i].Code != rep2.Errors[i].Code || rep.Errors[i].Detail != rep2.Errors[i].Detail {
			t.Fatalf("report not deterministic at %d", i)
		}
	}
}

// T6.19.2 — Multiple INDEPENDENT errors are returned at once (no fail-fast):
// one machine carrying a template violation, a disabled-role assignment, a
// dead-end, and an orphan yields every code in a single report.
func TestP616MultipleErrorsAtOnce(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, _, st := p615DevMachineFixture(t, h, proj, "p616-multi")
	// Template violation: every template cleared (the two action/validation
	// states then require one).
	if _, err := h.DBExec(context.Background(), "UPDATE workflow_states SET prompt_template = '' WHERE machine_id = '"+mid.String()+"'"); err != nil {
		t.Fatalf("clear templates: %v", err)
	}
	// Disabled-role assignment (WBS 6.4.3 configuration rule).
	disabled := p607Role(t, h, proj, "P616-DISABLED")
	if _, err := h.DBExec(context.Background(), "UPDATE roles SET enabled = false WHERE id = '"+disabled+"'"); err != nil {
		t.Fatalf("disable role: %v", err)
	}
	if _, err := h.DBExec(context.Background(), "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+st["START"].ID.String()+"', '"+disabled+"', 0, now(), now())"); err != nil {
		t.Fatalf("seed disabled assignment: %v", err)
	}
	// Dead-end + orphan: two new enabled CONTROL states with no edges
	// (control kinds need no template, keeping the template count exact).
	p615StateFixture(t, h, mid, "STUCK2", state.KindControl, false, true)
	p615StateFixture(t, h, mid, "ORB", state.KindControl, false, true)
	if _, err := h.DBExec(context.Background(), "UPDATE state_machines SET initial_state_id = '"+st["START"].ID.String()+"' WHERE id = '"+mid.String()+"'"); err != nil {
		t.Fatalf("set initial: %v", err)
	}
	svc := p616Svc(t, h)
	rep, err := svc.Validate(context.Background(), mid.String())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.Valid {
		t.Fatal("machine with violations reported valid")
	}
	codes := map[string]int{}
	for _, e := range rep.Errors {
		codes[e.Code]++
	}
	want := map[string]int{
		"template_missing":   2, // IMPLEMENT + VALIDATE
		"role_allowed_state": 1, // the disabled role does not allow START
		"role_disabled":      1,
		"dead_end":           2, // STUCK2 + ORB (no outgoing enabled transition)
		"reachability":       2, // STUCK2 + ORB (no path from initial)
	}
	for code, n := range want {
		if codes[code] != n {
			t.Fatalf("code %q count = %d, want %d (all codes: %v)", code, codes[code], n, codes)
		}
	}
	if total := len(rep.Errors); total != 8 {
		t.Fatalf("expected 8 independent errors at once, got %d: %+v", total, rep.Errors)
	}
}

// T6.19.3 — Validation is non-mutating: the persisted machine definition is
// byte-identical before and after validating an invalid machine.
func TestP616ValidationDoesNotMutate(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, _, st := p615DevMachineFixture(t, h, proj, "p616-nomut")
	p615StateFixture(t, h, mid, "GHOST", state.KindAction, false, true)
	if _, err := h.DBExec(context.Background(), "UPDATE workflow_states SET prompt_template = '' WHERE machine_id = '"+mid.String()+"'"); err != nil {
		t.Fatalf("clear template: %v", err)
	}
	if _, err := h.DBExec(context.Background(), "UPDATE state_machines SET initial_state_id = '"+st["START"].ID.String()+"' WHERE id = '"+mid.String()+"'"); err != nil {
		t.Fatalf("set initial: %v", err)
	}
	before := p616MachineSnapshot(t, h, mid.String())
	svc := p616Svc(t, h)
	rep, err := svc.Validate(context.Background(), mid.String())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if rep.Valid {
		t.Fatal("expected an invalid machine")
	}
	p616AssertSnapshotEqual(t, h, mid.String(), before)
}

// T6.19.4 — An invalid machine cannot become active: the activation gate
// accepts a clean machine and rejects an invalid one; the machine's Enabled
// flag (the activation surface at this stage — the REST surface is P6-25+)
// is also enforced by the gate.
func TestP616InvalidCannotBecomeActive(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	ctx := context.Background()
	mid := p616ValidMachine(t, h, proj, "p616-active", "P616-ACTIVE-ROLE")
	svc := p616Svc(t, h)
	if rep, err := svc.CanActivate(ctx, mid); err != nil {
		t.Fatalf("activation gate on clean machine: %v", err)
	} else if !rep.Valid {
		t.Fatalf("clean machine failed the activation gate: %+v", rep.Errors)
	}
	// Invalidate it: add an unreachable state.
	midUUID := mustParse(t, mid)
	p615StateFixture(t, h, midUUID, "GHOST2", state.KindControl, false, true)
	if rep, err := svc.CanActivate(ctx, mid); err != nil {
		t.Fatalf("activation gate on invalid machine: %v", err)
	} else if rep.Valid {
		t.Fatal("invalid machine passed the activation gate")
	}
	// Disabled machine: the gate rejects it even when its definition is clean.
	mid2 := p616ValidMachine(t, h, proj, "p616-disabled", "P616-DISABLED2-ROLE")
	if _, err := h.DBExec(ctx, "UPDATE state_machines SET enabled = false WHERE id = '"+mid2+"'"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if rep, err := svc.CanActivate(ctx, mid2); err != nil {
		t.Fatalf("activation gate on disabled machine: %v", err)
	} else if rep.Valid {
		t.Fatal("disabled machine passed the activation gate")
	}
}
