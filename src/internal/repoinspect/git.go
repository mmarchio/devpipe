// Package repoinspect implements execution.RepositoryInspector with the git
// CLI (WBS 15.19/15.21/15.80). It runs read-only commands only — never
// commit, checkout, reset, clean or stash — with GIT_OPTIONAL_LOCKS=0 so
// `git status` does not refresh (write) the index.
package repoinspect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/gitcheckpoint"
)

// Git inspects repositories through the git binary.
type Git struct {
	// Binary is the git executable (default "git").
	Binary string
	// Exclude are worker-reserved project paths: changes there are not
	// part of the working-tree status, fingerprint or change lists.
	Exclude []string
	// Timeout bounds every git invocation (WBS 18.60; 0 =
	// gitcheckpoint.DefaultTimeout).
	Timeout time.Duration
}

// New returns a git inspector.
func New() *Git { return &Git{Binary: "git"} }

var _ execution.RepositoryInspector = (*Git)(nil)

func (g *Git) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin := g.Binary
	if bin == "" {
		bin = "git"
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = gitcheckpoint.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", dir, "--no-pager"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return stdout.Bytes(), fmt.Errorf("%w after %s: git %s", gitcheckpoint.ErrTimeout, timeout, strings.Join(args, " "))
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Inspect captures repository identity for the working directory. A
// directory that is not inside a git work tree yields IsGitRepository
// false and no error.
func (g *Git) Inspect(ctx context.Context, dir string) (execution.RepositorySnapshot, error) {
	snap := execution.RepositorySnapshot{WorkingDirectory: dir}
	if _, err := os.Stat(dir); err != nil {
		return snap, err
	}
	out, err := g.run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		var ee *exec.Error
		if errors.As(err, &ee) || errors.Is(err, gitcheckpoint.ErrTimeout) {
			return snap, err // git unavailable or hung: not "not a repository"
		}
		return snap, nil
	}
	snap.IsGitRepository = true
	if out, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		h := strings.TrimSpace(string(out))
		if h != "" {
			snap.HeadCommit = &h
		}
	}
	if out, err := g.run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		b := strings.TrimSpace(string(out))
		if b != "" {
			snap.Branch = &b
		}
	}
	status, err := g.run(ctx, dir, append([]string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none"},
		gitcheckpoint.Pathspec(g.Exclude)...)...)
	if err != nil {
		return snap, err
	}
	snap.Dirty = len(status) > 0
	h := sha256.New()
	if snap.HeadCommit != nil {
		h.Write([]byte(*snap.HeadCommit))
	}
	h.Write([]byte{0})
	h.Write(status)
	snap.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return snap, nil
}

// ChangedPaths lists the working-tree paths that differ from HEAD
// (`git status --porcelain`, read-only; WBS 17.94 / 17.95). Paths are
// sorted; renames report the destination.
func (g *Git) ChangedPaths(ctx context.Context, dir string) ([]string, error) {
	out, err := g.run(ctx, dir, append([]string{"status", "--porcelain", "--untracked-files=all"}, gitcheckpoint.Pathspec(g.Exclude)...)...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+4:]
		}
		paths = append(paths, strings.Trim(p, `"`))
	}
	sort.Strings(paths)
	return paths, nil
}

var _ execution.RepositoryIdentifier = (*Git)(nil)

// Identify reads the working directory's identity (WBS 18.48 / 18.49):
// read-only git commands only. A missing directory wraps fs.ErrNotExist;
// a directory outside any git work tree has IsGit false.
func (g *Git) Identify(ctx context.Context, dir string) (execution.RepositoryIdentity, error) {
	id := execution.RepositoryIdentity{Toplevel: filepath.Clean(dir)}
	st, err := os.Stat(dir)
	if err != nil {
		return id, err
	}
	if !st.IsDir() {
		return id, fmt.Errorf("%s is not a directory: %w", dir, fs.ErrNotExist)
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		id.Toplevel = real
	}
	out, err := g.run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		var ee *exec.Error
		if errors.As(err, &ee) || errors.Is(err, gitcheckpoint.ErrTimeout) {
			return id, err // git unavailable or hung: not "not a repository"
		}
		return id, nil
	}
	id.IsGit = true
	if out, err := g.run(ctx, dir, "rev-parse", "--show-toplevel"); err == nil {
		top := strings.TrimSpace(string(out))
		if real, err := filepath.EvalSymlinks(top); err == nil {
			top = real
		}
		id.Toplevel = top
	}
	// The oldest root commit (a repository can have several roots).
	if out, err := g.run(ctx, dir, "rev-list", "--max-parents=0", "HEAD"); err == nil {
		roots := strings.Fields(string(out))
		sort.Strings(roots)
		if len(roots) > 0 {
			id.RootCommit = roots[0]
		}
	}
	if out, err := g.run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		id.Branch = strings.TrimSpace(string(out))
	}
	if out, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		id.Head = strings.TrimSpace(string(out))
	}
	return id, nil
}

// Writable proves dir — and, for a work tree, its git directory — can be
// written by creating and removing a probe file (WBS 18.53). The fs_write
// fault point lets tests inject EROFS / EACCES / ENOSPC.
func (g *Git) Writable(ctx context.Context, dir string) error {
	dirs := []string{dir}
	if out, err := g.run(ctx, dir, "rev-parse", "--absolute-git-dir"); err == nil {
		if gd := strings.TrimSpace(string(out)); gd != "" {
			dirs = append(dirs, gd)
		}
	}
	for _, d := range dirs {
		if err := faults.Hit(ctx, faults.FSWrite); err != nil {
			return &fs.PathError{Op: "write probe", Path: d, Err: err}
		}
		f, err := os.CreateTemp(d, ".overseer-write-probe-*")
		if err != nil {
			return err
		}
		name := f.Name()
		_ = f.Close()
		if err := os.Remove(name); err != nil {
			return err
		}
	}
	return nil
}
