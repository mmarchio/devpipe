package gitcheckpoint

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"overseer/internal/execution"
	"overseer/internal/resilience"
)

// Branch operations of the git-gated TDD workflow. main holds only
// validated work; task/<key> starts from main and receives the QA role's
// test commit; sandbox/<key> starts from that commit and is where the
// coder works. A sandbox is discarded by a hard reset to the test commit,
// and a validated sandbox is fast-forwarded into main.

// ErrNotFastForward: main moved since the task branched from it, so the
// validated work cannot be fast-forwarded into it.
var ErrNotFastForward = errors.New("gitcheckpoint: main cannot be fast-forwarded to the task's commit")

// CurrentBranch returns the checked-out branch ("" when HEAD is detached).
func (g *Git) CurrentBranch(ctx context.Context, dir string) (string, error) {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return "", err
	}
	out, err := g.run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if errors.Is(err, ErrTimeout) {
			return "", err
		}
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// Resolve returns the commit rev names ("" when it names none).
func (g *Git) Resolve(ctx context.Context, dir, rev string) (string, error) {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return "", err
	}
	out, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		if errors.Is(err, ErrTimeout) {
			return "", err
		}
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// StartBranch checks out branch, created (or moved) at commit. Changes in
// the work tree are carried over (git refuses only a conflicting switch).
func (g *Git) StartBranch(ctx context.Context, dir, branch, at string) error {
	return g.locked(ctx, dir, func(ctx context.Context) error {
		_, err := g.run(ctx, dir, "checkout", "-q", "-B", branch, at)
		return err
	})
}

// Discard resets the checked-out branch and the work tree to commit and
// removes every untracked file except ignored ones and the reserved
// paths: the sandbox is exactly the commit again.
func (g *Git) Discard(ctx context.Context, dir, commit string) error {
	return g.locked(ctx, dir, func(ctx context.Context) error {
		if _, err := g.run(ctx, dir, "reset", "-q", "--hard", commit); err != nil {
			return err
		}
		clean := []string{"clean", "-q", "-f", "-d"}
		for _, p := range g.Exclude {
			clean = append(clean, "-e", "/"+strings.TrimSuffix(p, "/"))
		}
		_, err := g.run(ctx, dir, clean...)
		return err
	})
}

// Promote fast-forwards main to commit, checks main out and deletes the
// task's branches. It is idempotent: main already at commit is not an
// error, nor is a branch already deleted.
func (g *Git) Promote(ctx context.Context, dir, main, commit string, drop ...string) error {
	return g.locked(ctx, dir, func(ctx context.Context) error {
		if _, err := g.run(ctx, dir, "checkout", "-q", main); err != nil {
			return err
		}
		if _, err := g.run(ctx, dir, "merge", "-q", "--ff-only", commit); err != nil {
			if errors.Is(err, ErrTimeout) || errors.Is(err, ErrLocked) {
				return err
			}
			return fmt.Errorf("%w: %v", ErrNotFastForward, err)
		}
		for _, b := range drop {
			if b == main {
				continue
			}
			if c, err := g.Resolve(ctx, dir, "refs/heads/"+b); err != nil {
				return err
			} else if c == "" {
				continue
			}
			if _, err := g.run(ctx, dir, "branch", "-q", "-D", b); err != nil {
				return err
			}
		}
		return nil
	})
}

// FromExecution reports whether the commit rev names carries the
// Overseer-Execution trailer of executionID (it is one of that
// execution's own commits).
func (g *Git) FromExecution(ctx context.Context, dir, rev, executionID string) (bool, error) {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return false, err
	}
	body, err := g.run(ctx, dir, "log", "-1", "--format=%B", rev)
	if err != nil {
		if errors.Is(err, ErrTimeout) {
			return false, err
		}
		return false, nil
	}
	needle := execution.TrailerExecution + ": " + executionID
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == needle {
			return true, nil
		}
	}
	return false, nil
}

// ChangedPaths lists the paths whose work-tree state differs from commit:
// tracked changes (including deletions) and untracked, non-ignored files.
func (g *Git) ChangedPaths(ctx context.Context, dir, commit string) ([]string, error) {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return nil, err
	}
	tracked, err := g.run(ctx, dir, "diff", "--name-only", "--no-renames", commit, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := g.run(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(tracked+"\n"+untracked, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// locked runs a mutating operation under the bounded git_lock retry.
func (g *Git) locked(ctx context.Context, dir string, op func(context.Context) error) error {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return err
	}
	retry := g.Retry
	if retry == nil {
		retry = resilience.MustDefault()
	}
	_, err := retry.Do(ctx, resilience.PolicyGitLock, func(ctx context.Context, _ int) error { return op(ctx) })
	return err
}

// Reserved returns the worker's reserved paths: never committed, never
// cleaned, never part of a test fingerprint.
func (g *Git) Reserved() []string { return append([]string(nil), g.Exclude...) }
