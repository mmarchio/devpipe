package statefile

import (
	"os"
	"path/filepath"
	"testing"
)

// P7-14 (WBS 7.21): filesystem path safety. State-file materialization must
// never escape the configured state directory: configured filenames are
// managed relative names (T7.21.1); .. traversal (T7.21.2) and absolute
// paths (T7.21.3) are rejected; a symlink escape is prevented by resolving
// the path and verifying containment after resolution (T7.21.4, the
// documented policy); an invalid filename writes nothing (T7.21.5).

// T7.21.1 — A normal relative filename is accepted (as is a nested relative
// name, which stays inside the state directory).
func TestSafeRelPathAcceptsRelativeNames(t *testing.T) {
	base := t.TempDir()
	got, err := SafeRelPath(base, "PROJECT_STATE.md")
	if err != nil {
		t.Fatalf("relative name rejected: %v", err)
	}
	if want := filepath.Join(base, "PROJECT_STATE.md"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	got, err = SafeRelPath(base, "nested/inner.md")
	if err != nil {
		t.Fatalf("nested relative name rejected: %v", err)
	}
	if want := filepath.Join(base, "nested", "inner.md"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// T7.21.2 — .. traversal is rejected: the name would escape the state
// directory.
func TestSafeRelPathRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"../../etc/passwd", "../source.go"} {
		if _, err := SafeRelPath(base, name); err == nil {
			t.Fatalf("traversal name %q accepted", name)
		}
	}
}

// T7.21.3 — An absolute path is rejected: it is not a managed relative
// name, so it cannot be contained by construction.
func TestSafeRelPathRejectsAbsolute(t *testing.T) {
	base := t.TempDir()
	if _, err := SafeRelPath(base, "/path/outside/project"); err == nil {
		t.Fatalf("absolute path accepted")
	}
}

// T7.21.4 — Symlink escape prevented per the documented policy: the path is
// resolved (symlinks followed) and containment is verified AFTER
// resolution — a component of the name that is a symlink pointing outside
// the state directory is rejected.
func TestSafeRelPathPreventsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	// A component of the relative name that resolves outside the base.
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := SafeRelPath(base, "link/target.md"); err == nil {
		t.Fatalf("symlink escape accepted")
	}
}

// T7.21.5 — An invalid filename writes nothing: SafeRelPath rejects it, so
// the materializer never opens a path for it. (The integration half
// pinning "nothing written" lives in the service tests; this unit pins the
// rejection of the invalid names themselves.)
func TestSafeRelPathRejectsInvalidNames(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"", ".", "a\x00b.md"} {
		if _, err := SafeRelPath(base, name); err == nil {
			t.Fatalf("invalid name %q accepted", name)
		}
	}
}
