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

// P6-05 (WBS 6.1 + 6.2 DB half): migrations 00021/00022 and the
// state-machine / workflow-state repositories against real PostgreSQL.

func p605Project(t *testing.T, h *Harness) string {
	t.Helper()
	return createPhaseProject(t, h)
}

// p605Machine creates a machine row via direct SQL and returns its ID.
func p605Machine(t *testing.T, h *Harness, projectID, name string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "INSERT INTO state_machines (id, project_id, family_id, name, description, version, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+projectID+"', gen_random_uuid(), '"+name+"', '', 1, true, now(), now())"); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	var id string
	if err := h.DBScan(ctx, "SELECT id::text FROM state_machines WHERE project_id = '"+projectID+"' AND name = '"+name+"'", &id); err != nil {
		t.Fatalf("read machine id: %v", err)
	}
	return id
}

// P6-05.1 — Migrations 00021/00022 apply cleanly on a fresh harness DB.
func TestP605MigrationsApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 20 -> 22 by P6-05 (migrations 00021/00022) and
	// 22 -> 23 by P6-06 (migration 00023) — accepted
	// contract change, not a weakened test.
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	for _, table := range []string{"state_machines", "workflow_states"} {
		var n int
		if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = '"+table+"'", &n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %s present = %d, want 1", table, n)
		}
	}
}

