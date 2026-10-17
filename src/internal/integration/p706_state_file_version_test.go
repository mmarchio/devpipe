package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/statefile"
)

// P7-06 (WBS 7.5/7.6 persistence half + 7.8): migration 00036
// (state_file_versions) and the version repository — advisory-locked MAX+1
// allocation, deterministic content hash, optimistic concurrency — against
// real PostgreSQL.

// p706File seeds a project + a PROJECT-scoped state file (type + file rows)
// and returns the file id.
func p706File(t *testing.T, h *Harness) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	projectStr := createPhaseProject(t, h)
	projectID, err := uuid.Parse(projectStr)
	if err != nil {
		t.Fatalf("parse project id: %v", err)
	}
	typeRepo := dbrepo.NewStateFileTypeRepository(h.App.DB().Raw())
	typeRec := p705Type(t, &projectID, "project-state", "PROJECT")
	if err := typeRepo.Create(ctx, typeRec); err != nil {
		t.Fatalf("create type: %v", err)
	}
	fileRepo := dbrepo.NewStateFileRepository(h.App.DB().Raw())
	rec := &persistence.StateFileRecord{
		ID: uuid.New(), ProjectID: projectID, TypeID: typeRec.ID, Scope: "PROJECT",
		CreatedAt: nowUTC(t), UpdatedAt: nowUTC(t),
	}
	if err := fileRepo.Create(ctx, rec); err != nil {
		t.Fatalf("create file: %v", err)
	}
	return rec.ID
}

// p706Current reads the file's current_version pointer by direct SQL.
func p706Current(t *testing.T, h *Harness, fileID uuid.UUID) int {
	t.Helper()
	var cur int
	if err := h.DBScan(context.Background(),
		"SELECT current_version FROM state_files WHERE id = '"+fileID.String()+"'", &cur); err != nil {
		t.Fatalf("scan current_version: %v", err)
	}
	return cur
}

// p706CountVersions counts the stored version rows by direct SQL.
func p706CountVersions(t *testing.T, h *Harness, fileID uuid.UUID) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

// T7.5.1 DB half — First content creates v1.
func TestP706FirstContentCreatesV1(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	v, err := repo.UpdateContent(ctx, fileID, "initial", 0)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if v.Version != 1 || v.Content != "initial" || v.StateFileID != fileID {
		t.Fatalf("v1 = %+v", v)
	}
	if p706CountVersions(t, h, fileID) != 1 {
		t.Fatalf("version rows = %d, want 1", p706CountVersions(t, h, fileID))
	}
}

