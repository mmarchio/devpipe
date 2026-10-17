package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// P7-14 (WBS 7.20/7.21): state-file materialization + filesystem path
// safety. The StateFileMaterializeService optionally writes each
// project-scoped state file's CURRENT persisted content version to
// <root>/.overseer/state/<type's DefaultFilename> (WBS 7.20 layout).
//
// File-selection policy (stated in the service's code comments, the task
// NOTES, and pinned here): the files materialized for a project are the
// project's PROJECT-scoped state files whose ENABLED type has a
// non-empty DefaultFilename and whose CURRENT persisted content version
// exists (P7-06 content source of truth). A PROJECT-scoped file of a
// type with no filename (T7.20.5 fixture) and a file without content are
// skipped — the op continues with the remaining files (a file-level
// skip, not an op failure); an IO error (read/IO) is reported cleanly
// as an op error with the file identified (T7.20.5).
//
// Path-safety policy (WBS 7.21, T7.21.4): a configured filename is a
// managed RELATIVE name under the state dir; .. traversal and absolute
// paths are rejected; a symlink component is resolved and containment is
// re-verified AFTER resolution, so a symlink pointing outside the state
// dir is rejected (unit-level proof in internal/statefile; the integration
// half pins the end-to-end "writes nothing" via the op).
//
// Service-only surface (REST is P7-22); the tests drive the op over the
// harness pool.

// p714Svc wires the materialization service against the harness pool.
func p714Svc(t *testing.T, h *Harness) *service.StateFileMaterializeService {
	t.Helper()
	raw := h.App.DB().Raw()
	return service.NewStateFileMaterializeService(
		dbrepo.NewStateFileTypeRepository(raw),
		dbrepo.NewStateFileRepository(raw),
		dbrepo.NewStateFileVersionRepository(raw),
	)
}

// p714Content appends the given content as the file's NEXT version
// (the current version is re-read first — a stale expected version is
// ErrConflict).
func p714Content(t *testing.T, h *Harness, fileID uuid.UUID, content string) {
	t.Helper()
	svc := p708Service(t, h)
	cur, err := svc.GetCurrentVersion(context.Background(), fileID.String())
	if err != nil {
		t.Fatalf("get current version: %v", err)
	}
	expected := 0
	if cur != nil {
		expected = cur.Version
	}
	if _, err := svc.UpdateContent(context.Background(), fileID.String(), content, expected); err != nil {
		t.Fatalf("update content: %v", err)
	}
}

// p714WriteFile creates a file with content for the given type (content
// "" = no version), returning the file id.
func p714WriteFile(t *testing.T, h *Harness, projectID, typeID uuid.UUID, content string) uuid.UUID {
	t.Helper()
	fileID := p713ProjectFile(t, h, projectID, typeID)
	if content != "" {
		p714Content(t, h, fileID, content)
	}
	return fileID
}

// p714Sig is a signature of every file under dir (path + content).
func p714Sig(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = path
		}
		lines = append(lines, rel+"="+string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return strings.Join(lines, "\n")
}

// T7.20.1 — Files materialized to the configured location: a project's
// materialization writes to <root>/.overseer/state/<DefaultFilename> for
// each PROJECT-scoped file with content (the WBS 7.20 layout).
func TestP714MaterializesToConfiguredLocation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P14A", "PROJECT", false), "alpha content")
	p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P14B", "PROJECT", false), "beta content")
	root := t.TempDir()

	if _, err := p714Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	stateDir := filepath.Join(root, ".overseer", "state")
	a, err := os.ReadFile(filepath.Join(stateDir, "P14A.md"))
	if err != nil || string(a) != "alpha content" {
		t.Fatalf("P14A.md = %q (err %v)", a, err)
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "P14B.md"))
	if err != nil || string(b) != "beta content" {
		t.Fatalf("P14B.md = %q (err %v)", b, err)
	}
}

