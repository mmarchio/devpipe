package gitcheckpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
)

func flowRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

// The git-gated lifecycle: task/<key> from main with the test commit,
// sandbox/<key> from it, a discarded sandbox back at the test commit, and
// the validated sandbox fast-forwarded into main with both branches gone.
func TestGitFlowLifecycle(t *testing.T) {
	ctx := context.Background()
	dir, base := flowRepo(t)
	g := New()
	g.Exclude = []string{".hermes"}
	ex := uuid.New()

	if b, err := g.CurrentBranch(ctx, dir); err != nil || b != "main" {
		t.Fatalf("branch %q %v", b, err)
	}
	if err := g.StartBranch(ctx, dir, "task/T1", "main"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a_test.go"), []byte("package a // spec\n"), 0o644)
	key := execution.CheckpointKey(ex, execution.RepoCheckpointTests, uuid.New(), 1)
	tests, err := g.Create(ctx, dir, execution.CommitMessage("qa: generate specification test suite for T1", key, ex))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := g.FromExecution(ctx, dir, tests.Commit, ex.String()); err != nil || !ok {
		t.Fatalf("test commit not attributed to the execution: %v %v", ok, err)
	}
	if ok, _ := g.FromExecution(ctx, dir, base, ex.String()); ok {
		t.Fatal("the base commit was attributed to the execution")
	}
	if err := g.StartBranch(ctx, dir, "sandbox/T1", tests.Commit); err != nil {
		t.Fatal(err)
	}

	// The coder edits a source file, tampers with the test, adds an
	// untracked file and touches the reserved path.
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a // impl\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a_test.go"), []byte("package a // weakened\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "new.go"), []byte("package a\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".hermes"), 0o755)
	os.WriteFile(filepath.Join(dir, ".hermes", "notes"), []byte("keep"), 0o644)
	changed, err := g.ChangedPaths(ctx, dir, tests.Commit)
	sort.Strings(changed)
	if err != nil || strings.Join(changed, ",") != ".hermes/notes,a.go,a_test.go,new.go" {
		t.Fatalf("changed %v %v", changed, err)
	}
	if err := g.Discard(ctx, dir, tests.Commit); err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "status", "--porcelain"); got != "?? .hermes/" {
		t.Fatalf("sandbox not discarded: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a_test.go")); string(b) != "package a // spec\n" {
		t.Fatalf("test file %q", b)
	}

	// A validated attempt is committed and promoted.
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a // impl\n"), 0o644)
	done, err := g.Create(ctx, dir, execution.CommitMessage("feat: completed task T1 successfully",
		execution.CheckpointKey(ex, execution.RepoCheckpointUnit, uuid.New(), 1), ex))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := g.Promote(ctx, dir, "main", done.Commit, "task/T1", "sandbox/T1"); err != nil {
			t.Fatalf("promote %d: %v", i, err)
		}
	}
	if b, _ := g.CurrentBranch(ctx, dir); b != "main" {
		t.Fatalf("branch after promote %q", b)
	}
	if head := git(t, dir, "rev-parse", "main"); head != done.Commit {
		t.Fatalf("main %s, want %s", head, done.Commit)
	}
	if got := git(t, dir, "branch", "--list"); strings.TrimSpace(got) != "* main" {
		t.Fatalf("branches left: %q", got)
	}
	if c, _ := g.Resolve(ctx, dir, "refs/heads/task/T1"); c != "" {
		t.Fatal("task branch still resolves")
	}
}

// main moved after the task branched: the promotion refuses instead of
// merging.
func TestGitFlowPromoteRefusesNonFastForward(t *testing.T) {
	ctx := context.Background()
	dir, _ := flowRepo(t)
	g := New()
	if err := g.StartBranch(ctx, dir, "sandbox/T1", "main"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package a\n"), 0o644)
	work, err := g.Create(ctx, dir, "work")
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(dir, "c.go"), []byte("package a\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "elsewhere")
	if err := g.Promote(ctx, dir, "main", work.Commit, "sandbox/T1"); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("promote over a moved main: %v", err)
	}
}
