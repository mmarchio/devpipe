package integration

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/wbs"

	"github.com/google/uuid"
)

// newWBSVersionRepo wires the real WBS version repository to the harness pool.
func newWBSVersionRepo(t *testing.T, h *Harness) *dbrepo.WBSVersionRepository {
	t.Helper()
	return dbrepo.NewWBSVersionRepository(h.App.DB().Raw())
}

// createPhaseForWBS creates a project and one phase and returns the phase ID
// (string form, for inline SQL literals).
func createPhaseForWBS(t *testing.T, h *Harness) string {
	t.Helper()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)
	p, err := svc.CreatePhase(context.Background(), projectID, "WBS phase", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	return p.ID.String()
}

// seedWBSDoc inserts the phase_documents WBS row for the phase — the row the
// WBSDocumentService creates when a phase gets its WBS (WBS 3.5) and that
// carries the current-version pointer. The version repository itself does not
// create it (service wiring is P4-10).
func seedWBSDoc(t *testing.T, h *Harness, phaseID, content string) {
	t.Helper()
	docID := uuid.New().String()
	if _, err := h.DBExec(context.Background(),
		"INSERT INTO phase_documents (id, phase_id, doc_type, content, version, created_at, updated_at) "+
			"VALUES ('"+docID+"', '"+phaseID+"', 'WBS', '"+content+"', 1, now(), now())"); err != nil {
		t.Fatal(err)
	}
}

// T4.10.1 — First document creates v1: the first append allocates version 1,
// stores the exact content, computes its hash, and the phase's current
// version pointer moves to 1.
func TestT4101FirstAppendCreatesV1(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	seedWBSDoc(t, h, phaseID, "placeholder")
	repo := newWBSVersionRepo(t, h)

	v1, err := repo.AppendVersion(ctx, phaseID, "# Phase 1\n\n## 1.1 Scope\n")
	if err != nil {
		t.Fatalf("AppendVersion: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("first version = %d, want 1", v1.Version)
	}
	want := "# Phase 1\n\n## 1.1 Scope\n"
	if v1.Content != want {
		t.Fatalf("v1 content = %q, want %q", v1.Content, want)
	}
	if v1.ContentHash != wbs.HashContent(want) {
		t.Fatalf("v1 content_hash does not match wbs.HashContent(content)")
	}
	var cur int
	if err := h.DBScan(ctx,
		"SELECT current_version FROM phase_documents WHERE phase_id = '"+phaseID+"'", &cur); err != nil {
		t.Fatal(err)
	}
	if cur != 1 {
		t.Fatalf("phase_documents.current_version = %d, want 1", cur)
	}
	var storedHash string
	if err := h.DBScan(ctx,
		"SELECT content_hash FROM wbs_versions WHERE phase_id = '"+phaseID+"' AND version = 1", &storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != wbs.HashContent(want) {
		t.Fatalf("stored hash = %q, want %q", storedHash, wbs.HashContent(want))
	}
}

// T4.10.2 — Edit creates v2: a second append allocates version 2 and does not
// touch version 1.
func TestT4102EditCreatesV2(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	repo := newWBSVersionRepo(t, h)

	if _, err := repo.AppendVersion(ctx, phaseID, "v1 content"); err != nil {
		t.Fatal(err)
	}
	v2, err := repo.AppendVersion(ctx, phaseID, "v2 content")
	if err != nil {
		t.Fatalf("second AppendVersion: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("second version = %d, want 2", v2.Version)
	}
	var countV1 int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"' AND version = 1", &countV1); err != nil {
		t.Fatal(err)
	}
	if countV1 != 1 {
		t.Fatalf("v1 rows = %d, want 1 (never overwritten)", countV1)
	}
}

// T4.10.3 — Second edit creates v3.
func TestT4103SecondEditCreatesV3(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	repo := newWBSVersionRepo(t, h)

	for i, c := range []string{"v1 content", "v2 content", "v3 content"} {
		v, err := repo.AppendVersion(ctx, phaseID, c)
		if err != nil {
			t.Fatalf("AppendVersion %d: %v", i+1, err)
		}
		if v.Version != i+1 {
			t.Fatalf("append %d allocated version %d, want %d", i+1, v.Version, i+1)
		}
	}
}

// T4.10.4 — v1 remains retrievable: after later edits, the original content
// and content_hash are intact.
func TestT4104HistoricalVersionRetrievable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	repo := newWBSVersionRepo(t, h)

	const c1, c2, c3 = "original v1", "edited v2", "edited v3"
	for _, c := range []string{c1, c2, c3} {
		if _, err := repo.AppendVersion(ctx, phaseID, c); err != nil {
			t.Fatal(err)
		}
	}
	v1, err := repo.GetVersion(ctx, phaseID, 1)
	if err != nil {
		t.Fatalf("GetVersion(1): %v", err)
	}
	if v1.Content != c1 {
		t.Fatalf("v1 content = %q, want %q", v1.Content, c1)
	}
	if v1.ContentHash != wbs.HashContent(c1) {
		t.Fatalf("v1 hash = %q, want %q", v1.ContentHash, wbs.HashContent(c1))
	}
	v3, err := repo.GetVersion(ctx, phaseID, 3)
	if err != nil {
		t.Fatalf("GetVersion(3): %v", err)
	}
	if v3.Content != c3 {
		t.Fatalf("v3 content = %q, want %q", v3.Content, c3)
	}
	if _, err := repo.GetVersion(ctx, phaseID, 4); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("GetVersion(4): got %v, want ErrNotFound", err)
	}
}