// T7.20.2 — Directories created where appropriate: the .overseer/state
// namespace is created by the op (the root starts empty).
func TestP714CreatesDirectories(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P14C", "PROJECT", false), "c")
	root := t.TempDir()
	if _, err := os.Stat(filepath.Join(root, ".overseer")); !os.IsNotExist(err) {
		t.Fatalf(".overseer pre-exists (err %v)", err)
	}
	if _, err := p714Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	fi, err := os.Stat(filepath.Join(root, ".overseer", "state"))
	if err != nil {
		t.Fatalf("state dir missing after op: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("state dir is not a directory")
	}
}

// T7.20.3 — Content matches the persisted version: the materialized bytes
// are the file's CURRENT version (after an update, the new version — not
// the historical one).
func TestP714ContentMatchesPersistedVersion(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	typeID := p713Required(t, h, projectID, "P14D", "PROJECT", false)
	fileID := p714WriteFile(t, h, projectID, typeID, "v1 content")
	p714Content(t, h, fileID, "v2 content")
	root := t.TempDir()

	if _, err := p714Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".overseer", "state", "P14D.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "v2 content" {
		t.Fatalf("materialized = %q, want current version %q", got, "v2 content")
	}
}

// T7.20.4 — Existing unrelated project files untouched: pre-existing
// project files (outside and inside the .overseer namespace) are
// byte-identical after the op.
func TestP714UnrelatedFilesUntouched(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P14E", "PROJECT", false), "e")
	root := t.TempDir()
	// Pre-existing unrelated files: a source file at the root and a file
	// inside .overseer (sibling of state/).
	unrelatedRoot := filepath.Join(root, "main.go")
	if err := os.WriteFile(unrelatedRoot, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".overseer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	unrelatedInner := filepath.Join(root, ".overseer", "config.yaml")
	if err := os.WriteFile(unrelatedInner, []byte("k: v\n"), 0o644); err != nil {
		t.Fatalf("write unrelated inner: %v", err)
	}
	before := p714Sig(t, root)

	if _, err := p714Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Signature of only the pre-existing files (the op adds P14E.md to
	// state/, which is a new file, not a mutation).
	for _, p := range []string{unrelatedRoot, unrelatedInner} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if !strings.Contains(before, p714Rel(root, p)+"="+string(b)) {
			t.Fatalf("unrelated file %s changed", p)
		}
	}
}

// p714Rel is a rel-path helper for p714Sig comparisons.
func p714Rel(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		rel = path
	}
	return rel
}

// T7.20.5 — Materialization failure reported cleanly: (a) a PROJECT-scoped
// file whose type has no DefaultFilename is skipped (nothing written for
// it, no error — the op continues); (b) a read/IO failure is a clean op
// error naming the file — the materialized state dir is left in a
// deterministic state (nothing is written for the failing file).
func TestP714FailureReportedCleanly(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)

	// (a) A file whose type carries no DefaultFilename: skipped cleanly.
	noName := p713Required(t, h, projectID, "P14NONAME", "PROJECT", false)
	if _, err := h.DBExec(ctx, "UPDATE state_file_types SET default_filename = '' WHERE id = '"+noName.String()+"'"); err != nil {
		t.Fatalf("clear filename: %v", err)
	}
	p714WriteFile(t, h, projectID, noName, "content with no filename")
	// A file without content: also skipped cleanly.
	typeID := p713Required(t, h, projectID, "P14NOCONTENT", "PROJECT", false)
	p713ProjectFile(t, h, projectID, typeID)
	goodID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P14GOOD", "PROJECT", false), "good content")
	_ = goodID
	root := t.TempDir()

	if _, err := p714Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize with skipped files: %v", err)
	}
	stateDir := filepath.Join(root, ".overseer", "state")
	if _, err := os.Stat(filepath.Join(stateDir, "P14NONAME")); !os.IsNotExist(err) {
		t.Fatalf("file without filename materialized (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "P14NOCONTENT.md")); !os.IsNotExist(err) {
		t.Fatalf("file without content materialized (err %v)", err)
	}
	got, err := os.ReadFile(filepath.Join(stateDir, "P14GOOD.md"))
	if err != nil || string(got) != "good content" {
		t.Fatalf("P14GOOD.md = %q (err %v)", got, err)
	}

	// (b) An IO failure (a pre-existing DIRECTORY where the file should
	// be written) is a clean op error, not a panic; the failing file's
	// write did not happen, and no partial bytes were left.
	root2 := t.TempDir()
	badName := p713Required(t, h, projectID, "P14IOBAD", "PROJECT", false)
	p714WriteFile(t, h, projectID, badName, "io content")
	if err := os.MkdirAll(filepath.Join(root2, ".overseer", "state", "P14IOBAD.md"), 0o755); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}
	_, err = p714Svc(t, h).Materialize(ctx, root2, projectID.String())
	if err == nil {
		t.Fatalf("materialize over a directory: expected a clean error, got nil")
	}
	if !strings.Contains(err.Error(), "P14IOBAD.md") {
		t.Fatalf("error does not identify the failing file: %v", err)
	}
	if fi, statErr := os.Stat(filepath.Join(root2, ".overseer", "state", "P14IOBAD.md")); statErr != nil || !fi.IsDir() {
		t.Fatalf("blocker changed by the op (err %v, isDir %v)", statErr, fi.IsDir())
	}
}