// T6.1.6 (DB half) — A machine for an unknown project is rejected by the FK.
func TestP605UnknownProjectRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	ghost := uuid.New().String()
	_, err := h.DBExec(ctx, "INSERT INTO state_machines (id, project_id, family_id, name, description, version, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+ghost+"', gen_random_uuid(), 'orphan', '', 1, true, now(), now())")
	if err == nil {
		t.Fatal("machine with unknown project accepted, want FK rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("unknown-project error = %v, want 23503", err)
	}
}

// T6.2.5 (DB half) — An unknown state kind is rejected by the CHECK.
func TestP605UnknownKindRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	machineID := p605Machine(t, h, proj, "m-kind")
	_, err := h.DBExec(ctx, "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+machineID+"', 'BOGUS_STATE', 'Bogus', '', 'BOGUS', false, true, now(), now())")
	if err == nil {
		t.Fatal("unknown kind accepted, want CHECK rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("unknown-kind error = %v, want 23514", err)
	}
}

// T6.2.3 (DB half) — A duplicate key within one machine is rejected.
func TestP605DuplicateKeyRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	machineID := p605Machine(t, h, proj, "m-dup")
	insert := "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '" + machineID + "', 'IMPLEMENT', 'Implement', '', 'ACTION', false, true, now(), now())"
	if _, err := h.DBExec(ctx, insert); err != nil {
		t.Fatalf("first state insert: %v", err)
	}
	_, err := h.DBExec(ctx, insert)
	if err == nil {
		t.Fatal("duplicate (machine_id, key) accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate-key error = %v, want 23505", err)
	}
}

// T6.2.4 (DB half) — The same key in a different machine is allowed.
func TestP605SameKeyAcrossMachines(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	m1 := p605Machine(t, h, proj, "m-a")
	m2 := p605Machine(t, h, proj, "m-b")
	base := "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"
	if _, err := h.DBExec(ctx, base+m1+"', 'IMPLEMENT', 'Implement', '', 'ACTION', false, true, now(), now())"); err != nil {
		t.Fatalf("state in machine 1: %v", err)
	}
	if _, err := h.DBExec(ctx, base+m2+"', 'IMPLEMENT', 'Implement', '', 'ACTION', false, true, now(), now())"); err != nil {
		t.Fatalf("same key in machine 2 rejected: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM workflow_states WHERE key = 'IMPLEMENT'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("IMPLEMENT rows across machines = %d, want 2", n)
	}
}

// T6.1.5 — Enabled status is persisted: an explicit false survives the
// round trip (default-true is the domain concern; the repo must not force it).
func TestP605MachineRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	machines := dbrepo.NewStateMachineRepository(pool)
	projID := uuid.MustParse(p605Project(t, h))
	now := time.Now().UTC()
	falseVal := false
	for i, name := range []string{"m-active", "m-disabled"} {
		spec := false
		if i == 0 {
			spec = true
		}
		rec := &persistence.MachineRecord{
			ID: uuid.New(), ProjectID: projID, Name: name, Description: "d" + name,
			Version: 1, Enabled: spec, CreatedAt: now, UpdatedAt: now,
		}
		if err := machines.Create(ctx, rec); err != nil {
			t.Fatalf("create machine %s: %v", name, err)
		}
		_ = falseVal
	}
	got, err := machines.ListByProject(ctx, projID)
	if err != nil {
		t.Fatalf("list machines: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("machines = %d, want 2", len(got))
	}
	enabled := map[string]bool{}
	for _, m := range got {
		enabled[m.Name] = m.Enabled
		if m.Version != 1 || m.ProjectID != projID || m.Description != "d"+m.Name {
			t.Fatalf("machine %s round-trip mismatch: %+v", m.Name, m)
		}
	}
	if !enabled["m-active"] || enabled["m-disabled"] {
		t.Fatalf("enabled round trip = %v, want {m-active:true m-disabled:false}", enabled)
	}
}

// T6.2.1/T6.2.6 — A valid state round-trips through the repositories,
// including the terminal flag and the kind value.
func TestP605StateRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projID := uuid.MustParse(p605Project(t, h))
	now := time.Now().UTC()
	machineRec := &persistence.MachineRecord{
		ID: uuid.New(), ProjectID: projID, Name: "m-rt", Description: "",
		Version: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := dbrepo.NewStateMachineRepository(pool).Create(ctx, machineRec); err != nil {
		t.Fatalf("create machine: %v", err)
	}
	states := dbrepo.NewWorkflowStateRepository(pool)
	actionRec := &persistence.WorkflowStateRecord{
		ID: uuid.New(), MachineID: machineRec.ID, Key: "IMPLEMENT", Name: "Implement",
		Description: "do the work", Kind: "ACTION", Terminal: false, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	terminalRec := &persistence.WorkflowStateRecord{
		ID: uuid.New(), MachineID: machineRec.ID, Key: "PROJECT_DONE", Name: "Project Done",
		Description: "", Kind: "TERMINAL", Terminal: true, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	for _, rec := range []*persistence.WorkflowStateRecord{actionRec, terminalRec} {
		if err := states.Create(ctx, rec); err != nil {
			t.Fatalf("create state %s: %v", rec.Key, err)
		}
	}
	got, err := states.Get(ctx, terminalRec.ID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if got.Key != "PROJECT_DONE" || got.Kind != "TERMINAL" || !got.Terminal || got.MachineID != machineRec.ID {
		t.Fatalf("state round trip mismatch: %+v", got)
	}
	list, err := states.ListByMachine(ctx, machineRec.ID, machineRec.Version)
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("states = %d, want 2", len(list))
	}
}

// Duplicate key through the repositories maps to persistence.ErrConflict
// (T6.2.3 at the repository surface).
func TestP605DuplicateKeyConflict(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projID := uuid.MustParse(p605Project(t, h))
	now := time.Now().UTC()
	machineRec := &persistence.MachineRecord{
		ID: uuid.New(), ProjectID: projID, Name: "m-cf", Description: "",
		Version: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := dbrepo.NewStateMachineRepository(pool).Create(ctx, machineRec); err != nil {
		t.Fatalf("create machine: %v", err)
	}
	states := dbrepo.NewWorkflowStateRepository(pool)
	rec := &persistence.WorkflowStateRecord{
		ID: uuid.New(), MachineID: machineRec.ID, Key: "FIX", Name: "Fix",
		Kind: "ACTION", Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := states.Create(ctx, rec); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// The domain normalizes keys before they reach the repository (P6-02),
	// so a duplicate at the DB surface is a byte-identical key.
	dup := &persistence.WorkflowStateRecord{
		ID: uuid.New(), MachineID: machineRec.ID, Key: "FIX", Name: "Fix 2",
		Kind: "ACTION", Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	err := states.Create(ctx, dup)
	if !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate key err = %v, want ErrConflict", err)
	}
}
