package gitcheckpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
)

func p18Repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func p18Message() string {
	ex, unit := uuid.New(), uuid.New()
	return execution.CheckpointMessage(execution.RepoCheckpointUnit, execution.CheckpointKey(ex, execution.RepoCheckpointUnit, unit, 1), ex, "unit")
}

// T18.60 — a hung git (and the process it spawned) is killed at the
// timeout; the call returns ErrTimeout instead of blocking.
func TestT1860HungGitKilled(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\nsleep 60 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	g := &Git{Binary: fake, Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := g.run(context.Background(), dir, "status")
	if !errors.Is(err, ErrTimeout) || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid <= 0 {
		t.Fatal("child pid not recorded")
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("the hung git's child process survived the timeout")
	}
}

// T18.57 — index.lock held by another git process: bounded retry; if
// released in time the checkpoint is created, otherwise ErrLocked with
// nothing committed.
func TestT1857IndexLock(t *testing.T) {
	ctx := context.Background()
	dir := p18Repo(t)
	lock := filepath.Join(dir, ".git", "index.lock")
	head := git(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	g := New()
	start := time.Now()
	if _, err := g.Create(ctx, dir, p18Message()); !errors.Is(err, ErrLocked) {
		t.Fatalf("held lock: %v", err)
	}
	if time.Since(start) > 10*time.Second || git(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("retry unbounded or something was committed")
	}
	// Released while the retry is waiting: the checkpoint is created.
	go func() { time.Sleep(120 * time.Millisecond); _ = os.Remove(lock) }()
	c, err := g.Create(ctx, dir, p18Message())
	if err != nil || c.Commit == head {
		t.Fatalf("after release: %v", err)
	}
}

// T18.61 — hooks are not run by default; when enabled, a rejecting hook
// fails the checkpoint and a hook that rewrites the commit is detected.
func TestT1861Hooks(t *testing.T) {
	ctx := context.Background()
	dir := p18Repo(t)
	hook := func(name, body string) {
		p := filepath.Join(dir, ".git", "hooks", name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hook("pre-commit", "echo rejected by policy >&2; exit 1")
	if _, err := New().Create(ctx, dir, p18Message()); err != nil {
		t.Fatalf("hooks off: %v", err)
	}
	g := New()
	g.RunHooks = true
	head := git(t, dir, "rev-parse", "HEAD")
	if _, err := g.Create(ctx, dir, p18Message()); err == nil || !strings.Contains(err.Error(), "rejected by policy") {
		t.Fatalf("rejecting hook: %v", err)
	}
	if git(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("rejected checkpoint committed")
	}
	_ = os.Remove(filepath.Join(dir, ".git", "hooks", "pre-commit"))
	// Amend once (the amend itself triggers post-commit again).
	hook("post-commit", "[ -n \"$P18_AMENDED\" ] && exit 0\nP18_AMENDED=1 git commit -q --amend --allow-empty --no-verify -m 'rewritten by hook'")
	if _, err := g.Create(ctx, dir, p18Message()); !errors.Is(err, ErrAltered) {
		t.Fatalf("rewriting hook: %v", err)
	}
}

// T18.59 — every commit carrying a checkpoint's trailer is reported.
func TestT1859FindAllByKey(t *testing.T) {
	ctx := context.Background()
	dir := p18Repo(t)
	msg := p18Message()
	g := New()
	if _, err := g.Create(ctx, dir, msg); err != nil {
		t.Fatal(err)
	}
	key := strings.TrimPrefix(trailerLine(msg), execution.TrailerCheckpoint+": ")
	if all, err := g.FindAllByKey(ctx, dir, key); err != nil || len(all) != 1 {
		t.Fatalf("one: %d %v", len(all), err)
	}
	git(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	all, err := g.FindAllByKey(ctx, dir, key)
	if err != nil || len(all) != 2 || all[0].Commit != git(t, dir, "rev-parse", "HEAD") {
		t.Fatalf("two: %d %v", len(all), err)
	}
}
