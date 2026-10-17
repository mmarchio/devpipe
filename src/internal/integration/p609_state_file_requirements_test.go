package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

// P6-09 (WBS 6.6): migration 00027 (state_file_requirements) and the
// state-file requirement repository against real PostgreSQL. States declare
// required state-file categories that supplement the role-level requirements
// (WBS 5.12); the union-dedup resolution helper combines them.

// P6-09.1 — Migration 00027 applies cleanly on a fresh harness DB.
func TestP609MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 26 -> 27 by P6-09 (migration 00027, state_file_requirements)
	// — accepted contract change, not a weakened test.
	if version != 126 {
		t.Fatalf("max migration version = %d, want 82", version)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_file_requirements'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("state_file_requirements present = %d, want 1", n)
	}
}

// P6-09.2 — Down migration reverses cleanly: dropping the table returns the
// schema to its pre-00027 shape.
func TestP609DownMigration(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "DROP TABLE state_file_requirements"); err != nil {
		t.Fatalf("down: drop table: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_file_requirements'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("state_file_requirements still present after down migration")
	}
}

// T6.6.1 — A state file requirement is added and persists: it round-trips
// through the repository (key, state, ordinal) and a missing state is
// rejected at the repository surface (FK -> ErrInvalidReference).
func TestP609RequirementPersists(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	stateIDStr := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-p609a"), "IMPLEMENT")
	stateID, _ := uuid.Parse(stateIDStr)
	repo := dbrepo.NewStateFileRequirementRepository(pool)

	rec, err := repo.Require(ctx, stateID, "ACTIVE_TASK")
	if err != nil {
		t.Fatalf("require: %v", err)
	}
	if rec.Category != "ACTIVE_TASK" || rec.StateID != stateID || rec.Ordinal != 0 {
		t.Fatalf("require result = %+v, want category ACTIVE_TASK on the state at ordinal 0", rec)
	}
	list, err := repo.ListByState(ctx, stateID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list length = %d, want 1", len(list))
	}
	if list[0].Category != "ACTIVE_TASK" || list[0].Ordinal != 0 {
		t.Fatalf("persisted requirement = %+v", list[0])
	}
	// A requirement for a missing state is rejected by the FK.
	if _, err := repo.Require(ctx, uuid.New(), "GHOST"); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("missing-state err = %v, want ErrInvalidReference", err)
	}
}

// T6.6.2 — Multiple requirements per state are supported, in order.
func TestP609MultipleRequirements(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	stateIDStr := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-p609b"), "IMPLEMENT")
	stateID, _ := uuid.Parse(stateIDStr)
	repo := dbrepo.NewStateFileRequirementRepository(pool)

	for _, cat := range []string{"ACTIVE_TASK", "PROJECT_STATE", "DECISIONS"} {
		if _, err := repo.Require(ctx, stateID, cat); err != nil {
			t.Fatalf("require %s: %v", cat, err)
		}
	}
	list, err := repo.ListByState(ctx, stateID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("list length = %d, want 3", len(list))
	}
	for i, rec := range list {
		if rec.Ordinal != i {
			t.Fatalf("list[%d].ordinal = %d, want %d", i, rec.Ordinal, i)
		}
	}
}

// T6.6.3 — A duplicate requirement on a state is rejected: ErrConflict at
// the repository surface and 23505 at the DB level.
func TestP609DuplicateRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	stateIDStr := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-p609c"), "IMPLEMENT")
	stateID, _ := uuid.Parse(stateIDStr)
	repo := dbrepo.NewStateFileRequirementRepository(pool)

	if _, err := repo.Require(ctx, stateID, "ACTIVE_TASK"); err != nil {
		t.Fatalf("first require: %v", err)
	}
	if _, err := repo.Require(ctx, stateID, "ACTIVE_TASK"); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate err = %v, want ErrConflict", err)
	}
	// DB level: the unique constraint fires with 23505.
	_, err := h.DBExec(ctx, "INSERT INTO state_file_requirements (id, state_id, category, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+stateIDStr+"', 'ACTIVE_TASK', 1, now(), now())")
	if err == nil {
		t.Fatal("duplicate accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate error = %v, want 23505", err)
	}
}

