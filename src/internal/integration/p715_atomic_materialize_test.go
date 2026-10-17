package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/statefile"
)

// P7-15 (WBS 7.22/7.23): atomic materialized writes + materialized-file
// metadata. Materialization writes go through a temporary file, fsync,
// rename (a crash must not leave a partial file — T7.22.1-T7.22.4), and
// each successful write persists one CURRENT record per state file
// (state_file_id, version, content_hash, path, materialized_at) that
// drift detection (P7-16) can compare against (T7.23.1-T7.23.4).
//
// Failure-injection seam (stated per the task NOTES and pinned here): the
// service's write step is a swappable field (default atomicWriteFile);
// T7.22.2 swaps it for a function that fails on the temp write, so the
// "previous file intact" assertion is driven by a FORCED IO error, not by
// luck.

// p715Svc wires the materialization service WITH the metadata repository
// (WBS 7.23) against the harness pool.
func p715Svc(t *testing.T, h *Harness) *service.StateFileMaterializeService {
	t.Helper()
	raw := h.App.DB().Raw()
	return p714Svc(t, h).
		WithMaterializedMetadata(dbrepo.NewMaterializedStateFileRepository(raw))
}

// p715MetaRow reads the materialized metadata row for a file by direct
// SQL; returns false when no row exists.
func p715MetaRow(t *testing.T, h *Harness, fileID string) (version int, contentHash, path, materializedAt string) {
	t.Helper()
	ctx := context.Background()
	err := h.DBScan(ctx, `SELECT version, content_hash, path, materialized_at::text
		FROM materialized_state_files WHERE state_file_id = '`+fileID+`'`,
		&version, &contentHash, &path, &materializedAt)
	if errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("scan meta: %v", err)
	}
	if err != nil {
		// Distinguish not-found (zero rows -> driver no-rows error) from a
		// real failure.
		if strings.Contains(err.Error(), "no rows in result set") {
			return 0, "", "", ""
		}
		t.Fatalf("scan meta: %v", err)
	}
	return version, contentHash, path, materializedAt
}

// p715MetaCount is the row count for a file's metadata record.
func p715MetaCount(t *testing.T, h *Harness, fileID string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(), `SELECT count(*) FROM materialized_state_files
		WHERE state_file_id = '`+fileID+`'`, &n); err != nil {
		t.Fatalf("count meta: %v", err)
	}
	return n
}

// T7.22.1 — Successful write produces full content: the materialized file
// carries the ENTIRE current version's bytes (not a prefix) — including a
// content payload large enough that a torn write would be visible.
func TestP715AtomicWriteFullContent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	payload := strings.Repeat("state file line\n", 500) // ~8.5KB of full content
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P15A", "PROJECT", false), payload)
	_ = fileID
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".overseer", "state", "P15A.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("materialized content length = %d, want the full %d bytes (no torn write)", len(got), len(payload))
	}
}

// T7.22.2 — Failed write leaves the previous file intact: with the
// write step forced to fail on the TEMP write (the failure-injection
// seam), a second materialization fails cleanly and the FIRST write's
// file is byte-identical afterward — the failed attempt never touched
// the target (the rename is the only target mutation).
func TestP715FailedWriteLeavesPreviousIntact(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P15B", "PROJECT", false), "previous version")
	root := t.TempDir()
	stateDir := filepath.Join(root, ".overseer", "state")

	// First materialization succeeds (baseline file).
	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("baseline materialize: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(stateDir, "P15B.md"))
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}

	// Update to a new version, then force the write to fail on the temp
	// write (seam swap) and materialize again.
	p714Content(t, h, fileID, "newer version that never lands")
	svc := p715Svc(t, h)
	svc.WriteFile = func(path string, data []byte) error {
		return errors.New("injected temp-write failure (T7.22.2)")
	}
	_, failErr := svc.Materialize(ctx, root, projectID.String())
	if failErr == nil {
		t.Fatalf("forced failure produced no error")
	}
	if !strings.Contains(failErr.Error(), "P15B.md") {
		t.Fatalf("error does not identify the failing file: %v", failErr)
	}
	after, err := os.ReadFile(filepath.Join(stateDir, "P15B.md"))
	if err != nil {
		t.Fatalf("previous file unreadable after failed write: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("previous file changed by the failed write: %q != %q", after, before)
	}
	// The failed attempt recorded no metadata (the record reflects what is
	// on disk; the write never completed).
	if n := p715MetaCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("metadata rows = %d, want 1 (the successful baseline only)", n)
	}
	v, _, _, _ := p715MetaRow(t, h, fileID.String())
	if v != 1 {
		t.Fatalf("recorded version = %d, want 1 (the failed v2 write must not be recorded)", v)
	}
}

