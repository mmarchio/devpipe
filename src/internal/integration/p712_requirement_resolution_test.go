package integration

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P7-12 (WBS 7.17): state-file requirement resolution. The
// StateFileRequirementResolutionService wraps the Phase 6 pure primitive
// (state.ResolveStateFileRequirements, union-dedup) end-to-end over persisted
// role + state requirement data:
//
//   - ROLE sources: the project-scoped roles (ListByScope(project)), each
//     role's state-file requirements in stored position order. Roles are
//     visited in repository order (key order); the primitive then keeps
//     first-seen order, so category order is (role key, position).
//   - STATE sources: the project's CURRENT machine version's states (the
//     P6-26 resolveCurrent rule: highest version, tie-broken by smallest
//     family id), each state's required categories in stored ordinal order.
//     States are visited in repository order (key order); category order is
//     (state key, ordinal).
//
// Union rule (the primitive's declared rule, restated): roles first, states
// second, each category appearing ONCE in first-seen order (a role
// requirement takes precedence over a state one with the same category).
//
// T7.17.6 DISABLED-REQUIREMENT SEMANTICS (stated here so TESTER/PM can
// verify the chosen reading): a DISABLED SOURCE CONTRIBUTES NOTHING — a
// disabled role contributes none of its state-file requirements; a disabled
// state contributes none of its required categories; a disabled current
// machine contributes no state requirements at all (the requirement rows
// stay persisted, but the resolved set is computed from enabled sources
// only).
//
// Service-only read model (no REST surface in this task); the integration
// tests drive the op over the harness pool.

// p712Svc wires the requirement-resolution service against the harness pool.
func p712Svc(t *testing.T, h *Harness) *service.StateFileRequirementResolutionService {
	t.Helper()
	raw := h.App.DB().Raw()
	return service.NewStateFileRequirementResolutionService(
		dbrepo.NewRoleRepository(raw),
		dbrepo.NewRoleStateFileRepository(raw),
		dbrepo.NewStateMachineRepository(raw),
		dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewStateFileRequirementRepository(raw),
	)
}

// p712ProjectRole creates a project-scoped role through the real service and
// returns its id.
func p712ProjectRole(t *testing.T, h *Harness, projectID uuid.UUID, key string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()
	svc := service.NewRoleService(
		dbrepo.NewRoleRepository(raw),
		dbrepo.NewSystemPromptRepository(raw),
	).WithStateFiles(dbrepo.NewRoleStateFileRepository(raw))
	pid := projectID.String()
	rec, err := svc.CreateRole(ctx, &pid, key, key, "", "prompt")
	if err != nil {
		t.Fatalf("create role %s: %v", key, err)
	}
	return rec.ID
}

// p712State seeds one machine (v1, enabled) for the project if none exists
// and adds one enabled state with the given key, returning the state id.
// p712ProjectID creates a project with a phase and work unit (shared rig).
func p712ProjectID(t *testing.T, h *Harness) uuid.UUID {
	t.Helper()
	return uuid.MustParse(p605Project(t, h))
}

func p712State(t *testing.T, h *Harness, projectID uuid.UUID, key string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var machineID uuid.UUID
	var machineCount int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_machines WHERE project_id = '"+projectID.String()+"'", &machineCount); err != nil {
		t.Fatalf("count machines: %v", err)
	}
	if machineCount == 0 {
		if err := h.DBScan(ctx,
			`INSERT INTO state_machines (id, project_id, family_id, name, description, version, enabled, created_at, updated_at)
			 VALUES (gen_random_uuid(), '`+projectID.String()+`', gen_random_uuid(), 'p712-machine', '', 1, true, now(), now())
			 RETURNING id`, &machineID); err != nil {
			t.Fatalf("seed machine: %v", err)
		}
	} else {
		if err := h.DBScan(ctx, "SELECT id FROM state_machines WHERE project_id = '"+projectID.String()+"' LIMIT 1", &machineID); err != nil {
			t.Fatalf("scan machine: %v", err)
		}
	}
	var id uuid.UUID
	if err := h.DBScan(ctx,
		`INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at)
		 VALUES (gen_random_uuid(), '`+machineID.String()+`', '`+key+`', '`+key+`', '', 'ACTION', false, true, now(), now())
		 RETURNING id`, &id); err != nil {
		t.Fatalf("seed state %s: %v", key, err)
	}
	return id
}

