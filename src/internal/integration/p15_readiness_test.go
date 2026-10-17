package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/config"
	"overseer/internal/execution"
)

func (r *p15Rig) dbCounts(t *testing.T) string {
	t.Helper()
	w := r.where()
	var parts []string
	for _, q := range []string{
		"SELECT count(*) FROM executions WHERE " + w,
		"SELECT count(*) FROM runtime_state WHERE " + w,
		"SELECT count(*) FROM state_files WHERE " + w,
		"SELECT count(*) FROM state_transition_history WHERE " + w,
		"SELECT count(*) FROM context_snapshots WHERE " + w,
		"SELECT count(*) FROM canonical_prompts WHERE " + w,
		"SELECT count(*) FROM worker_sessions WHERE " + w,
		"SELECT count(*) FROM worker_dispatches WHERE " + w,
	} {
		parts = append(parts, itoa(r.count(t, q)))
	}
	return strings.Join(parts, ",") + "|" + r.projectStatus(t) + "|" + r.boundarySnapshot(t)
}

func codes(rep *execution.ExecutionReadinessReport) map[execution.BootstrapErrorCode][]string {
	out := map[execution.BootstrapErrorCode][]string{}
	for _, e := range rep.Errors {
		out[e.Code] = append(out[e.Code], e.Resource+": "+e.Message)
	}
	return out
}

// TestP15163FailedReadiness — WBS 15.163 / 15.12–15.14 / 15.102: ENGINEER
// disabled + CONTINUE_WORK_UNIT missing + working directory missing →
// all three reported together; nothing is written; project stays READY.
func TestP15163FailedReadiness(t *testing.T) {
	r := p15NewRig(t)
	r.exec(t, "UPDATE roles SET enabled = false WHERE id = '"+r.roles["ENGINEER"].String()+"'")
	r.exec(t, "UPDATE prompt_templates SET enabled = false WHERE key = 'CONTINUE_WORK_UNIT'")
	r.exec(t, "UPDATE project_settings SET working_directory = '/nonexistent/p15/dir' WHERE "+r.where())
	before := r.dbCounts(t)
	res, be := r.bootstrapErr(t)
	rep := res.Readiness
	c := codes(rep)
	if len(c[execution.CodeRoleInvalid]) == 0 || len(c[execution.CodePromptInvalid]) == 0 || len(c[execution.CodeWorkdirInvalid]) == 0 {
		t.Fatalf("missing failures: %v", c)
	}
	if be.Stage != execution.StageValidating {
		t.Fatalf("stage %s", be.Stage)
	}
	v := execution.NewReadinessView(rep)
	if v.Ready || !strings.HasPrefix(v.Headline, "Cannot Start Project") || len(v.Issues) < 3 {
		t.Fatalf("view %+v", v)
	}
	if after := r.dbCounts(t); after != before {
		t.Fatalf("side effects:\n%s\n%s", before, after)
	}
	if r.projectStatus(t) != "READY" {
		t.Fatal("project status changed")
	}
	r.noWorker(t)
	// GET readiness is non-mutating too (WBS 15.96).
	if _, err := r.svc().Readiness(context.Background(), r.pid); err != nil {
		t.Fatal(err)
	}
	if after := r.dbCounts(t); after != before {
		t.Fatal("readiness mutated")
	}
}

// TestP1511EveryProjectStatus — WBS 15.11.
func TestP1511EveryProjectStatus(t *testing.T) {
	r := p15NewRig(t)
	for _, st := range []string{"DRAFT", "PAUSED", "COMPLETED", "CANCELLED", "FAILED", "RUNNING"} {
		r.exec(t, "UPDATE projects SET status = '"+st+"' WHERE id = '"+r.pid.String()+"'")
		_, be := r.bootstrapErr(t)
		if be.Code != execution.CodeProjectNotReady {
			t.Fatalf("%s → %s", st, be.Code)
		}
	}
	r.exec(t, "UPDATE projects SET status = 'READY' WHERE id = '"+r.pid.String()+"'")
	r.bootstrap(t)
}

// TestP15121To123Missing — WBS 15.121 (PLANNER disabled), 15.122 (required
// ARCHITECTURE missing), 15.123 (PLAN_PHASE / PHASE_PLAN template missing).
func TestP15121To123Missing(t *testing.T) {
	t.Run("planner", func(t *testing.T) {
		r := p15NewRig(t)
		r.exec(t, "UPDATE roles SET enabled = false WHERE id = '"+r.roles["PLANNER"].String()+"'")
		res, _ := r.bootstrapErr(t)
		if c := codes(res.Readiness); len(c[execution.CodeRoleInvalid]) == 0 {
			t.Fatalf("%v", c)
		}
	})
	t.Run("architecture", func(t *testing.T) {
		r := p15NewRig(t)
		r.exec(t, "UPDATE state_file_types SET required = true WHERE key = 'ARCHITECTURE' AND project_id IS NULL")
		p712RequireRoleFiles(t, r.h, r.roles["PLANNER"], []string{"ARCHITECTURE"})
		res, _ := r.bootstrapErr(t)
		if c := codes(res.Readiness); len(c[execution.CodeStateFileInvalid]) == 0 {
			t.Fatalf("%v", c)
		}
		if n := r.count(t, "SELECT count(*) FROM executions WHERE "+r.where()); n != 0 {
			t.Fatal("execution created")
		}
	})
	t.Run("template", func(t *testing.T) {
		r := p15NewRig(t)
		r.exec(t, "UPDATE prompt_templates SET enabled = false WHERE key = 'PHASE_PLAN'")
		res, _ := r.bootstrapErr(t)
		if c := codes(res.Readiness); len(c[execution.CodePromptInvalid]) == 0 {
			t.Fatalf("%v", c)
		}
		if r.projectStatus(t) != "READY" {
			t.Fatal("not READY")
		}
	})
}

