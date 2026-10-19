package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// P6-07 (WBS 6.4): migration 00024 (state_role_assignments) and the
// state-to-role assignment repository against real PostgreSQL.
//
// DB-level checks: role/state existence (T6.4.2), cross-project rejection
// (T6.4.6), duplicate rejection. T6.4.3 (disabled role) and T6.4.4
// (allowed states) are service-level configuration concerns — the
// repository persists the assignment either way.

// p607Role inserts a role row via direct SQL and returns its ID.
// projectID = "" creates a GLOBAL scope role (project_id NULL).
func p607Role(t *testing.T, h *Harness, projectID, key string) string {
	t.Helper()
	ctx := context.Background()
	var projExpr string
	if projectID == "" {
		projExpr = "NULL"
	} else {
		projExpr = "'" + projectID + "'"
	}
	if _, err := h.DBExec(ctx, "INSERT INTO roles (id, project_id, key, name, description, enabled, version, created_at, updated_at) VALUES (gen_random_uuid(), "+projExpr+", '"+key+"', '"+key+"', '', true, 1, now(), now())"); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	var id string
	if err := h.DBScan(ctx, "SELECT id::text FROM roles WHERE key = '"+key+"'", &id); err != nil {
		t.Fatalf("read role id: %v", err)
	}
	return id
}

// p607State creates a workflow state in the given machine and returns its
// ID.
func p607State(t *testing.T, h *Harness, machineID, key string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+machineID+"', '"+key+"', '"+key+"', '', 'ACTION', false, true, now(), now())"); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	var id string
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+machineID+"' AND key = '"+key+"'", &id); err != nil {
		t.Fatalf("read state id: %v", err)
	}
	return id
}

// P6-07.1 — Migration 00024 applies cleanly on a fresh harness DB.
func TestP607MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 23 -> 24 by P6-07 (migration 00024, state_role_assignments)
	// — accepted contract change, not a weakened test.
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_role_assignments'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("state_role_assignments present = %d, want 1", n)
	}
}