func p712RequireRoleFiles(t *testing.T, h *Harness, roleID uuid.UUID, files []string) {
	t.Helper()
	ctx := context.Background()
	pool := h.App.DB().Raw()
	svc := service.NewRoleService(
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
	).WithStateFiles(dbrepo.NewRoleStateFileRepository(pool))
	if _, err := svc.SetStateFiles(ctx, roleID.String(), files); err != nil {
		t.Fatalf("set role state files: %v", err)
	}
}

func p712RequireStateFiles(t *testing.T, h *Harness, stateID uuid.UUID, categories []string) {
	t.Helper()
	ctx := context.Background()
	repo := dbrepo.NewStateFileRequirementRepository(h.App.DB().Raw())
	for _, c := range categories {
		if _, err := repo.Require(ctx, stateID, c); err != nil {
			t.Fatalf("require state file %s: %v", c, err)
		}
	}
}

// T7.17.1 — Role requirements resolved: the op returns exactly the
// project's role-declared state-file categories, in stored (position) order.
func TestP712RoleRequirementsResolved(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := p712ProjectID(t, h)
	role := p712ProjectRole(t, h, projectID, "p712r1")
	p712RequireRoleFiles(t, h, role, []string{"PROJECT_STATE", "ARCHITECTURE"})

	got, err := p712Svc(t, h).ResolveStateFileRequirements(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"PROJECT_STATE", "ARCHITECTURE"}
	if !slices.Equal(got, want) {
		t.Fatalf("resolved = %v, want %v", got, want)
	}
}

// T7.17.2 — State requirements resolved: the op returns exactly the
// current machine's enabled state-declared categories, in (state key,
// ordinal) order.
func TestP712StateRequirementsResolved(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := p712ProjectID(t, h)
	st := p712State(t, h, projectID, "IMPLEMENT")
	p712RequireStateFiles(t, h, st, []string{"PHASE_STATE", "ACTIVE_TASK"})

	got, err := p712Svc(t, h).ResolveStateFileRequirements(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"PHASE_STATE", "ACTIVE_TASK"}
	if !slices.Equal(got, want) {
		t.Fatalf("resolved = %v, want %v", got, want)
	}
}

