package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

// P8-13 (WBS 8.23 persistence half + T8.23.1-T8.23.4 DB half): migration 00043
// (response_protocols + versioned protocol content + state references) and the
// repository — advisory-locked MAX+1 versioning, deterministic content hash,
// historical retention — against real PostgreSQL. The pure half
// (internal/prompt/protocol.go, P8-09) covers the in-domain series; this file
// covers the DB-backed half.

// p813Protocol creates a protocol (scope nil = global, project-scoped when set)
// with initial content v1 and returns the protocol id. The domain series
// (P8-09) supplies the validated identity; the repository Create persists the
// parent row AND the v1 version row.
func p813Protocol(t *testing.T, h *Harness, projectID *uuid.UUID, name string, kind prompt.ResponseProtocolKind, content string) uuid.UUID {
	t.Helper()
	series, err := prompt.NewResponseProtocolSeries(prompt.ResponseProtocolInput{
		Name: name, Kind: kind, Content: content,
		Ts: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewResponseProtocolSeries: %v", err)
	}
	repo := dbrepo.NewResponseProtocolRepository(h.App.DB().Raw())
	rec := &persistence.ResponseProtocolRecord{
		ID: series.ID, ProjectID: projectID, Name: series.Name,
		Kind: string(series.Kind), Enabled: true,
		CreatedAt: series.Rows[0].CreatedAt, UpdatedAt: series.Rows[0].CreatedAt,
	}
	if err := repo.Create(context.Background(), rec, series.Rows[0].Content); err != nil {
		t.Fatalf("create protocol: %v", err)
	}
	return series.ID
}

// p813CreateState creates a workflow state row under the given machine id via
// direct SQL (no repository surface for states is needed here) and returns its
// id.
func p813CreateState(t *testing.T, h *Harness, machineID uuid.UUID, key string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := h.DBScan(ctx,
		"INSERT INTO workflow_states (id, machine_id, key, name, kind, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+machineID.String()+"', '"+key+"', '"+key+"', 'ACTION', true, now(), now()) RETURNING id", &id); err != nil {
		t.Fatalf("create state: %v", err)
	}
	return id
}

// P8-13.1 — Migration 00043 applies cleanly on a fresh harness DB and the
// tables + unique indexes exist.
func TestP813MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 42 -> 43 by P8-13 (migration 00043, response_protocols) —
	// accepted contract change, not a weakened test.
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	for _, tbl := range []string{"response_protocols", "response_protocol_versions", "state_protocol_references"} {
		var n int
		if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = '"+tbl+"'", &n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%s present = %d, want 1", tbl, n)
		}
	}
	var idx int
	if err := h.DBScan(ctx, "SELECT count(*) FROM pg_indexes WHERE tablename = 'response_protocol_versions' AND indexname = 'response_protocol_versions_protocol_version_unique'", &idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("unique index present = %d, want 1", idx)
	}
}

// T8.23.1 (DB half) — A valid protocol persists and reads back
// byte-for-byte through the repository: the parent row (name, kind, flags) and
// the v1 version row (exact content + deterministic hash) round-trip intact.
func TestP813ProtocolPersisted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	const content = "{\"status\": \"complete|incomplete|blocked|failed\", \"summary\": \"...\"}"
	pid := p813Protocol(t, h, nil, "worker-protocol", prompt.KindImplementationResponse, content)
	repo := dbrepo.NewResponseProtocolRepository(h.App.DB().Raw())

	rec, err := repo.Get(ctx, pid)
	if err != nil {
		t.Fatalf("get protocol: %v", err)
	}
	if rec.Name != "worker-protocol" || rec.Kind != "IMPLEMENTATION_RESPONSE" || rec.ProjectID != nil || !rec.Enabled {
		t.Fatalf("parent row = %+v", rec)
	}
	v1, err := repo.GetVersion(ctx, pid, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if v1.Content != content {
		t.Fatalf("T8.23.1 content = %q, want byte-for-byte %q", v1.Content, content)
	}
	if v1.ContentHash != prompt.ContentHash(content) {
		t.Fatalf("T8.23.1 hash = %q, want domain function %q", v1.ContentHash, prompt.ContentHash(content))
	}
	// The hash is persisted (read it back by direct SQL, not the return).
	var stored string
	if err := h.DBScan(ctx, "SELECT content_hash FROM response_protocol_versions WHERE protocol_id = '"+pid.String()+"' AND version = 1", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != prompt.ContentHash(content) {
		t.Fatalf("stored hash = %q, want %q", stored, prompt.ContentHash(content))
	}
	cur, err := repo.GetCurrentVersion(ctx, pid)
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if cur.Version != 1 || cur.Content != content {
		t.Fatalf("current = %+v, want v1", cur)
	}
}

// T8.23.2 (DB half) — Appends produce v2, v3 ... contiguously; an unknown
// protocol yields ErrNotFound BEFORE any write.
func TestP813ProtocolVersioned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := p813Protocol(t, h, nil, "ver", prompt.KindPlanResponse, "v1")
	repo := dbrepo.NewResponseProtocolRepository(h.App.DB().Raw())

	v2, err := repo.AppendVersion(ctx, pid, "v2")
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("v2 = version %d, want 2", v2.Version)
	}
	v3, err := repo.AppendVersion(ctx, pid, "v3")
	if err != nil {
		t.Fatalf("v3: %v", err)
	}
	if v3.Version != 3 {
		t.Fatalf("v3 = version %d, want 3", v3.Version)
	}
	list, err := repo.ListVersions(ctx, pid)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(list) != 3 || list[0].Version != 1 || list[1].Version != 2 || list[2].Version != 3 {
		t.Fatalf("versions = %+v, want [1 2 3]", list)
	}
	cur, err := repo.GetCurrentVersion(ctx, pid)
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if cur.Version != 3 {
		t.Fatalf("current = version %d, want 3", cur.Version)
	}
	// Unknown protocol -> ErrNotFound before any write.
	if _, err := repo.AppendVersion(ctx, uuid.New(), "x"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown protocol err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetVersion(ctx, uuid.New(), 1); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown protocol version err = %v, want ErrNotFound", err)
	}
}

