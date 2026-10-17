package gitcheckpoint

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
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

// P16-42 (16.54 commit / parent / tree, 16.80 found by key, 16.143).
func TestP1642CreateAndFind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New()
	if _, err := g.Create(ctx, dir, "x"); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("non-repo: %v", err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	if h, err := g.Head(ctx, dir); err != nil || h != nil {
		t.Fatal("unborn head")
	}
	ex, unit := uuid.New(), uuid.New()
	key := execution.CheckpointKey(ex, execution.RepoCheckpointUnit, unit, 1)
	if c, err := g.FindByKey(ctx, dir, key); err != nil || c != nil {
		t.Fatalf("unborn find: %v %v", c, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	c1, err := g.Create(ctx, dir, execution.CheckpointMessage(execution.RepoCheckpointUnit, key, ex, "unit 1"))
	if err != nil || c1.Parent != nil || c1.Tree == "" || c1.Commit != git(t, dir, "rev-parse", "HEAD") {
		t.Fatalf("root checkpoint: %+v %v", c1, err)
	}
	if git(t, dir, "status", "--porcelain") != "" {
		t.Fatal("not everything committed")
	}
	if git(t, dir, "log", "-1", "--format=%an") != "overseer" {
		t.Fatal("author")
	}
	key2 := execution.CheckpointKey(ex, execution.RepoCheckpointUnit, uuid.New(), 1)
	c2, err := g.Create(ctx, dir, execution.CheckpointMessage(execution.RepoCheckpointUnit, key2, ex, "unit 2 (no changes)"))
	if err != nil || c2.Parent == nil || *c2.Parent != c1.Commit || c2.Tree != c1.Tree {
		t.Fatalf("empty checkpoint: %+v %v", c2, err)
	}
	found, err := g.FindByKey(ctx, dir, key)
	if err != nil || found == nil || found.Commit != c1.Commit {
		t.Fatalf("find: %+v %v", found, err)
	}
	if f, _ := g.FindByKey(ctx, dir, key[:len(key)-1]+"9"); f != nil {
		t.Fatal("wrong key matched")
	}
	h, _ := g.Head(ctx, dir)
	if h.Commit != c2.Commit {
		t.Fatal("head")
	}
}
