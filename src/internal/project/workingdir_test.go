package project

import (
	"os"
	"path/filepath"
	"testing"
)

// T3.4.1 — Existing directory accepted (configured, valid, accessible).
func TestValidateWorkingDirectoryExisting(t *testing.T) {
	dir := t.TempDir()
	rep := ValidateWorkingDirectory(dir)
	if !rep.Configured {
		t.Fatal("existing directory reported as not configured")
	}
	if !rep.Valid {
		t.Fatal("existing directory reported as invalid")
	}
	if !rep.Accessible {
		t.Fatal("existing directory reported as not accessible")
	}
}

// T3.4.2 — Missing directory rejected (configured but not valid/accessible).
func TestValidateWorkingDirectoryMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	rep := ValidateWorkingDirectory(missing)
	if !rep.Configured {
		t.Fatal("missing path reported as not configured")
	}
	if rep.Valid {
		t.Fatal("missing path reported as valid")
	}
	if rep.Accessible {
		t.Fatal("missing path reported as accessible")
	}
}

// T3.4.3 — File path rejected (a regular file is not a working directory).
func TestValidateWorkingDirectoryFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := ValidateWorkingDirectory(f)
	if !rep.Configured {
		t.Fatal("file path reported as not configured")
	}
	if rep.Valid {
		t.Fatal("file path reported as valid directory")
	}
	if rep.Accessible {
		t.Fatal("file path reported as accessible")
	}
}

// T3.4.4 — Empty path rejected (nothing configured).
func TestValidateWorkingDirectoryEmpty(t *testing.T) {
	rep := ValidateWorkingDirectory("")
	if rep.Configured || rep.Valid || rep.Accessible {
		t.Fatalf("empty path reported %v", rep)
	}
}

// T3.4.5 — Path retained without unexpected rewriting.
func TestValidateWorkingDirectoryNoRewriting(t *testing.T) {
	cases := []string{
		"/work/demo",
		"  /with/leading/trailing/  ",
		"relative/dir",
		"~/home/user",
		"/UPPER/CASE",
	}
	for _, p := range cases {
		rep := ValidateWorkingDirectory(p)
		if rep.Path != p {
			t.Fatalf("path %q rewritten to %q", p, rep.Path)
		}
	}
}

// T3.4.6 — Validation does not modify process working directory.
func TestValidateWorkingDirectoryNoCWDChange(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	_ = ValidateWorkingDirectory(t.TempDir())
	_ = ValidateWorkingDirectory(filepath.Join(t.TempDir(), "missing"))
	_ = ValidateWorkingDirectory("")
	after, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("CWD changed: %q -> %q", before, after)
	}
}