// T4.10.5 — Current version points to the latest appended version.
func TestT4105CurrentVersionPointsToLatest(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	seedWBSDoc(t, h, phaseID, "placeholder")
	repo := newWBSVersionRepo(t, h)

	for i := 0; i < 3; i++ {
		if _, err := repo.AppendVersion(ctx, phaseID, "content "+itoa(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	cur, err := repo.GetCurrentVersion(ctx, phaseID)
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if cur != 3 {
		t.Fatalf("current version = %d, want 3 (latest)", cur)
	}
}

// T4.10.6 — Versions increase monotonically: every appended version is
// strictly greater than the previous, and the listing is ascending.
func TestT4106VersionsIncreaseMonotonically(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	repo := newWBSVersionRepo(t, h)

	var prev int
	for i := 0; i < 5; i++ {
		v, err := repo.AppendVersion(ctx, phaseID, "content "+itoa(i+1))
		if err != nil {
			t.Fatal(err)
		}
		if v.Version <= prev {
			t.Fatalf("version %d did not increase (prev %d)", v.Version, prev)
		}
		prev = v.Version
	}
	versions, err := repo.ListVersions(ctx, phaseID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 5 {
		t.Fatalf("listed %d versions, want 5", len(versions))
	}
	for i, v := range versions {
		if v.Version != i+1 {
			t.Fatalf("ListVersions is not ascending 1..5: got %d at position %d", v.Version, i+1)
		}
	}
}

// T4.10.7 — Concurrent edits cannot produce duplicate version numbers: N real
// goroutines appending against real PG must produce exactly the contiguous
// sequence 1..N (non-vacuous: real goroutines, real PG, exact sequence).
func TestT4107ConcurrentAppendsNoDuplicates(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	seedWBSDoc(t, h, phaseID, "placeholder")
	repo := newWBSVersionRepo(t, h)

	const n = 12
	results := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := repo.AppendVersion(ctx, phaseID, "concurrent content "+itoa(i))
			if err != nil {
				results <- -1 // signals failure; the test fatals below
				return
			}
			results <- v.Version
		}(i)
	}
	wg.Wait()
	close(results)

	versions := []int{}
	for v := range results {
		if v < 0 {
			t.Fatal("concurrent AppendVersion failed")
		}
		versions = append(versions, v)
	}
	sort.Ints(versions)
	for i, v := range versions {
		if v != i+1 {
			t.Fatalf("concurrent append versions = %v, want exactly 1..%d (no duplicates, no gaps)", versions, n)
		}
	}
	var count int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("concurrent appends persisted %d rows, want %d", count, n)
	}
	var cur int
	if err := h.DBScan(ctx,
		"SELECT current_version FROM phase_documents WHERE phase_id = '"+phaseID+"'", &cur); err != nil {
		t.Fatal(err)
	}
	if cur != n {
		t.Fatalf("current_version after concurrent appends = %d, want %d", cur, n)
	}
}

// T4.11.4 (persistence half) — Stored hash matches persisted content: for
// every version, re-hashing the content read back from the DB reproduces the
// stored content_hash.
func TestT4114StoredHashMatchesPersistedContent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	repo := newWBSVersionRepo(t, h)

	contents := []string{"# v1", "# v1 with edits", "# v2 entirely new"}
	for _, c := range contents {
		if _, err := repo.AppendVersion(ctx, phaseID, c); err != nil {
			t.Fatal(err)
		}
	}
	for i := range contents {
		v, err := repo.GetVersion(ctx, phaseID, i+1)
		if err != nil {
			t.Fatalf("GetVersion(%d): %v", i+1, err)
		}
		if v.ContentHash != wbs.HashContent(v.Content) {
			t.Fatalf("version %d: stored hash != wbs.HashContent(content read back)", i+1)
		}
	}
}

// T4.11.5 (persistence half) — Retrieval does not alter content/hash: repeated
// reads return byte-identical content and content_hash, with the stored row
// untouched.
func TestT4115RetrievalDoesNotAlterContent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	phaseID := createPhaseForWBS(t, h)
	repo := newWBSVersionRepo(t, h)

	const c = "stable content across reads"
	if _, err := repo.AppendVersion(ctx, phaseID, c); err != nil {
		t.Fatal(err)
	}
	first, err := repo.GetVersion(ctx, phaseID, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		v, err := repo.GetVersion(ctx, phaseID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if v.Content != first.Content || v.ContentHash != first.ContentHash {
			t.Fatalf("repeated read %d altered content/hash", i+1)
		}
	}
	var count int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("retrieval persisted %d rows, want 1", count)
	}
}