// T7.21.1 (integration half) — A normal relative filename is accepted:
// covered end-to-end by TestP714MaterializesToConfiguredLocation (the
// DefaultFilename writes into the state dir).

// T7.21.2/T7.21.3/T7.21.5 (integration half) — .. traversal, absolute
// paths, and invalid filenames write nothing: a type whose
// DefaultFilename would escape the state dir is rejected with a clean
// error and nothing is written anywhere (inside or outside the state
// dir).
func TestP714PathSafetyWritesNothing(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		fragile string
	}{
		{"traversal", "../outside.md"},
		{"absolute", "/outside/abs.md"},
		{"invalid", ""},
	} {
		// A fresh project per case: earlier cases' files (with escape
		// filenames) must not pollute later cases' op results.
		projectID, _, _ := p705Rig(t, h)
		root := t.TempDir()
		bad := p713Required(t, h, projectID, "P14BAD"+strings.ToUpper(tc.name), "PROJECT", false)
		if _, err := h.DBExec(ctx, "UPDATE state_file_types SET default_filename = '"+tc.fragile+"' WHERE id = '"+bad.String()+"'"); err != nil {
			t.Fatalf("set filename: %v", err)
		}
		p714WriteFile(t, h, projectID, bad, "bad content")
		_, err := p714Svc(t, h).Materialize(ctx, root, projectID.String())
		if tc.fragile == "" {
			// Invalid (empty) filename: skipped, nothing written, no error.
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			if _, statErr := os.Stat(filepath.Join(root, ".overseer", "state")); !os.IsNotExist(statErr) {
				t.Fatalf("%s: state dir created for invalid filename", tc.name)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: escape path accepted (no error)", tc.name)
		}
		// Nothing written outside the state dir.
		if _, statErr := os.Stat(filepath.Join(root, "..", "outside.md")); !os.IsNotExist(statErr) {
			t.Fatalf("%s: wrote outside the state dir", tc.name)
		}
		// The state dir (if created at all) carries no file for it.
		entries, _ := os.ReadDir(filepath.Join(root, ".overseer", "state"))
		for _, e := range entries {
			if e.Name() == "outside.md" {
				t.Fatalf("%s: escape file landed in the state dir", tc.name)
			}
		}
	}
}

// T7.21.4 (integration half) — Symlink escape prevented per the documented
// policy (resolve, verify containment after resolution): a symlink
// component pointing outside the state dir is rejected and nothing is
// written outside it.
func TestP714SymlinkEscapePrevented(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".overseer", "state"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".overseer", "state", "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	bad := p713Required(t, h, projectID, "P14SYMLINK", "PROJECT", false)
	if _, err := h.DBExec(ctx, "UPDATE state_file_types SET default_filename = 'link/escape.md' WHERE id = '"+bad.String()+"'"); err != nil {
		t.Fatalf("set filename: %v", err)
	}
	p714WriteFile(t, h, projectID, bad, "escape content")
	if _, err := p714Svc(t, h).Materialize(ctx, root, projectID.String()); err == nil {
		t.Fatalf("symlink escape accepted (no error)")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "escape.md")); !os.IsNotExist(statErr) {
		t.Fatalf("wrote through the symlink: %v", statErr)
	}
}

var _ = persistence.ErrNotFound // (kept for future error-assertion use)