// T6.6.4 — Ordering is deterministic: rows inserted out of ordinal order via
// direct SQL come back in ordinal order, and concurrent requires yield the
// contiguous sequence 0..N-1 exactly once each.
func TestP609DeterministicOrdering(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	stateIDStr := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-p609d"), "IMPLEMENT")
	stateID, _ := uuid.Parse(stateIDStr)
	repo := dbrepo.NewStateFileRequirementRepository(pool)

	// Direct SQL out of ordinal order.
	seedIns := func(cat string, ordinal int) {
		if _, err := h.DBExec(ctx, "INSERT INTO state_file_requirements (id, state_id, category, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+stateIDStr+"', '"+cat+"', "+itoa(ordinal)+", now(), now())"); err != nil {
			t.Fatalf("seed %s: %v", cat, err)
		}
	}
	seedIns("LATE", 1)
	seedIns("EARLY", 0)
	list, err := repo.ListByState(ctx, stateID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Category != "EARLY" || list[1].Category != "LATE" {
		t.Fatalf("list order = %+v, want EARLY then LATE by ordinal", list)
	}

	// Concurrent requires allocate distinct contiguous ordinals.
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := repo.Require(ctx, stateID, "CONC-"+itoa(i))
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent require: %v", err)
		}
	}
	all, err := repo.ListByState(ctx, stateID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != n+2 {
		t.Fatalf("total requirements = %d, want %d (no lost rows)", len(all), n+2)
	}
	seen := map[int]bool{}
	for _, rec := range all {
		if seen[rec.Ordinal] {
			t.Fatalf("duplicate ordinal %d", rec.Ordinal)
		}
		seen[rec.Ordinal] = true
	}
	for i := 0; i < n+2; i++ {
		if !seen[i] {
			t.Fatalf("ordinal %d missing, want contiguous 0..%d", i, n+1)
		}
	}
}

// T6.6.5 — Role/state requirement overlap is deduplicated during resolution:
// the helper combines the role-level requirements (WBS 5.12, role position
// order) with the state-level requirements (ordinal order) into the
// deterministic union, keeping the first occurrence of each category.
func TestP609UnionDedupResolution(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	// A unique role key: the harness pre-seeds a GLOBAL ENGINEER role.
	roleIDStr := p607Role(t, h, proj, "P609-ROLE")
	roleID, _ := uuid.Parse(roleIDStr)
	stateIDStr := p607State(t, h, p605Machine(t, h, proj, "m-p609e"), "IMPLEMENT")
	stateID, _ := uuid.Parse(stateIDStr)

	roleRepo := dbrepo.NewRoleStateFileRepository(pool)
	for _, key := range []string{"PROJECT_STATE", "ARCHITECTURE"} {
		if _, err := roleRepo.Require(ctx, roleID, key); err != nil {
			t.Fatalf("role require %s: %v", key, err)
		}
	}
	stateRepo := dbrepo.NewStateFileRequirementRepository(pool)
	// OVERLAP with the role (ARCHITECTURE) plus a state-only category.
	for _, key := range []string{"ARCHITECTURE", "ACTIVE_TASK"} {
		if _, err := stateRepo.Require(ctx, stateID, key); err != nil {
			t.Fatalf("state require %s: %v", key, err)
		}
	}

	roleList, err := roleRepo.ListByRole(ctx, roleID)
	if err != nil {
		t.Fatalf("list role: %v", err)
	}
	stateList, err := stateRepo.ListByState(ctx, stateID)
	if err != nil {
		t.Fatalf("list state: %v", err)
	}
	var roleKeys, stateKeys []string
	for _, rec := range roleList {
		roleKeys = append(roleKeys, rec.StateFileKey)
	}
	for _, rec := range stateList {
		stateKeys = append(stateKeys, rec.Category)
	}
	got := state.ResolveStateFileRequirements(roleKeys, stateKeys)
	want := []string{"PROJECT_STATE", "ARCHITECTURE", "ACTIVE_TASK"}
	if len(got) != len(want) {
		t.Fatalf("resolution = %v, want %v (overlap deduplicated)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolution = %v, want %v (deterministic order)", got, want)
		}
	}
}
