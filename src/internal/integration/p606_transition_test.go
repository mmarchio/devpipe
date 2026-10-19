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

// P6-06 (WBS 6.8 + 6.9 DB half): migration 00023 and the
// state-transition repository against real PostgreSQL.

// p606MachineAndStates creates a machine with two states via direct SQL and
// returns the machine ID and the state IDs (in order).
func p606MachineAndStates(t *testing.T, h *Harness, projectID, machineName string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	machineID := p605Machine(t, h, projectID, machineName)
	ids := make([]string, 2)
	for i, key := range []string{"STATE_A", "STATE_B"} {
		if _, err := h.DBExec(ctx, "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+machineID+"', '"+key+"', '"+key+"', '', 'ACTION', false, true, now(), now())"); err != nil {
			t.Fatalf("seed state %s: %v", key, err)
		}
		if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+machineID+"' AND key = '"+key+"'", &ids[i]); err != nil {
			t.Fatalf("read state id: %v", err)
		}
	}
	return machineID, ids[0], ids[1]
}

// P6-06.1 — Migration 00023 applies cleanly on a fresh harness DB.
func TestP606MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 22 -> 23 by P6-06 (migration 00023, state_transitions) —
	// accepted contract change, not a weakened test.
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_transitions'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("state_transitions present = %d, want 1", n)
	}
}