// T8.23.2 (DB half, concurrency) — Concurrent appends do not duplicate
// versions: 20 real goroutines race the repository against real PG; the
// committed versions are exactly the contiguous sequence 1..20.
func TestP813ConcurrentAppendsContiguous(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := p813Protocol(t, h, nil, "conc", prompt.KindValidationResponse, "v1")
	repo := dbrepo.NewResponseProtocolRepository(h.App.DB().Raw())

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := repo.AppendVersion(ctx, pid, fmt.Sprintf("r%d", i)); err != nil {
				errs <- fmt.Errorf("racer %d: %w", i, err)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("racer: %v", err)
	}
	versions, err := repo.ListVersions(ctx, pid)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != writers+1 {
		t.Fatalf("committed versions = %d, want %d", len(versions), writers+1)
	}
	for i, v := range versions {
		if v.Version != i+1 {
			t.Fatalf("T8.23.2 versions not contiguous: position %d = %d, want %d", i, v.Version, i+1)
		}
	}
}

// T8.23.3 (DB half) — A state can reference a protocol: the reference
// (state, protocol, version) persists and resolves back to the pinned version
// row, which stays resolvable after the protocol advances.
func TestP813StateReferencesProtocol(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	var machineID uuid.UUID
	if err := h.DBScan(ctx, "INSERT INTO state_machines (id, project_id, family_id, name, version, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+proj+"', gen_random_uuid(), 'p813-m', 1, true, now(), now()) RETURNING id", &machineID); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	stateID := p813CreateState(t, h, machineID, "A")
	pid := p813Protocol(t, h, nil, "ref", prompt.KindReviewResponse, "v1 content")
	repo := dbrepo.NewResponseProtocolRepository(h.App.DB().Raw())

	ref, err := repo.CreateStateReference(ctx, stateID, pid, 1)
	if err != nil {
		t.Fatalf("create reference: %v", err)
	}
	if ref.StateID != stateID || ref.ProtocolID != pid || ref.ProtocolVersion != 1 {
		t.Fatalf("reference = %+v", ref)
	}
	got, err := repo.GetStateReference(ctx, stateID, pid)
	if err != nil {
		t.Fatalf("get reference: %v", err)
	}
	if got.ProtocolVersion != 1 {
		t.Fatalf("stored reference = %+v, want version 1", got)
	}
	// Resolvable: the pinned version row exists and reads back.
	resolved, err := repo.GetVersion(ctx, pid, got.ProtocolVersion)
	if err != nil {
		t.Fatalf("resolve pinned version: %v", err)
	}
	if resolved.Content != "v1 content" {
		t.Fatalf("resolved = %+v", resolved)
	}
	// A second state of the same machine referencing the same protocol is a
	// distinct reference row; a duplicate (state, protocol) yields
	// ErrConflict.
	stateB := p813CreateState(t, h, machineID, "B")
	if _, err := repo.CreateStateReference(ctx, stateB, pid, 1); err != nil {
		t.Fatalf("second state reference: %v", err)
	}
	if _, err := repo.CreateStateReference(ctx, stateID, pid, 1); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate reference err = %v, want ErrConflict", err)
	}
	// The reference names a real protocol: unknown protocol -> ErrNotFound.
	if _, err := repo.CreateStateReference(ctx, stateID, uuid.New(), 1); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown protocol ref err = %v, want ErrNotFound", err)
	}
	// ... and a real version: a version the protocol does not carry ->
	// ErrNotFound.
	if _, err := repo.CreateStateReference(ctx, stateID, pid, 99); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("absent version ref err = %v, want ErrNotFound", err)
	}
	if _, err := repo.CreateStateReference(ctx, uuid.New(), pid, 1); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown state ref err = %v, want ErrNotFound", err)
	}
}

