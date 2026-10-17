package repoinspect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// treeHash fingerprints every file (path, mode, mtime, content) under dir,
// including .git, so any mutation by the inspector is detected (T15.21).
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, _ := d.Info()
		rel, _ := filepath.Rel(dir, p)
		line := rel + "|" + info.Mode().String() + "|" + info.ModTime().String()
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			s := sha256.Sum256(b)
			line += "|" + hex.EncodeToString(s[:])
		}
		lines = append(lines, line)
		return nil
	})
	sort.Strings(lines)
	s := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(s[:])
}

func TestP1510NonRepo(t *testing.T) {
	dir := t.TempDir()
	snap, err := New().Inspect(context.Background(), dir)
	if err != nil || snap.IsGitRepository || snap.HeadCommit != nil {
		t.Fatalf("%+v %v", snap, err)
	}
}

func TestP1510UnbornAndCommitted(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	snap, err := New().Inspect(context.Background(), dir)
	if err != nil || !snap.IsGitRepository || snap.HeadCommit != nil || snap.Branch == nil || *snap.Branch != "main" || snap.Dirty {
		t.Fatalf("unborn %+v %v", snap, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-q", "-m", "c1")
	head := git(t, dir, "rev-parse", "HEAD")
	snap, _ = New().Inspect(context.Background(), dir)
	if snap.HeadCommit == nil || *snap.HeadCommit != head || snap.Dirty {
		t.Fatalf("clean %+v", snap)
	}
	clean := snap.Fingerprint
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	snap, _ = New().Inspect(context.Background(), dir)
	if !snap.Dirty || snap.Fingerprint == clean {
		t.Fatalf("dirty %+v", snap)
	}
	git(t, dir, "checkout", "-q", "--detach")
	snap, _ = New().Inspect(context.Background(), dir)
	if snap.Branch != nil {
		t.Fatal("detached branch")
	}
}

func TestP1521InspectionDoesNotMutate(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-q", "-m", "c1")
	// Modify a tracked file (stat-dirty index) + untracked + staged changes.
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "u.txt"), []byte("u"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "s.txt"), []byte("s"), 0o644)
	git(t, dir, "add", "s.txt")
	before := treeHash(t, dir)
	headBefore := git(t, dir, "rev-parse", "HEAD")
	for i := 0; i < 3; i++ {
		if _, err := New().Inspect(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
	}
	if after := treeHash(t, dir); after != before {
		t.Fatal("inspection mutated the repository")
	}
	if git(t, dir, "rev-parse", "HEAD") != headBefore {
		t.Fatal("HEAD moved")
	}
	if out := git(t, dir, "stash", "list"); out != "" {
		t.Fatal("stash created")
	}
}
