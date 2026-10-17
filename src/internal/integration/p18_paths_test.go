package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/config"
	"overseer/internal/execution"
)

// T18.85 — a working directory written as ../../other-project relative to
// an allowed project path resolves outside the allowed roots and is
// rejected.
func TestT1885TraversalCannotEscapeRoots(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "projects", "a")
	other := filepath.Join(t.TempDir(), "other-project")
	for _, d := range []string{proj, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	roots := []string{filepath.Join(root, "projects")}
	rel, _ := filepath.Rel(proj, other)
	if !strings.Contains(rel, "..") {
		t.Fatalf("setup: %s", rel)
	}
	if _, errs := execution.CheckWorkingDirectory(filepath.Join(proj, rel), roots); len(errs) == 0 || !strings.Contains(errs[0].Message, "outside the allowed roots") {
		t.Fatalf("traversal accepted: %v", errs)
	}
	if _, errs := execution.CheckWorkingDirectory(filepath.Join(proj, "..", "..", "..", filepath.Base(filepath.Dir(other)), "other-project"), roots); len(errs) == 0 {
		t.Fatal("../ escape accepted")
	}
	if _, errs := execution.CheckWorkingDirectory(proj, roots); len(errs) != 0 {
		t.Fatalf("allowed project rejected: %v", errs)
	}
}

// T18.86 — after bootstrap, the project directory is replaced by a
// symlink to a copy outside the allowed roots: the next side effect is
// refused (REPOSITORY_PERMISSION) and nothing is sent.
func TestT1886SymlinkSwapOutsideRootsStops(t *testing.T) {
	r := p16NewRigWith(t, p16Options{config: func(c *config.Config) {
		p := c.WorkerProviders["hermes"]
		p.AllowedRoots = []string{os.TempDir()}
		c.WorkerProviders["hermes"] = p
	}})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	outside, err := os.MkdirTemp("/var/tmp", "p18-outside-")
	if err != nil {
		t.Skipf("no directory outside the allowed root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	p18BreakRepoAt(t, r, execution.ActionDispatch, func() {
		copyDir := filepath.Join(outside, "repo")
		if out, err := exec.Command("cp", "-a", r.dir, copyDir).CombinedOutput(); err != nil {
			t.Fatalf("copy: %v %s", err, out)
		}
		if err := os.RemoveAll(r.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(copyDir, r.dir); err != nil {
			t.Fatal(err)
		}
	}, "REPOSITORY_PERMISSION", 1)
	if !strings.Contains(r.recoveryReason(t), "outside the allowed roots") {
		t.Fatalf("reason %q", r.recoveryReason(t))
	}
}
