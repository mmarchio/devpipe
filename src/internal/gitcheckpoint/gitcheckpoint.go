// Package gitcheckpoint creates and reconciles overseer-owned git
// checkpoints (WBS 16.54, 16.80, 16.143). It is the only package allowed
// to write to a project's repository history, and only through
// `git add -A` + `git commit` — never checkout, rebase, stash or push,
// and never a reset that moves HEAD or touches the working tree (the only
// reset is `git reset -q -- <reserved paths>`, which unstages). Checkpoints are identified by the Overseer-Checkpoint trailer, so
// a crash after commit is reconciled instead of committing twice.
package gitcheckpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/resilience"
)

// Commit is a created or found checkpoint commit.
type Commit struct {
	Commit string
	// Parent is nil for a root commit.
	Parent *string
	Tree   string
}

// Git writes checkpoints through the git binary.
type Git struct {
	// Binary is the git executable (default "git").
	Binary string
	// AuthorName / AuthorEmail identify overseer commits.
	AuthorName  string
	AuthorEmail string
	// Exclude are project-relative paths the worker reserves for itself
	// (Hermes's own role / skill definitions): never committed.
	Exclude []string
	// Timeout bounds every git invocation (WBS 18.60; 0 = DefaultTimeout).
	Timeout time.Duration
	// RunHooks runs the repository's commit hooks (WBS 18.61). Off by
	// default: hooks are repository-controlled code.
	RunHooks bool
	// Retry runs the git_lock policy for index.lock conflicts (nil = the
	// default registry).
	Retry *resilience.Registry
}

// DefaultTimeout bounds a git invocation when none is configured.
const DefaultTimeout = 2 * time.Minute

var (
	// ErrLocked: another git process holds the repository index lock
	// (WBS 18.57). Nothing was committed; the checkpoint stays pending.
	ErrLocked = errors.New("gitcheckpoint: repository index is locked by another git process")
	// ErrTimeout: a git invocation exceeded its time bound and was killed
	// (WBS 18.60). Whether a commit landed is reconciled by trailer.
	ErrTimeout = errors.New("gitcheckpoint: git command timed out")
	// ErrAltered: the commit at HEAD after the checkpoint is not the
	// checkpoint commit (a hook rewrote or replaced it, WBS 18.61).
	ErrAltered = errors.New("gitcheckpoint: the checkpoint commit was altered by a hook")
)

// New returns a checkpointer with the overseer identity.
func New() *Git {
	return &Git{Binary: "git", AuthorName: "overseer", AuthorEmail: "overseer@localhost"}
}

// ErrNotRepository reports a working directory outside a git work tree.
var ErrNotRepository = errors.New("gitcheckpoint: not a git repository")

func (g *Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	bin := g.Binary
	if bin == "" {
		bin = "git"
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := []string{"-C", dir, "--no-pager", "-c", "commit.gpgsign=false"}
	if !g.RunHooks {
		base = append(base, "-c", "core.hooksPath=/dev/null")
	}
	cmd := exec.CommandContext(ctx, bin, append(base, args...)...)
	// Own process group: a timeout kills git and every hook it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
		"GIT_AUTHOR_NAME="+g.AuthorName, "GIT_AUTHOR_EMAIL="+g.AuthorEmail,
		"GIT_COMMITTER_NAME="+g.AuthorName, "GIT_COMMITTER_EMAIL="+g.AuthorEmail)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return stdout.String(), fmt.Errorf("%w after %s: git %s", ErrTimeout, timeout, strings.Join(args, " "))
		case strings.Contains(msg, "index.lock"):
			return stdout.String(), resilience.Mark(fmt.Errorf("%w: %s", ErrLocked, msg), resilience.Transient)
		}
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}

func (g *Git) ensureRepo(ctx context.Context, dir string) error {
	out, err := g.run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		return fmt.Errorf("%w: %s", ErrNotRepository, dir)
	}
	return nil
}

