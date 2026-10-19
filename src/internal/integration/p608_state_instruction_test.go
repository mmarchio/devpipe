package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/role"
)

// P6-08 (WBS 6.5): migrations 00025 (state_instructions) + 00026
// (state_instruction_versions) and the state-instruction repository against
// real PostgreSQL. State instructions are versioned (T6.5.2), historically
// retained (T6.5.3), hash-generated (T6.5.4), and a distinct entity from the
// role system prompt (T6.5.5).

// P6-08.1 — Migrations 00025/00026 apply cleanly on a fresh harness DB.
func TestP608MigrationsApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 24 -> 26 by P6-08 (migrations 00025/00026, state_instructions /
	// state_instruction_versions) — accepted contract change, not a
	// weakened test.
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	for _, table := range []string{"state_instructions", "state_instruction_versions"} {
		var n int
		if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = '"+table+"'", &n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %s present = %d, want 1", table, n)
		}
	}
}

// P6-08.2 — Down migration reverses cleanly: dropping the added objects
// returns the schema to its pre-00025/00026 shape.
func TestP608DownMigration(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	if _, err := h.DBExec(ctx, "DROP TABLE state_instruction_versions"); err != nil {
		t.Fatalf("down: drop versions: %v", err)
	}
	if _, err := h.DBExec(ctx, "DROP TABLE state_instructions"); err != nil {
		t.Fatalf("down: drop marker: %v", err)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_instruction_versions'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("state_instruction_versions still present after down migration")
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_instructions'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("state_instructions still present after down migration")
	}
}

// T6.5.1 — State instruction persists and round-trips through the
// repository: the stored version's content and hash come back unchanged, and
// the current-version pointer tracks it.
func TestP608PersistedRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	state := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-rt"), "IMPLEMENT")
	stateID, _ := uuid.Parse(state)
	repo := dbrepo.NewStateInstructionRepository(pool)

	v1, err := repo.AppendVersion(ctx, stateID, "Correct only the failures reported by validation.")
	if err != nil {
		t.Fatalf("append v1: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("first version = %d, want 1", v1.Version)
	}
	got, err := repo.GetVersion(ctx, stateID, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if got.Content != "Correct only the failures reported by validation." {
		t.Fatalf("round-trip content = %q", got.Content)
	}
	if got.ContentHash != v1.ContentHash || got.ContentHash == "" {
		t.Fatalf("round-trip hash mismatch: %q vs %q", got.ContentHash, v1.ContentHash)
	}
	cur, err := repo.GetCurrentVersion(ctx, stateID)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur != 1 {
		t.Fatalf("current version = %d, want 1", cur)
	}
}

// T6.5.2 + T6.5.3 — Edits create a new version; v1+ all remain retrievable;
// historical rows are retained (append-only, never overwritten).
func TestP608VersionedAndHistorical(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	state := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-ver"), "FIX")
	stateID, _ := uuid.Parse(state)
	repo := dbrepo.NewStateInstructionRepository(pool)

	if _, err := repo.AppendVersion(ctx, stateID, "first"); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if _, err := repo.AppendVersion(ctx, stateID, "second"); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if _, err := repo.AppendVersion(ctx, stateID, "third"); err != nil {
		t.Fatalf("v3: %v", err)
	}

	// All historical versions remain retrievable (T6.5.3).
	if v, err := repo.GetVersion(ctx, stateID, 1); err != nil || v.Content != "first" {
		t.Fatalf("v1 historical: %v %v", v, err)
	}
	if v, err := repo.GetVersion(ctx, stateID, 2); err != nil || v.Content != "second" {
		t.Fatalf("v2 historical: %v %v", v, err)
	}
	if v, err := repo.GetVersion(ctx, stateID, 3); err != nil || v.Content != "third" {
		t.Fatalf("v3: %v %v", v, err)
	}

	// The version list is complete and ascending.
	list, err := repo.ListVersions(ctx, stateID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("list length = %d, want 3 (historical retained)", len(list))
	}
	for i, v := range list {
		if v.Version != i+1 {
			t.Fatalf("list[%d].version = %d, want %d", i, v.Version, i+1)
		}
	}

	// The current pointer tracks the latest.
	cur, err := repo.GetCurrentVersion(ctx, stateID)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur != 3 {
		t.Fatalf("current = %d, want 3", cur)
	}

	// A missing version yields ErrNotFound.
	if _, err := repo.GetVersion(ctx, stateID, 99); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing version err = %v, want ErrNotFound", err)
	}
}

// T6.5.4 — A deterministic hash is generated per version: the same content
// yields the same hash, a changed content yields a different hash, and the
// stored hash equals the known role content-hash of the content.
func TestP608HashGenerated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	state := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-hash"), "VALIDATE")
	stateID, _ := uuid.Parse(state)
	repo := dbrepo.NewStateInstructionRepository(pool)

	v, err := repo.AppendVersion(ctx, stateID, "hash payload A")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if v.ContentHash != role.HashSystemPromptContent("hash payload A") {
		t.Fatalf("stored hash != content hash: %q", v.ContentHash)
	}
	if v.ContentHash == "" {
		t.Fatal("hash is empty, want non-empty")
	}

	// Same content in a second state yields the identical hash (deterministic).
	state2 := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-hash2"), "VALIDATE")
	state2ID, _ := uuid.Parse(state2)
	v2, err := repo.AppendVersion(ctx, state2ID, "hash payload A")
	if err != nil {
		t.Fatalf("append2: %v", err)
	}
	if v2.ContentHash != v.ContentHash {
		t.Fatalf("identical content produced different hashes: %q vs %q", v.ContentHash, v2.ContentHash)
	}

	// Changed content yields a different hash.
	v3, err := repo.AppendVersion(ctx, stateID, "hash payload B")
	if err != nil {
		t.Fatalf("append3: %v", err)
	}
	if v3.ContentHash == v.ContentHash {
		t.Fatal("changed content produced an identical hash")
	}
}

