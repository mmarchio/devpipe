package gorefactor

import (
	"os"
	"path/filepath"
	"testing"
)

// T18.85 / T18.86 — refactoring writes cannot leave the module: neither by
// ../ traversal nor through a symlink inside the module pointing outside.
func TestT1885ConfinedToModule(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(root, "..", "other-project", "x.go"),
		filepath.Join(root, "pkg", "..", "..", "other-project", "x.go"),
		filepath.Join(root, "escape", "x.go"),
		filepath.Join(root, "escape"),
		outside,
	} {
		if err := confined(root, p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
	for _, p := range []string{filepath.Join(root, "pkg", "shared", "x.go"), filepath.Join(root, "a.go")} {
		if err := confined(root, p); err != nil {
			t.Errorf("%s rejected: %v", p, err)
		}
	}
	if _, err := writeIfChanged(root, filepath.Join(root, "escape", "evil.go"), []byte("package x\n")); err == nil {
		t.Fatal("write through a symlink escaped the module")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.go")); err == nil {
		t.Fatal("file created outside the module")
	}
}