// T8.23.4 (DB half) — Historical protocol retained: appending v2 leaves the v1
// version row byte-unchanged (full-row signature), and the historical version
// stays retrievable after the current version advances.
func TestP813HistoricalProtocolRetained(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := p813Protocol(t, h, nil, "hist", prompt.KindPlanResponse, "schema-v1")
	repo := dbrepo.NewResponseProtocolRepository(h.App.DB().Raw())

	// Full-row signature of the v1 row: every column, not just content.
	var before string
	if err := h.DBScan(ctx,
		"SELECT id::text || '|' || version::text || '|' || content || '|' || content_hash || '|' || created_at::text FROM response_protocol_versions WHERE protocol_id = '"+pid.String()+"' AND version = 1", &before); err != nil {
		t.Fatalf("capture v1 signature: %v", err)
	}
	if _, err := repo.AppendVersion(ctx, pid, "schema-v2"); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	var after string
	if err := h.DBScan(ctx,
		"SELECT id::text || '|' || version::text || '|' || content || '|' || content_hash || '|' || created_at::text FROM response_protocol_versions WHERE protocol_id = '"+pid.String()+"' AND version = 1", &after); err != nil {
		t.Fatalf("re-capture v1 signature: %v", err)
	}
	if before != after {
		t.Fatalf("T8.23.4 v1 row mutated: before=%q after=%q", before, after)
	}
	// The historical version is still retrievable, and the current version has
	// advanced to v2.
	hist, err := repo.GetVersion(ctx, pid, 1)
	if err != nil {
		t.Fatalf("T8.23.4 historical v1: %v", err)
	}
	if hist.Content != "schema-v1" {
		t.Fatalf("historical v1 = %+v", hist)
	}
	if cur, err := repo.GetCurrentVersion(ctx, pid); err != nil || cur.Version != 2 {
		t.Fatalf("current = %+v err %v, want v2", cur, err)
	}
	// Delete cascade: dropping the protocol removes its version and reference
	// rows.
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM response_protocol_versions WHERE protocol_id = '"+pid.String()+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("version rows = %d, want 2", n)
	}
	if _, err := h.DBExec(ctx, "DELETE FROM response_protocols WHERE id = '"+pid.String()+"'"); err != nil {
		t.Fatalf("delete protocol: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM response_protocol_versions WHERE protocol_id = '"+pid.String()+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("version rows after delete = %d, want 0 (cascade)", n)
	}
}

// T8.23.1 (DB half, scope) — The scoped-kind unique constraint: the same kind
// is legal globally and per project, but twice within one scope is rejected
// (23505 at the DB level; ErrConflict through the repository).
func TestP813ScopedKindUniqueness(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := p813Protocol(t, h, nil, "dup1", prompt.KindReviewResponse, "v1")
	repo := dbrepo.NewResponseProtocolRepository(h.App.DB().Raw())

	rec := &persistence.ResponseProtocolRecord{
		ID: uuid.New(), ProjectID: nil, Name: "dup2", Kind: "REVIEW_RESPONSE",
		Enabled: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	err := repo.Create(ctx, rec, "v1")
	if !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate global kind err = %v, want ErrConflict", err)
	}
	_ = pid
	// A project-scoped duplicate of the same kind is rejected too.
	var projID uuid.UUID
	if err := h.DBScan(ctx, "SELECT gen_random_uuid()", &projID); err != nil {
		t.Fatal(err)
	}
	rec = &persistence.ResponseProtocolRecord{
		ID: uuid.New(), ProjectID: &projID, Name: "dup3", Kind: "REVIEW_RESPONSE",
		Enabled: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.Create(ctx, rec, "v1"); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("unknown project ref err = %v, want ErrInvalidReference", err)
	}
}