// Create stages every change and commits it with message (an empty commit
// when nothing changed, so every checkpoint maps to exactly one commit).
func (g *Git) Create(ctx context.Context, dir, message string) (*Commit, error) {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return nil, err
	}
	retry := g.Retry
	if retry == nil {
		retry = resilience.MustDefault()
	}
	// An index.lock held by another git process is retried under the
	// bounded git_lock policy; afterwards ErrLocked is returned with
	// nothing committed (WBS 18.57).
	var c *Commit
	_, err := retry.Do(ctx, resilience.PolicyGitLock, func(ctx context.Context, _ int) error {
		var err error
		c, err = g.create(ctx, dir, message)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (g *Git) create(ctx context.Context, dir, message string) (*Commit, error) {
	// Fault point (WBS 18.141): the index lock is held by another git
	// process (the bounded git_lock retry then applies).
	if err := faults.Hit(ctx, faults.GitLock); err != nil {
		if errors.Is(err, faults.ErrCrash) {
			return nil, err
		}
		return nil, resilience.Mark(fmt.Errorf("%w: %v", ErrLocked, err), resilience.Transient)
	}
	if _, err := g.run(ctx, dir, append([]string{"add", "-A"}, Pathspec(g.Exclude)...)...); err != nil {
		return nil, err
	}
	if len(g.Exclude) > 0 {
		// Anything already staged under a reserved path (e.g. by the
		// worker) is unstaged: it never enters an overseer checkpoint.
		if _, err := g.run(ctx, dir, append([]string{"reset", "-q", "--"}, g.Exclude...)...); err != nil {
			return nil, err
		}
	}
	if err := faults.Hit(ctx, faults.BeforeGitCommit); err != nil {
		return nil, fmt.Errorf("git commit: %w", err)
	}
	commit := []string{"commit", "-q", "--allow-empty", "-m", message}
	if !g.RunHooks {
		commit = append(commit, "--no-verify")
	}
	if _, err := g.run(ctx, dir, commit...); err != nil {
		return nil, err
	}
	if err := faults.Hit(ctx, faults.AfterGitCommit); err != nil {
		return nil, fmt.Errorf("after git commit: %w", err)
	}
	// HEAD must be the checkpoint commit itself: a hook could have
	// amended or replaced it (WBS 18.61).
	if trailer := trailerLine(message); trailer != "" {
		body, err := g.run(ctx, dir, "log", "-1", "--format=%B", "HEAD")
		if err != nil {
			return nil, err
		}
		found := false
		for _, line := range strings.Split(body, "\n") {
			found = found || strings.TrimSpace(line) == trailer
		}
		if !found {
			return nil, fmt.Errorf("%w: HEAD does not carry %q", ErrAltered, trailer)
		}
	}
	return g.describe(ctx, dir, "HEAD")
}

func (g *Git) describe(ctx context.Context, dir, rev string) (*Commit, error) {
	out, err := g.run(ctx, dir, "log", "-1", "--format=%H%x00%P%x00%T", rev)
	if err != nil {
		return nil, err
	}
	return parseLine(strings.TrimSpace(out))
}

func parseLine(line string) (*Commit, error) {
	parts := strings.Split(line, "\x00")
	if len(parts) != 3 || parts[0] == "" {
		return nil, fmt.Errorf("gitcheckpoint: unexpected log line %q", line)
	}
	c := &Commit{Commit: parts[0], Tree: parts[2]}
	if ps := strings.Fields(parts[1]); len(ps) > 0 {
		p := ps[0]
		c.Parent = &p
	}
	return c, nil
}

// FindByKey returns the newest HEAD-reachable commit carrying the
// checkpoint trailer for key, or nil (WBS 16.80). An unborn HEAD has no
// checkpoints.
func (g *Git) FindByKey(ctx context.Context, dir, key string) (*Commit, error) {
	all, err := g.FindAllByKey(ctx, dir, key)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[0], nil
}

// FindAllByKey returns every HEAD-reachable commit carrying the
// checkpoint trailer for key, newest first (WBS 18.58 / 18.59: more than
// one is an ambiguity, never resolved by guessing).
func (g *Git) FindAllByKey(ctx context.Context, dir, key string) ([]*Commit, error) {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if errors.Is(err, ErrTimeout) {
			return nil, err
		}
		return nil, nil
	}
	needle := execution.TrailerCheckpoint + ": " + key
	out, err := g.run(ctx, dir, "log", "--fixed-strings", "--grep="+needle, "--format=%H%x00%P%x00%T%x00%B%x01", "HEAD")
	if err != nil {
		return nil, err
	}
	var found []*Commit
	for _, rec := range strings.Split(out, "\x01") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x00", 4)
		if len(f) != 4 {
			continue
		}
		for _, line := range strings.Split(f[3], "\n") {
			if strings.TrimSpace(line) == needle {
				c, err := parseLine(strings.Join(f[:3], "\x00"))
				if err != nil {
					return nil, err
				}
				found = append(found, c)
				break
			}
		}
	}
	return found, nil
}

// Head returns the current HEAD commit (nil when unborn).
func (g *Git) Head(ctx context.Context, dir string) (*Commit, error) {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return nil, nil
	}
	return g.describe(ctx, dir, "HEAD")
}

// Pathspec returns the git pathspec selecting the whole work tree except
// the excluded paths ("" args when none are excluded).
func Pathspec(exclude []string) []string {
	if len(exclude) == 0 {
		return nil
	}
	out := []string{"--", "."}
	for _, p := range exclude {
		out = append(out, ":(exclude)"+p)
	}
	return out
}

// trailerLine returns the checkpoint trailer line of a commit message.
func trailerLine(message string) string {
	for _, line := range strings.Split(message, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, execution.TrailerCheckpoint+": ") {
			return l
		}
	}
	return ""
}

// CommitExists reports whether commit is an existing commit object in the
// repository (WBS 18.203). A missing repository is an error.
func (g *Git) CommitExists(ctx context.Context, dir, commit string) (bool, error) {
	if err := g.ensureRepo(ctx, dir); err != nil {
		return false, err
	}
	if _, err := g.run(ctx, dir, "cat-file", "-e", commit+"^{commit}"); err != nil {
		if errors.Is(err, ErrTimeout) {
			return false, err
		}
		return false, nil
	}
	return true, nil
}