// T7.17.3 — The union contains BOTH the role-declared and the
// state-declared categories (the WBS 7.17 example, end to end).
func TestP712UnionContainsBoth(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := p712ProjectID(t, h)
	role := p712ProjectRole(t, h, projectID, "p712r3")
	p712RequireRoleFiles(t, h, role, []string{"PROJECT_STATE", "ARCHITECTURE"})
	st := p712State(t, h, projectID, "IMPLEMENT")
	p712RequireStateFiles(t, h, st, []string{"ACTIVE_TASK", "PHASE_STATE"})

	got, err := p712Svc(t, h).ResolveStateFileRequirements(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, c := range []string{"PROJECT_STATE", "ARCHITECTURE", "ACTIVE_TASK", "PHASE_STATE"} {
		if !slices.Contains(got, c) {
			t.Fatalf("resolved %v missing category %s", got, c)
		}
	}
	if len(got) != 4 {
		t.Fatalf("resolved %v, want exactly the four categories", got)
	}
}

// T7.17.4 — Duplicates removed: a category declared by BOTH a role and a
// state appears exactly once (the set-union, not concatenation).
func TestP712DuplicatesRemoved(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := p712ProjectID(t, h)
	role := p712ProjectRole(t, h, projectID, "p712r4")
	p712RequireRoleFiles(t, h, role, []string{"ACTIVE_TASK"})
	st := p712State(t, h, projectID, "IMPLEMENT")
	p712RequireStateFiles(t, h, st, []string{"ACTIVE_TASK", "PHASE_STATE"})

	got, err := p712Svc(t, h).ResolveStateFileRequirements(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	n := 0
	for _, g := range got {
		if g == "ACTIVE_TASK" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("ACTIVE_TASK appears %d times in %v, want exactly 1", n, got)
	}
	want := []string{"ACTIVE_TASK", "PHASE_STATE"}
	if !slices.Equal(got, want) {
		t.Fatalf("resolved = %v, want %v", got, want)
	}
}

// T7.17.5 — Ordering deterministic: repeated calls return byte-identical
// results, and the order follows the primitive's first-seen rule (role
// categories in (role key, position) order first, then state categories in
// (state key, ordinal) order, a role category taking precedence over a
// same-category state one).
func TestP712OrderingDeterministic(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := p712ProjectID(t, h)
	role := p712ProjectRole(t, h, projectID, "p712r5")
	p712RequireRoleFiles(t, h, role, []string{"A1", "B1"})
	st := p712State(t, h, projectID, "ZLAST")
	p712RequireStateFiles(t, h, st, []string{"B1", "C1"})

	svc := p712Svc(t, h)
	first, err := svc.ResolveStateFileRequirements(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"A1", "B1", "C1"}
	if !slices.Equal(first, want) {
		t.Fatalf("resolved = %v, want %v (role-first, first-seen, deduped)", first, want)
	}
	for i := 0; i < 3; i++ {
		again, err := svc.ResolveStateFileRequirements(ctx, projectID.String())
		if err != nil {
			t.Fatalf("resolve (repeat %d): %v", i+1, err)
		}
		if !slices.Equal(again, first) {
			t.Fatalf("repeat %d = %v, want deterministic %v", i+1, again, first)
		}
	}
}

// T7.17.6 — Disabled requirement handled: per the stated semantics, a
// DISABLED SOURCE CONTRIBUTES NOTHING — a disabled role's and a disabled
// state's requirements are excluded from the resolved set (the rows stay
// persisted), and an enabled source's requirements remain.
func TestP712DisabledSourceContributesNothing(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	raw := h.App.DB().Raw()
	roles := dbrepo.NewRoleRepository(raw)
	projectID := p712ProjectID(t, h)

	roleOff := p712ProjectRole(t, h, projectID, "p712off")
	p712RequireRoleFiles(t, h, roleOff, []string{"DISABLED_ROLE_FILE"})
	if _, err := roles.SetEnabled(ctx, roleOff, false); err != nil {
		t.Fatalf("disable role: %v", err)
	}
	roleOn := p712ProjectRole(t, h, projectID, "p712on")
	p712RequireRoleFiles(t, h, roleOn, []string{"ENABLED_ROLE_FILE"})

	stOff := p712State(t, h, projectID, "OFFSTATE")
	p712RequireStateFiles(t, h, stOff, []string{"DISABLED_STATE_FILE"})
	if _, err := h.DBExec(ctx, "UPDATE workflow_states SET enabled = false WHERE id = '"+stOff.String()+"'"); err != nil {
		t.Fatalf("disable state: %v", err)
	}
	stOn := p712State(t, h, projectID, "ONSTATE")
	p712RequireStateFiles(t, h, stOn, []string{"ENABLED_STATE_FILE"})

	// The disabled sources' rows are still persisted (exclusion is at
	// resolution time, not a deletion).
	var offRoleRows, offStateRows int
	if err := h.DBScan(ctx, "SELECT count(*) FROM role_state_files WHERE role_id = '"+roleOff.String()+"'", &offRoleRows); err != nil {
		t.Fatalf("count role rows: %v", err)
	}
	if offRoleRows != 1 {
		t.Fatalf("disabled role's requirement rows = %d, want 1 (rows persist)", offRoleRows)
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_file_requirements WHERE state_id = '"+stOff.String()+"'", &offStateRows); err != nil {
		t.Fatalf("count state rows: %v", err)
	}
	if offStateRows != 1 {
		t.Fatalf("disabled state's requirement rows = %d, want 1 (rows persist)", offStateRows)
	}

	got, err := p712Svc(t, h).ResolveStateFileRequirements(ctx, projectID.String())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"ENABLED_ROLE_FILE", "ENABLED_STATE_FILE"}
	if !slices.Equal(got, want) {
		t.Fatalf("resolved = %v, want %v (disabled sources excluded)", got, want)
	}
}
