package testintegrity

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func compute(t *testing.T, root string, exclude ...string) Fingerprint {
	t.Helper()
	f, err := Compute(root, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The digest covers exactly the test files, in sorted order, and changes
// with any edit, addition, deletion or rename of one.
func TestComputeDetectsEveryTestChange(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/a_test.go", "package a")
	write(t, root, "b/b.test.js", "test()")
	write(t, root, "b/c.spec.js", "spec()")
	write(t, root, "a/a.go", "package a")
	base := compute(t, root)
	if got := base.Files; len(got) != 3 || got[0] != "a/a_test.go" || got[1] != "b/b.test.js" || got[2] != "b/c.spec.js" {
		t.Fatalf("files %v", got)
	}

	// A non-test file does not change the digest.
	write(t, root, "a/a.go", "package a // changed")
	if compute(t, root).MD5 != base.MD5 {
		t.Fatal("a non-test edit changed the digest")
	}

	cases := []struct {
		name string
		do   func()
		undo func()
	}{
		{"edit", func() { write(t, root, "a/a_test.go", "package a // weakened") }, func() { write(t, root, "a/a_test.go", "package a") }},
		{"add", func() { write(t, root, "a/extra_test.go", "package a") }, func() { os.Remove(filepath.Join(root, "a/extra_test.go")) }},
		{"delete", func() { os.Remove(filepath.Join(root, "b/b.test.js")) }, func() { write(t, root, "b/b.test.js", "test()") }},
		{"rename", func() {
			os.Rename(filepath.Join(root, "a/a_test.go"), filepath.Join(root, "a/z_test.go"))
		}, func() { os.Rename(filepath.Join(root, "a/z_test.go"), filepath.Join(root, "a/a_test.go")) }},
	}
	for _, c := range cases {
		c.do()
		if compute(t, root).MD5 == base.MD5 {
			t.Errorf("%s: digest unchanged", c.name)
		}
		c.undo()
		if compute(t, root).MD5 != base.MD5 {
			t.Fatalf("%s: digest not restored", c.name)
		}
	}
}

// Content cannot move between files without changing the digest (each
// file's length is hashed with it).
func TestComputeSeparatesFiles(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, a, "x_test.go", "ab")
	write(t, a, "y_test.go", "c")
	write(t, b, "x_test.go", "a")
	write(t, b, "y_test.go", "bc")
	if compute(t, a).MD5 == compute(t, b).MD5 {
		t.Fatal("content moved between files kept the digest")
	}
}

// Repository metadata, dependency trees and excluded paths are skipped.
func TestComputeSkipsDirs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a_test.go", "package a")
	base := compute(t, root, ".hermes")
	for _, rel := range []string{".git/x_test.go", "vendor/v/v_test.go", "node_modules/m/m.test.js", ".hermes/h_test.go"} {
		write(t, root, rel, "ignored")
	}
	if got := compute(t, root, ".hermes"); got.MD5 != base.MD5 || len(got.Files) != 1 {
		t.Fatalf("skipped trees were hashed: %v", got.Files)
	}
}

func TestIsTest(t *testing.T) {
	for p, want := range map[string]bool{"a_test.go": true, "x/a.test.js": true, "a.spec.js": true, "a.go": false, "test.go": false, "a_test.go.bak": false} {
		if IsTest(p) != want {
			t.Errorf("IsTest(%q) = %v", p, !want)
		}
	}
}
