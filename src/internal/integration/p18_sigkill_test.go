package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/hardening"
)

// T18.129 — forced termination of the real binary: the overseer process
// (profile test, OVERSEER_FAULTS) dies with exit 137 right after a
// checkpoint commit lands in git, before PostgreSQL records it. A new
// instance recovers: the checkpoint is adopted by its trailer (no second
// commit), the unit is not re-dispatched, and the project completes.
func TestT18129ForcedTerminationRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the overseer binary")
	}
	// A short driver lease: the killed process never releases its lease;
	// the new instance takes over once it expires (WBS 18.13).
	r, restart := p16RestartRig(t, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) { s.DriverLease = 2 * time.Second }})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.exec(t, "UPDATE project_settings SET auto_continue = true WHERE project_id = '"+r.pid.String()+"'")
	cfg := r.h.App.Config()
	r.h.keepDB = true
	r.h.Close()

	root, _ := filepath.Abs(filepath.Join("..", ".."))
	bin := filepath.Join(t.TempDir(), "overseer")
	build := exec.Command("go", "build", "-o", bin, "./cmd/overseer")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cfg.Profile = config.ProfileTest
	cfg.Faults = "after_git_commit:exit@1"
	cfg.Engine.SchedulerDisabled = false
	cfg.Engine.Interval = 50 * time.Millisecond
	cfg.Server.Port = freePort(t)
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "overseer.yaml")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "OVERSEER_CONFIG_PATH="+cfgPath, "OVERSEER_FAULTS="+cfg.Faults)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 137 {
			t.Fatalf("process ended with %v (want exit 137 at the fault point)\n%s", err, tail(out.String()))
		}
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the fault point was never reached\n%s", tail(out.String()))
	}
	commits := func() int {
		c := exec.Command("git", "log", "--all", "--format=%B")
		c.Dir = r.dir
		b, _ := c.Output()
		return strings.Count(string(b), "Overseer-Checkpoint:")
	}
	landed := commits()
	if landed == 0 {
		t.Fatal("setup: the checkpoint commit did not land before termination")
	}

	restart()
	time.Sleep(2500 * time.Millisecond)
	r.runUntil(t, execution.StepCompleted, 300)
	if n := r.count(t, `SELECT count(*) FROM worker_dispatches WHERE project_id = '`+r.pid.String()+`' AND work_unit_id IS NOT NULL`); n != 1 {
		t.Fatalf("unit dispatched %d times", n)
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE project_id = '"+r.pid.String()+"' AND kind = 'UNIT' AND status = 'CREATED'"); n != 1 {
		t.Fatalf("%d unit checkpoints recorded", n)
	}
	// No checkpoint was committed twice: every trailer is unique.
	c := exec.Command("git", "log", "--all", "--format=%B")
	c.Dir = r.dir
	b, _ := c.Output()
	seen := map[string]int{}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "Overseer-Checkpoint:") {
			seen[strings.TrimSpace(l)]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Fatalf("checkpoint %q committed %d times", k, n)
		}
	}
	if commits() < landed {
		t.Fatal("a landed checkpoint commit disappeared")
	}
}

func tail(s string) string {
	if len(s) > 3000 {
		return s[len(s)-3000:]
	}
	return s
}