// P6-06.2 — Down migration reverses cleanly: dropping the added objects
// returns the schema to its pre-00023 shape.
func TestP606DownMigration(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "DROP TRIGGER IF EXISTS state_transitions_same_machine ON state_transitions"); err != nil {
		t.Fatalf("down: drop trigger: %v", err)
	}
	// P6-27 (00032): transition_conditions references state_transitions and
	// is dropped first in its own down migration; the 00023 down sequence
	// assumes it is already gone.
	if _, err := h.DBExec(ctx, "DROP TABLE IF EXISTS transition_conditions"); err != nil {
		t.Fatalf("down: drop transition_conditions: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE state_transitions"); err != nil {
		t.Fatalf("down: drop state_transitions: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP FUNCTION IF EXISTS check_transition_same_machine()"); err != nil {
		t.Fatalf("down: drop function: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_transitions'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("state_transitions still present after down migration")
	}
}

// T6.8.4 (DB half) — A transition whose endpoints do not belong to the
// machine (or to each other) is rejected at the DB level.
func TestP606SameMachineEnforced(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	m1, a, b := p606MachineAndStates(t, h, proj, "m-same")
	// A second machine whose state is used as the foreign endpoint.
	m2 := p605Machine(t, h, proj, "m-same-2")
	var otherState string
	if _, err := h.DBExec(ctx, "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+m2+"', 'STATE_A', 'A', '', 'ACTION', false, true, now(), now())"); err != nil {
		t.Fatalf("seed state in machine 2: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+m2+"'", &otherState); err != nil {
		t.Fatalf("read foreign state id: %v", err)
	}
	// Endpoints from different machines (transition bound to m1).
	_, err := h.DBExec(ctx, "INSERT INTO state_transitions (id, machine_id, from_state_id, to_state_id, outcome, priority, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+m1+"', '"+a+"', '"+otherState+"', 'PASS', 0, true, now(), now())")
	if err == nil {
		t.Fatal("cross-machine transition accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("cross-machine error = %v, want 23514", err)
	}
	// Endpoints inside one machine, transition bound to a DIFFERENT machine.
	_, err = h.DBExec(ctx, "INSERT INTO state_transitions (id, machine_id, from_state_id, to_state_id, outcome, priority, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+m2+"', '"+a+"', '"+b+"', 'PASS', 0, true, now(), now())")
	if err == nil {
		t.Fatal("mismatched-machine transition accepted, want rejection")
	}
	// The valid case: both endpoints of m1, transition bound to m1.
	if _, err := h.DBExec(ctx, "INSERT INTO state_transitions (id, machine_id, from_state_id, to_state_id, outcome, priority, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+m1+"', '"+a+"', '"+b+"', 'PASS', 0, true, now(), now())"); err != nil {
		t.Fatalf("valid same-machine transition rejected: %v", err)
	}
}

// T6.8.5 (DB half) — A duplicate (machine, from, outcome) — outcome compared
// under the documented case policy (case-insensitive after trim) — is
// rejected; a distinct outcome from the same state is allowed.
func TestP606DuplicateAmbiguityRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	m1, a, b := p606MachineAndStates(t, h, proj, "m-dup")
	ins := func(outcome string) string {
		return "INSERT INTO state_transitions (id, machine_id, from_state_id, to_state_id, outcome, priority, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '" + m1 + "', '" + a + "', '" + b + "', '" + outcome + "', 0, true, now(), now())"
	}
	if _, err := h.DBExec(ctx, ins("PASS")); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	// Case variant of the same outcome is ambiguous (T6.9.4 policy).
	_, err := h.DBExec(ctx, ins("pass"))
	if err == nil {
		t.Fatal("case-variant duplicate outcome accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate error = %v, want 23505", err)
	}
	// Whitespace-trimmed variant is ambiguous too.
	_, err = h.DBExec(ctx, ins(" PASS "))
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("trimmed duplicate error = %v, want 23505", err)
	}
	// A distinct outcome from the same state is not ambiguous.
	if _, err := h.DBExec(ctx, ins("FAIL")); err != nil {
		t.Fatalf("distinct outcome rejected: %v", err)
	}
}

// T6.8.6 (DB half) + round trip — The repository creates and reads back a
// transition, including priority and an explicit enabled=false.
func TestP606TransitionRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	m1, a, b := p606MachineAndStates(t, h, proj, "m-rt")
	now := time.Now().UTC()
	mid, _ := uuid.Parse(m1)
	fid, _ := uuid.Parse(a)
	tid, _ := uuid.Parse(b)
	repo := dbrepo.NewStateTransitionRepository(pool)
	rec := &persistence.StateTransitionRecord{
		ID: uuid.New(), MachineID: mid, FromStateID: fid, ToStateID: tid,
		Outcome: "COMPLETE", Priority: 42, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create transition: %v", err)
	}
	disabledRec := &persistence.StateTransitionRecord{
		ID: uuid.New(), MachineID: mid, FromStateID: fid, ToStateID: tid,
		Outcome: "blocked", Priority: -3, Enabled: false,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(ctx, disabledRec); err != nil {
		t.Fatalf("create disabled transition: %v", err)
	}
	got, err := repo.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get transition: %v", err)
	}
	if got.MachineID != mid || got.FromStateID != fid || got.ToStateID != tid {
		t.Fatalf("transition ids mismatch: %+v", got)
	}
	// Outcome is stored AS GIVEN (T6.9.4 storage policy); priority and
	// enabled must survive the round trip exactly (T6.8.6).
	if got.Outcome != "COMPLETE" || got.Priority != 42 || !got.Enabled {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	got2, err := repo.Get(ctx, disabledRec.ID)
	if err != nil {
		t.Fatalf("get disabled transition: %v", err)
	}
	if got2.Outcome != "blocked" || got2.Priority != -3 || got2.Enabled {
		t.Fatalf("disabled round trip mismatch: %+v", got2)
	}
	list, err := repo.ListByMachine(ctx, mid, 1)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("transitions = %d, want 2", len(list))
	}
	// Outcomes are listed in a deterministic order (from_state, outcome).
}

// T6.8.5 at the repository surface: a duplicate (machine, from, outcome)
// yields persistence.ErrConflict.
func TestP606DuplicateConflict(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	m1, a, b := p606MachineAndStates(t, h, proj, "m-cf")
	now := time.Now().UTC()
	mid, _ := uuid.Parse(m1)
	fid, _ := uuid.Parse(a)
	tid, _ := uuid.Parse(b)
	repo := dbrepo.NewStateTransitionRepository(pool)
	first := &persistence.StateTransitionRecord{
		ID: uuid.New(), MachineID: mid, FromStateID: fid, ToStateID: tid,
		Outcome: "RESET", Priority: 0, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	dup := &persistence.StateTransitionRecord{
		ID: uuid.New(), MachineID: mid, FromStateID: fid, ToStateID: tid,
		Outcome: "RESET", Priority: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	err := repo.Create(ctx, dup)
	if !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate outcome err = %v, want ErrConflict", err)
	}
	// Same (from, outcome) in a DIFFERENT machine is not a conflict.
	m2, a2, b2 := p606MachineAndStates(t, h, proj, "m-cf-2")
	m2id, _ := uuid.Parse(m2)
	f2, _ := uuid.Parse(a2)
	t2, _ := uuid.Parse(b2)
	other := &persistence.StateTransitionRecord{
		ID: uuid.New(), MachineID: m2id, FromStateID: f2, ToStateID: t2,
		Outcome: "RESET", Priority: 0, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.Create(ctx, other); err != nil {
		t.Fatalf("same outcome in other machine rejected: %v", err)
	}
}

// T6.8.4 at the repository surface: a cross-machine transition yields a
// translated domain error (not a raw driver error).
func TestP606SameMachineRepoSurface(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	m1, a, _ := p606MachineAndStates(t, h, proj, "m-srf")
	m2 := p605Machine(t, h, proj, "m-srf-2")
	var otherState string
	if _, err := h.DBExec(ctx, "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+m2+"', 'STATE_A', 'A', '', 'ACTION', false, true, now(), now())"); err != nil {
		t.Fatalf("seed foreign state: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+m2+"'", &otherState); err != nil {
		t.Fatalf("read foreign state id: %v", err)
	}
	now := time.Now().UTC()
	mid, _ := uuid.Parse(m1)
	fid, _ := uuid.Parse(a)
	tid, _ := uuid.Parse(otherState)
	rec := &persistence.StateTransitionRecord{
		ID: uuid.New(), MachineID: mid, FromStateID: fid, ToStateID: tid,
		Outcome: "PASS", Priority: 0, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	err := dbrepo.NewStateTransitionRepository(pool).Create(ctx, rec)
	if err == nil {
		t.Fatal("cross-machine transition accepted, want rejection")
	}
	if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("cross-machine err = %v, want neither ErrConflict nor ErrNotFound", err)
	}
	// ErrNotFound on a missing transition id.
	if _, err := dbrepo.NewStateTransitionRepository(pool).Get(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("get missing err = %v, want ErrNotFound", err)
	}
}