// T7.22.3 — Temporary files cleaned where possible: after a materialization
// (successful) there are no leftover temporary files anywhere under the
// state directory, and after a FORCED failed write (same seam as T7.22.2)
// the temp file the failing writer created is removed.
func TestP715TempFilesCleaned(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P15C", "PROJECT", false), "c content")
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	leftovers := p715TempFiles(t, root)
	if len(leftovers) != 0 {
		t.Fatalf("leftover temp files after successful materialization: %v", leftovers)
	}

	// Forced failure on the temp write: the failing writer must clean up
	// its own temp file.
	p714Content(t, h, fileID, "second version")
	svc := p715Svc(t, h)
	svc.WriteFile = func(path string, data []byte) error {
		return errors.New("injected temp-write failure (T7.22.3)")
	}
	if _, failErr := svc.Materialize(ctx, root, projectID.String()); failErr == nil {
		t.Fatalf("forced failure produced no error")
	}
	leftovers = p715TempFiles(t, root)
	if len(leftovers) != 0 {
		t.Fatalf("leftover temp files after forced failed write: %v", leftovers)
	}
}

// p715TempFiles lists files matching the atomic-write temp pattern under
// dir (recursively).
func p715TempFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), ".materialize-") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// T7.22.4 — Concurrent materialization never produces partial content:
// real goroutines materialize the SAME project concurrently over real PG
// and the real filesystem. The current version is bumped between batches,
// so racers materialize different complete versions; a watcher loop reads
// the target file throughout the race and asserts every observed state is
// a COMPLETE version's bytes (never a partial), and the final state is
// one complete version.
func TestP715ConcurrentMaterializationNeverPartial(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P15D", "PROJECT", false), "concurrent v1")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "P15D.md")

	const versions = 6
	// The set of COMPLETE version byte-strings the watchers may observe,
	// guarded by a mutex (the advancer writes it while the watcher reads
	// it — the -race gate applies to test memory too).
	var completeMu sync.Mutex
	complete := map[string]bool{}
	addComplete := func() {
		ver, err := p708Service(t, h).GetCurrentVersion(ctx, fileID.String())
		if err != nil {
			t.Fatalf("get current: %v", err)
		}
		completeMu.Lock()
		complete[ver.Content] = true
		completeMu.Unlock()
	}
	addComplete()
	isComplete := func(b []byte) bool {
		completeMu.Lock()
		defer completeMu.Unlock()
		return complete[string(b)]
	}

	var wg sync.WaitGroup
	// Watcher: continuously read the target; every observed content must
	// be a complete version's bytes.
	watch := make(chan struct{})
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			case <-watch:
				return
			default:
			}
			b, err := os.ReadFile(target)
			if err != nil {
				if os.IsNotExist(err) {
					continue // not written yet
				}
				t.Errorf("watch read: %v", err)
				return
			}
			if !isComplete(b) {
				t.Errorf("observed PARTIAL content (len %d): not a complete version's bytes", len(b))
				return
			}
		}
	}()

	// Racers: each goroutine materializes repeatedly; between batches the
	// CURRENT version advances, so concurrent ops write different
	// complete versions of the SAME file.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; r < 10; r++ {
				if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
					t.Errorf("goroutine %d materialize: %v", g, err)
					return
				}
			}
		}(g)
	}
	// Version-advancer: appends new complete versions while the race runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := 2; v < versions; v++ {
			// Register the complete content BEFORE it becomes current: a
			// racer may materialize it immediately (Phase 16 gate: the
			// registration-after-write window flaked under -race).
			content := "concurrent v" + itoa(v) + " padding padding padding"
			completeMu.Lock()
			complete[content] = true
			completeMu.Unlock()
			p714Content(t, h, fileID, content)
			addComplete()
		}
	}()
	wg.Wait()
	close(watch)

	// Final state is one complete version's bytes.
	final, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if !isComplete(final) {
		t.Fatalf("final file content is not a complete version (len %d)", len(final))
	}
}