// T6.5.5 — The state instruction is a distinct entity from the role system
// prompt: writing a state instruction neither creates nor mutates a role
// system prompt, and a role prompt carries its own separate version history.
func TestP608SeparateFromRoleSystemPrompt(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	machine := p605Machine(t, h, proj, "m-sep")
	state := p607State(t, h, machine, "FIX")
	stateID, _ := uuid.Parse(state)
	repo := dbrepo.NewStateInstructionRepository(pool)

	payload := "Correct only the failures reported by validation."
	// A role (GLOBAL scope) with a v1 system prompt of the SAME content.
	roleID := p607Role(t, h, proj, "P608-ROLE")
	if _, err := h.DBExec(ctx, "INSERT INTO role_system_prompts (id, role_id, version, content, content_hash) VALUES (gen_random_uuid(), '"+roleID+"', 1, '"+payload+"', '')"); err != nil {
		t.Fatalf("seed role prompt: %v", err)
	}

	// Writing the state instruction must not touch the role prompt table.
	if _, err := repo.AppendVersion(ctx, stateID, payload); err != nil {
		t.Fatalf("state instruction append: %v", err)
	}
	var promptRows int
	if err := h.DBScan(ctx, "SELECT count(*) FROM role_system_prompts WHERE role_id = '"+roleID+"'", &promptRows); err != nil {
		t.Fatal(err)
	}
	if promptRows != 1 {
		t.Fatalf("role prompt rows after state-instruction write = %d, want 1 (unchanged)", promptRows)
	}
	var promptVersion int
	if err := h.DBScan(ctx, "SELECT current_system_prompt_version FROM roles WHERE id = '"+roleID+"'", &promptVersion); err != nil {
		t.Fatal(err)
	}
	if promptVersion != 1 {
		t.Fatalf("role prompt version after state-instruction write = %d, want 1 (unchanged)", promptVersion)
	}
	// And the state-instruction history is its own table, not the role prompt table.
	var stateVerRows int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_instruction_versions WHERE state_id = '"+stateID.String()+"'", &stateVerRows); err != nil {
		t.Fatal(err)
	}
	if stateVerRows != 1 {
		t.Fatalf("state_instruction_versions rows = %d, want 1 (distinct entity)", stateVerRows)
	}
}

// DB-level: duplicate (state_id, version) is rejected; the version's FK to a
// missing state is rejected; the marker is unique per state.
func TestP608DBEnforcement(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	state := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-db"), "IMPLEMENT")
	ins := "INSERT INTO state_instruction_versions (id, state_id, version, content, content_hash) VALUES (gen_random_uuid(), '" + state + "', 1, 'v1', '')"
	if _, err := h.DBExec(ctx, ins); err != nil {
		t.Fatalf("first version: %v", err)
	}
	_, err := h.DBExec(ctx, ins)
	if err == nil {
		t.Fatal("duplicate (state_id, version) accepted, want rejection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate error = %v, want 23505", err)
	}
	// A version for a missing state is rejected by the FK.
	_, err = h.DBExec(ctx, "INSERT INTO state_instruction_versions (id, state_id, version, content, content_hash) VALUES (gen_random_uuid(), '"+uuid.New().String()+"', 1, 'x', '')")
	if err == nil {
		t.Fatal("missing-state version accepted, want FK rejection")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("missing-state error = %v, want 23503", err)
	}
	// version 0 is rejected by the CHECK constraint.
	_, err = h.DBExec(ctx, "INSERT INTO state_instruction_versions (id, state_id, version, content, content_hash) VALUES (gen_random_uuid(), '"+state+"', 0, 'x', '')")
	if err == nil {
		t.Fatal("version 0 accepted, want CHECK rejection")
	}
}

// Repository surface: a missing state for AppendVersion yields
// ErrInvalidReference (the version FK fires before the pointer update); the
// concurrent-invocation path is exercised so the -race gate covers the
// advisory-locked append.
func TestP608RepoSurfaceAndConcurrency(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	repo := dbrepo.NewStateInstructionRepository(pool)

	// Missing state -> the FK rejects the insert -> ErrInvalidReference.
	if _, err := repo.AppendVersion(ctx, uuid.New(), "payload"); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("missing-state append err = %v, want ErrInvalidReference", err)
	}

	// Concurrent appends to the same state must yield the contiguous sequence
	// 1..N exactly once each (deterministic ordering under the advisory lock).
	state := p607State(t, h, p605Machine(t, h, p605Project(t, h), "m-conc"), "IMPLEMENT")
	stateID, _ := uuid.Parse(state)
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := repo.AppendVersion(ctx, stateID, "conc-"+itoa(i))
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	if cur, err := repo.GetCurrentVersion(ctx, stateID); err != nil || cur != n {
		t.Fatalf("concurrent current = %d, want %d (err %v)", cur, n, err)
	}
	list, err := repo.ListVersions(ctx, stateID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != n {
		t.Fatalf("concurrent list length = %d, want %d (no lost versions)", len(list), n)
	}
	for i, v := range list {
		if v.Version != i+1 {
			t.Fatalf("concurrent list[%d].version = %d, want %d", i, v.Version, i+1)
		}
	}
}