// T7.5.2 DB half — An update creates v2; T7.5.3 DB half — historical v1
// remains retrievable.
func TestP706UpdateAdvancesAndHistoryRetained(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	if _, err := repo.UpdateContent(ctx, fileID, "one", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2, err := repo.UpdateContent(ctx, fileID, "two", 1)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.Version != 2 || v2.Content != "two" {
		t.Fatalf("v2 = %+v", v2)
	}
	hist, err := repo.GetVersion(ctx, fileID, 1)
	if err != nil {
		t.Fatalf("historical v1: %v", err)
	}
	if hist.Content != "one" {
		t.Fatalf("historical v1 content = %q, want one", hist.Content)
	}
}

// T7.5.4 DB half — The current version advances (pointer + repository read).
func TestP706CurrentVersionAdvances(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	if _, err := repo.UpdateContent(ctx, fileID, "a", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if got := p706Current(t, h, fileID); got != 1 {
		t.Fatalf("current_version = %d, want 1", got)
	}
	if _, err := repo.UpdateContent(ctx, fileID, "b", 1); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if got := p706Current(t, h, fileID); got != 2 {
		t.Fatalf("current_version = %d, want 2", got)
	}
	cur, err := repo.GetCurrentVersion(ctx, fileID)
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if cur.Version != 2 || cur.Content != "b" {
		t.Fatalf("current = %+v, want v2/b", cur)
	}
}

// T7.5.5 DB half — Concurrent writers never create duplicate versions: 20
// real goroutines race the repository against real PG, each retrying on
// ErrConflict after re-reading the current version; the committed versions
// are exactly the contiguous sequence 1..20.
func TestP706ConcurrentWritersNoDuplicateVersions(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each racer: read current, attempt, retry on conflict.
			for attempt := 0; attempt < 50; attempt++ {
				expected := 0
				cur, err := repo.GetCurrentVersion(ctx, fileID)
				if err != nil {
					errs <- fmt.Errorf("racer %d: read current: %w", i, err)
					return
				}
				if cur != nil {
					expected = cur.Version
				}
				_, uerr := repo.UpdateContent(ctx, fileID, fmt.Sprintf("r%d", i), expected)
				if uerr == nil {
					return
				}
				if !errors.Is(uerr, persistence.ErrConflict) {
					errs <- uerr
					return
				}
			}
			errs <- fmt.Errorf("racer %d: exhausted retries", i)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("racer: %v", err)
	}
	versions, err := repo.ListVersions(ctx, fileID)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != writers {
		t.Fatalf("committed versions = %d, want %d", len(versions), writers)
	}
	for i, v := range versions {
		if v.Version != i+1 {
			t.Fatalf("versions not contiguous: position %d = %d, want %d", i, v.Version, i+1)
		}
	}
	if got := p706Current(t, h, fileID); got != writers {
		t.Fatalf("current_version = %d, want %d", got, writers)
	}
}

// T7.5.6 DB half — A version number never moves backward: after reaching
// vN the stored sequence stays 1..N and every number is present exactly
// once (no gaps, no reuse).
func TestP706VersionNeverMovesBackward(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	for i := 0; i < 3; i++ {
		if _, err := repo.UpdateContent(ctx, fileID, fmt.Sprintf("c%d", i), i); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	versions, err := repo.ListVersions(ctx, fileID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[int]int{}
	for _, v := range versions {
		seen[v.Version]++
	}
	if len(seen) != 3 {
		t.Fatalf("distinct versions = %v, want exactly {1,2,3}", seen)
	}
	for n := 1; n <= 3; n++ {
		if seen[n] != 1 {
			t.Fatalf("version %d count = %d, want 1 (no gaps, no reuse)", n, seen[n])
		}
	}
	if _, err := repo.GetVersion(ctx, fileID, 4); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing version err = %v, want ErrNotFound", err)
	}
}

// T7.6.1–T7.6.5 DB half — Hash generated, deterministic over the exact
// bytes, changed content changes the hash, the persisted hash matches the
// content, and retrieval does not alter it.
func TestP706ContentHashPersistedDeterministic(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	same := "identical-bytes"
	v1, err := repo.UpdateContent(ctx, fileID, same, 0)
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if v1.ContentHash == "" {
		t.Fatal("T7.6.1: no hash generated")
	}
	if v1.ContentHash != statefile.ContentHash(same) {
		t.Fatalf("T7.6.4: stored hash does not match content")
	}
	// Identical content in a new version produces the identical hash
	// (T7.6.2); changed content changes it (T7.6.3).
	v2, err := repo.UpdateContent(ctx, fileID, same, 1)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.ContentHash != v1.ContentHash {
		t.Fatalf("T7.6.2: identical content, different hashes")
	}
	v3, err := repo.UpdateContent(ctx, fileID, "changed", 2)
	if err != nil {
		t.Fatalf("v3: %v", err)
	}
	if v3.ContentHash == v1.ContentHash {
		t.Fatal("T7.6.3: changed content produced an identical hash")
	}
	// Retrieval does not alter the hash (T7.6.5).
	got, err := repo.GetVersion(ctx, fileID, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if got.ContentHash != v1.ContentHash || got.Content != v1.Content {
		t.Fatalf("T7.6.5: retrieval altered the stored version: %+v", got)
	}
	// Token estimate is stored as metadata (bytes/4 heuristic).
	if v1.TokenEstimate != statefile.TokenEstimate(same) {
		t.Fatalf("token estimate = %d, want %d", v1.TokenEstimate, statefile.TokenEstimate(same))
	}
}

// T7.7.2 + T7.7.3 + T7.7.4 DB half — Current version retrieved, historical
// version retrieved, scope (per-file listing) query works.
func TestP706VersionRetrievalOps(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	contents := []string{"first", "second", "third"}
	for i, c := range contents {
		if _, err := repo.UpdateContent(ctx, fileID, c, i); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	cur, err := repo.GetCurrentVersion(ctx, fileID)
	if err != nil {
		t.Fatalf("T7.7.2 current: %v", err)
	}
	if cur.Version != 3 || cur.Content != "third" {
		t.Fatalf("current = %+v, want v3/third", cur)
	}
	hist, err := repo.GetVersion(ctx, fileID, 2)
	if err != nil {
		t.Fatalf("T7.7.3 historical: %v", err)
	}
	if hist.Content != "second" {
		t.Fatalf("historical = %+v", hist)
	}
	list, err := repo.ListVersions(ctx, fileID)
	if err != nil {
		t.Fatalf("T7.7.4 list: %v", err)
	}
	if len(list) != 3 || list[0].Version != 1 || list[2].Version != 3 {
		t.Fatalf("list = %+v", list)
	}
	// Unknown file / version yield the domain not-found error.
	if _, err := repo.GetCurrentVersion(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown file err = %v, want ErrNotFound", err)
	}
}

// T7.8.1 + T7.8.2 — A current-version write succeeds and the version
// increments.
func TestP706CurrentVersionWriteSucceeds(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	if _, err := repo.UpdateContent(ctx, fileID, "base", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2, err := repo.UpdateContent(ctx, fileID, "next", 1)
	if err != nil {
		t.Fatalf("T7.8.1 current-version write failed: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("T7.8.2 version = %d, want 2", v2.Version)
	}
}

// T7.8.3 + T7.8.4 — A stale write is rejected ErrConflict and persists no
// version row.
func TestP706StaleWriteRejectedAndNotPersisted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	if _, err := repo.UpdateContent(ctx, fileID, "base", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if _, err := repo.UpdateContent(ctx, fileID, "advance", 1); err != nil {
		t.Fatalf("v2: %v", err)
	}
	// Both writers read v1; the second's expected version is now stale.
	_, err := repo.UpdateContent(ctx, fileID, "stale", 1)
	if !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("T7.8.3 stale err = %v, want ErrConflict", err)
	}
	if n := p706CountVersions(t, h, fileID); n != 2 {
		t.Fatalf("T7.8.4 version rows = %d, want 2 (stale content not persisted)", n)
	}
	if cur := p706Current(t, h, fileID); cur != 2 {
		t.Fatalf("T7.8.4 current_version = %d, want 2 (unchanged)", cur)
	}
}

// T7.8.5 — Conflict is distinguishable from an infrastructure failure:
// ErrConflict matches, the infrastructure sentinel does not.
func TestP706ConflictDistinguishableFromInfrastructure(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fileID := p706File(t, h)
	repo := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw())

	if _, err := repo.UpdateContent(ctx, fileID, "base", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	_, err := repo.UpdateContent(ctx, fileID, "stale", 0)
	if !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if errors.Is(err, persistence.ErrNotFound) ||
		errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("conflict confused with another domain error: %v", err)
	}
	var infra *persistence.InfrastructureError
	if errors.As(err, &infra) {
		t.Fatalf("conflict surfaced as infrastructure error: %v", err)
	}
}

// P7-06.1 — Migration 00036 applies on a fresh harness DB; the pinned max
// schema_migrations version moves 35 -> 36.
func TestP706MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 35 -> 36 by P7-06 (migration 00036) — accepted contract
	// change, not a weakened test.
	if version != 126 {
		t.Fatalf("max migration version = %d, want 82", version)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_file_versions'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("state_file_versions present = %d, want 1", n)
	}
}