// T7.23.1 — Materialization record created: after a materialization the
// metadata table carries a row for the file with all WBS 7.23 fields
// (state_file_id, version, content_hash, path, materialized_at);
// materialized_at is UTC.
func TestP715MaterializationRecordCreated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P15E", "PROJECT", false), "record content")
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	version, contentHash, path, materializedAt := p715MetaRow(t, h, fileID.String())
	if version == 0 || contentHash == "" || path == "" || materializedAt == "" {
		t.Fatalf("record incomplete: version=%d hash=%q path=%q at=%q", version, contentHash, path, materializedAt)
	}
	if n := p715MetaCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("row count = %d, want 1", n)
	}
	// materialized_at is UTC (assert the timezone name/offset, not
	// Location identity).
	name, off := p715ParseTime(t, materializedAt).Zone()
	if name != "UTC" || off != 0 {
		t.Fatalf("materialized_at zone = %s/%d, want UTC/0", name, off)
	}
}

// p715ParseTime parses the timestamptz text the harness returns.
func p715ParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	// The harness returns timestamptz text like
	// "2006-01-02 15:04:05.999999+00" (fractional seconds + a compact 3-char
	// zone offset, no space before it). Strip a trailing compact offset and
	// normalize it to the colon form (+00 -> +00:00) so the standard
	// Z07:00 layout parses it.
	zone := "Z"
	if len(s) >= 3 && (s[len(s)-3] == '+' || s[len(s)-3] == '-') && (s[len(s)-3] == '+' || s[len(s)-3] == '-') {
		zone = s[len(s)-3:] + ":00"
		s = s[:len(s)-3]
	}
	layouts := []string{
		"2006-01-02 15:04:05.999999999 Z07:00",
		"2006-01-02 15:04:05 Z07:00",
	}
	for _, layout := range layouts {
		if ts, err := time.Parse(layout, s+" "+zone); err == nil {
			return ts
		}
	}
	t.Fatalf("parse materialized_at %q: no layout matched", s+" "+zone)
	return time.Time{}
}

// T7.23.2 — Correct version recorded: the record's version is the version
// that was WRITTEN (the current version at materialization time).
func TestP715RecordedVersionIsWrittenVersion(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P15F", "PROJECT", false), "v1")
	// Advance to v3 BEFORE materializing: the record must say 3, not 1.
	p714Content(t, h, fileID, "v2")
	p714Content(t, h, fileID, "v3")
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	version, _, _, _ := p715MetaRow(t, h, fileID.String())
	if version != 3 {
		t.Fatalf("recorded version = %d, want the written version 3", version)
	}
}

// T7.23.3 — Correct hash recorded: the record's content_hash is the
// content hash of the written bytes (statefile.ContentHash over the
// version's content — and equals the hash of the on-disk file's bytes).
func TestP715RecordedHashIsWrittenBytesHash(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	content := "hash content for p15-g"
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P15G", "PROJECT", false), content)
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	_, contentHash, _, _ := p715MetaRow(t, h, fileID.String())
	want := statefile.ContentHash(content)
	if contentHash != want {
		t.Fatalf("recorded hash = %q, want %q (hash of the written bytes)", contentHash, want)
	}
	// Cross-check against the on-disk bytes: hash(file) == recorded hash.
	onDisk, err := os.ReadFile(filepath.Join(root, ".overseer", "state", "P15G.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if statefile.ContentHash(string(onDisk)) != contentHash {
		t.Fatalf("on-disk hash %q != recorded %q", statefile.ContentHash(string(onDisk)), contentHash)
	}
}

// T7.23.4 — Re-materialization UPDATES the same record (not duplicates):
// across multiple re-materializations (including a version bump) the row
// count stays 1, and the row's version/hash move to the newly written
// values in place.
func TestP715ReMaterializationUpdatesRecord(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P15H", "PROJECT", false), "h v1")
	root := t.TempDir()
	svc := p715Svc(t, h)

	if _, err := svc.Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	// Same-version re-materialization: the record is updated in place.
	if _, err := svc.Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if n := p715MetaCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("after re-materialization rows = %d, want 1 (update in place, not duplicates)", n)
	}
	v, _, _, _ := p715MetaRow(t, h, fileID.String())
	if v != 1 {
		t.Fatalf("version after same-version re-materialize = %d, want 1", v)
	}
	// A version bump re-materializes and the SAME row now records v2.
	p714Content(t, h, fileID, "h v2")
	if _, err := svc.Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize v2: %v", err)
	}
	if n := p715MetaCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("after v2 materialization rows = %d, want 1", n)
	}
	v, contentHash, _, _ := p715MetaRow(t, h, fileID.String())
	if v != 2 || contentHash != statefile.ContentHash("h v2") {
		t.Fatalf("record = v%d/%q, want v2 with the hash of the v2 bytes", v, contentHash)
	}
}