// TestP1515WorkingDirectoryAndRepository — WBS 15.15–15.21.
func TestP1515WorkingDirectoryAndRepository(t *testing.T) {
	r := p15NewRig(t)
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(file, []byte("x"), 0o644)
	r.exec(t, "UPDATE project_settings SET working_directory = '"+file+"' WHERE "+r.where())
	rep, _ := r.svc().Readiness(ctx, r.pid)
	if c := codes(rep); len(c[execution.CodeWorkdirInvalid]) == 0 {
		t.Fatalf("file accepted: %v", c)
	}
	// Non-git directory with git required.
	plain := t.TempDir()
	r.exec(t, "UPDATE project_settings SET working_directory = '"+plain+"' WHERE "+r.where())
	if _, err := r.svc().UpdateExecutionSettings(ctx, r.pid, execution.Settings{RequireGitRepository: true, DirtyPolicy: execution.DirtyWarn,
		MaxBootstrapControlTransitions: 100, BootstrapLease: execution.DefaultSettings().BootstrapLease}); err != nil {
		t.Fatal(err)
	}
	rep, _ = r.svc().Readiness(ctx, r.pid)
	if c := codes(rep); len(c[execution.CodeRepositoryInvalid]) == 0 {
		t.Fatalf("non-git accepted: %v", c)
	}
	// Dirty policies.
	r.exec(t, "UPDATE project_settings SET working_directory = '"+r.dir+"' WHERE "+r.where())
	_ = os.WriteFile(filepath.Join(r.dir, "dirty.txt"), []byte("d"), 0o644)
	head := p15Git(t, r.dir, "rev-parse", "HEAD")
	for _, pol := range []execution.DirtyPolicy{execution.DirtyAllow, execution.DirtyWarn, execution.DirtyReject} {
		if _, err := r.svc().UpdateExecutionSettings(ctx, r.pid, execution.Settings{RequireGitRepository: true, DirtyPolicy: pol,
			MaxBootstrapControlTransitions: 100, BootstrapLease: execution.DefaultSettings().BootstrapLease}); err != nil {
			t.Fatal(err)
		}
		rep, _ := r.svc().Readiness(ctx, r.pid)
		errs, warns := len(codes(rep)[execution.CodeRepositoryInvalid]), 0
		for _, w := range rep.Warnings {
			if w.Code == execution.CodeRepositoryInvalid {
				warns++
			}
		}
		want := map[execution.DirtyPolicy][2]int{execution.DirtyAllow: {0, 0}, execution.DirtyWarn: {0, 1}, execution.DirtyReject: {1, 0}}[pol]
		if errs != want[0] || warns != want[1] || rep.Ready != (want[0] == 0) {
			t.Fatalf("%s: errs %d warns %d ready %v", pol, errs, warns, rep.Ready)
		}
	}
	// Inspection never mutated the repository (WBS 15.21).
	if p15Git(t, r.dir, "rev-parse", "HEAD") != head || p15Git(t, r.dir, "stash", "list") != "" {
		t.Fatal("repository mutated")
	}
	if !strings.Contains(p15Git(t, r.dir, "status", "--porcelain"), "dirty.txt") {
		t.Fatal("untracked file disappeared")
	}
}

// TestP1517AllowedRoots — WBS 15.17: a symlink escaping the worker
// provider's allowed roots is rejected.
func TestP1517AllowedRoots(t *testing.T) {
	root := t.TempDir()
	r := p15NewRigWith(t, p15DefaultPhases, func(c *config.Config) {
		p := c.WorkerProviders["hermes"]
		p.AllowedRoots = []string{root}
		c.WorkerProviders["hermes"] = p
	})
	ctx := context.Background()
	rep, _ := r.svc().Readiness(ctx, r.pid)
	if c := codes(rep); len(c[execution.CodeWorkdirInvalid]) == 0 {
		t.Fatalf("outside root accepted: %v", c)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(r.dir, link); err != nil {
		t.Fatal(err)
	}
	r.exec(t, "UPDATE project_settings SET working_directory = '"+link+"' WHERE "+r.where())
	rep, _ = r.svc().Readiness(ctx, r.pid)
	if c := codes(rep); len(c[execution.CodeWorkdirInvalid]) == 0 {
		t.Fatalf("symlink escape accepted: %v", c)
	}
	inside := filepath.Join(root, "proj")
	_ = os.Mkdir(inside, 0o755)
	r.exec(t, "UPDATE project_settings SET working_directory = '"+inside+"/../proj' WHERE "+r.where())
	rep, _ = r.svc().Readiness(ctx, r.pid)
	if c := codes(rep); len(c[execution.CodeWorkdirInvalid]) != 0 {
		t.Fatalf("inside root rejected: %v", c)
	}
}