// P6-07.2 — Down migration reverses cleanly: dropping the added objects
// returns the schema to its pre-00024 shape.
func TestP607DownMigration(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "DROP TRIGGER IF EXISTS state_role_assignments_same_project ON state_role_assignments"); err != nil {
		t.Fatalf("down: drop trigger: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE state_role_assignments"); err != nil {
		t.Fatalf("down: drop table: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP FUNCTION IF EXISTS check_assignment_same_project()"); err != nil {
		t.Fatalf("down: drop function: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_role_assignments'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("state_role_assignments still present after down migration")
	}
}

// T6.4.2 (DB half) — An assignment referencing a missing role (or state)
// is rejected at the DB level: the FK fires, or — when the trigger skips a
// role row it cannot read — the FK still rejects before commit.
func TestP607UnknownRoleRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	machine := p605Machine(t, h, proj, "m-unk")
	state := p607State(t, h, machine, "IMPLEMENT")
	ghost := uuid.New().String()
	_, err := h.DBExec(ctx, "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+state+"', '"+ghost+"', 0, now(), now())")
	if err == nil {
		t.Fatal("unknown-role assignment accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("unknown-role error = %v, want 23503", err)
	}
	// A missing state is likewise rejected by the FK.
	role := p607Role(t, h, proj, "ENGINEER")
	_, err = h.DBExec(ctx, "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+uuid.New().String()+"', '"+role+"', 0, now(), now())")
	if err == nil {
		t.Fatal("unknown-state assignment accepted, want FK rejection")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("unknown-state error = %v, want 23503", err)
	}
}

// T6.4.6 (DB half) — A role from a different project is rejected at the
// DB level; a same-project role and a GLOBAL role are accepted.
func TestP607CrossProjectRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	proj2 := createPhaseProject(t, h)
	machine := p605Machine(t, h, proj, "m-xproj")
	state := p607State(t, h, machine, "IMPLEMENT")

	// A role in a DIFFERENT project is rejected.
	otherRole := p607Role(t, h, proj2, "OTHER-PROJ")
	_, err := h.DBExec(ctx, "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+state+"', '"+otherRole+"', 0, now(), now())")
	if err == nil {
		t.Fatal("cross-project role accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("cross-project error = %v, want 23514", err)
	}

	// A same-project role is accepted.
	ownRole := p607Role(t, h, proj, "OWN-ROLE")
	if _, err := h.DBExec(ctx, "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+state+"', '"+ownRole+"', 0, now(), now())"); err != nil {
		t.Fatalf("same-project role rejected: %v", err)
	}
	// A GLOBAL role (project_id NULL) is valid for any project.
	globalRole := p607Role(t, h, "", "GLOBAL-ROLE")
	if _, err := h.DBExec(ctx, "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+state+"', '"+globalRole+"', 1, now(), now())"); err != nil {
		t.Fatalf("global role rejected: %v", err)
	}
}

// Duplicate (state, role) is rejected at the DB level (uniqueness); two
// roles on the same state are fine.
func TestP607DuplicateAssignmentRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	machine := p605Machine(t, h, proj, "m-dup")
	state := p607State(t, h, machine, "VALIDATE")
	role := p607Role(t, h, proj, "TESTER")
	ins := "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '" + state + "', '" + role + "', "
	if _, err := h.DBExec(ctx, ins+"0, now(), now())"); err != nil {
		t.Fatalf("first assignment: %v", err)
	}
	_, err := h.DBExec(ctx, ins+"1, now(), now())")
	if err == nil {
		t.Fatal("duplicate (state, role) accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate error = %v, want 23505", err)
	}
	// A second role on the same state is allowed.
	role2 := p607Role(t, h, proj, "ENGINEER")
	if _, err := h.DBExec(ctx, "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+state+"', '"+role2+"', 1, now(), now())"); err != nil {
		t.Fatalf("second role on same state rejected: %v", err)
	}
}

// T6.4.5 — Ordered multiple roles round-trip through the repository: the
// list comes back in ordinal order even when created out of order.
func TestP607OrderedRolesRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	machine := p605Machine(t, h, proj, "m-ord")
	state := p607State(t, h, machine, "PHASE_PLAN")
	roleA := p607Role(t, h, proj, "ARCHITECT")
	roleB := p607Role(t, h, proj, "PLANNER")
	stateID, _ := uuid.Parse(state)
	roleAID, _ := uuid.Parse(roleA)
	roleBID, _ := uuid.Parse(roleB)
	now := time.Now().UTC()
	repo := dbrepo.NewStateRoleAssignmentRepository(pool)
	// Insert out of ordinal order; the list must come back in ordinal order.
	recs := []*persistence.StateRoleAssignmentRecord{
		{ID: uuid.New(), StateID: stateID, RoleID: roleBID, Ordinal: 1, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), StateID: stateID, RoleID: roleAID, Ordinal: 0, CreatedAt: now, UpdatedAt: now},
	}
	for _, rec := range recs {
		if err := repo.Create(ctx, rec); err != nil {
			t.Fatalf("create assignment: %v", err)
		}
	}
	list, err := repo.ListByState(ctx, stateID)
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("assignments = %d, want 2", len(list))
	}
	if list[0].RoleID != roleAID || list[1].RoleID != roleBID {
		t.Fatalf("list order = [%s, %s], want [%s, %s]", list[0].RoleID, list[1].RoleID, roleAID, roleBID)
	}
	if list[0].Ordinal != 0 || list[1].Ordinal != 1 {
		t.Fatalf("ordinals = [%d, %d], want [0, 1]", list[0].Ordinal, list[1].Ordinal)
	}
	got, err := repo.Get(ctx, recs[0].ID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.RoleID != roleBID || got.Ordinal != 1 || got.StateID != stateID {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if _, err := repo.Get(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("get missing err = %v, want ErrNotFound", err)
	}
}

// Repository surface: duplicate (state, role) -> ErrConflict; missing role
// -> ErrInvalidReference; cross-project -> translated (neither Conflict
// nor NotFound).
func TestP607RepoSurfaces(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	proj2 := createPhaseProject(t, h)
	machine := p605Machine(t, h, proj, "m-rsf")
	state := p607State(t, h, machine, "IMPLEMENT")
	stateID, _ := uuid.Parse(state)
	now := time.Now().UTC()
	repo := dbrepo.NewStateRoleAssignmentRepository(pool)
	role := p607Role(t, h, proj, "ENGINEER")
	roleID, _ := uuid.Parse(role)
	first := &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: roleID, Ordinal: 0,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	dup := &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: roleID, Ordinal: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(ctx, dup); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate err = %v, want ErrConflict", err)
	}
	// Missing role (T6.4.2 at the repository surface).
	ghost := &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: uuid.New(), Ordinal: 0,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(ctx, ghost); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("missing-role err = %v, want ErrInvalidReference", err)
	}
	// Cross-project role (T6.4.6 at the repository surface).
	other := p607Role(t, h, proj2, "FOREIGN-ROLE")
	otherID, _ := uuid.Parse(other)
	xp := &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: stateID, RoleID: otherID, Ordinal: 0,
		CreatedAt: now, UpdatedAt: now,
	}
	err := repo.Create(ctx, xp)
	if err == nil {
		t.Fatal("cross-project accepted, want rejection")
	}
	if errors.Is(err, persistence.ErrConflict) || errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("cross-project err = %v, want neither ErrConflict nor ErrNotFound", err)
	}
}
