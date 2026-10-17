package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/wbs"
)

func (r *p16Rig) runningOpPackage(t *testing.T) string {
	t.Helper()
	var pkg *string
	_ = r.h.DBScan(context.Background(), `SELECT o.package FROM hardening_operations o JOIN hardening_groups g ON g.id = o.group_id
		JOIN hardening_passes p ON p.id = g.pass_id WHERE p.execution_id = '`+r.execID.String()+`' AND o.status = 'RUNNING' LIMIT 1`, &pkg)
	if pkg == nil {
		return ""
	}
	return *pkg
}

const p16BrokenTest = "package role\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { t.Fatal(\"role caller regression\") }\n"

// TestP16174HardeningFailure — WBS 16.174 / 16.131 / 16.132: a deterministic
// test failure after reference migration keeps the group incomplete, goes
// through FIX with the exact evidence, then validation; persistent failure
// never completes the project.
func TestP16174HardeningFailure(t *testing.T) {
	t.Run("fix then validation", func(t *testing.T) {
		r := p16HardeningRig(t, p16DupModule(), nil)
		r.runToHardening(t)
		r.queue("complete") // HARDENING_FIX reply
		broken := false
		var fixPrompt string
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if stage == "hardening:TEST_CALLERS" && r.runningOpPackage(t) == "ex/role" && !broken {
				broken = true
				_ = os.WriteFile(filepath.Join(r.dir, "role/broken_test.go"), []byte(p16BrokenTest), 0o644)
			}
			if a == execution.ActionHardeningFix && stage == "dispatch" {
				var status string
				_ = r.h.DBScan(ctx, `SELECT g.status FROM hardening_groups g JOIN hardening_passes p ON p.id = g.pass_id WHERE p.execution_id = '`+r.execID.String()+`'`, &status)
				if status == "CONSOLIDATED" {
					t.Error("group consolidated while failing")
				}
				_ = os.Remove(filepath.Join(r.dir, "role/broken_test.go")) // the worker repairs it
			}
			return nil
		})
		r.runUntil(t, execution.StepCompleted, 500)
		r.eng().SetStepHook(nil)
		for _, m := range r.g.Messages() {
			if strings.Contains(m.Content, "role caller regression") {
				fixPrompt = m.Content
			}
		}
		if fixPrompt == "" {
			t.Fatal("HARDENING_FIX prompt lacks the exact failure evidence")
		}
		if n := r.count(t, "SELECT count(*) FROM fix_attempts WHERE target_kind = 'HARDENING' AND dispatch_id IS NOT NULL AND execution_id = '"+r.execID.String()+"'"); n != 1 {
			t.Fatalf("fix attempts = %d", n)
		}
		if !strings.Contains(r.statesSeq(t), "FINAL_HARDENING→HARDENING_FIX→FINAL_HARDENING") {
			t.Fatalf("history %s", r.statesSeq(t))
		}
	})
	t.Run("persistent failure is bounded", func(t *testing.T) {
		r := p16HardeningRig(t, p16DupModule(), nil)
		r.runToHardening(t)
		r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
			if stage == "hardening:TEST_CALLERS" && r.runningOpPackage(t) == "ex/role" {
				_ = os.WriteFile(filepath.Join(r.dir, "role/broken_test.go"), []byte(p16BrokenTest), 0o644)
			}
			return nil
		})
		defer r.eng().SetStepHook(nil)
		for i := 0; i < 4; i++ {
			r.queue("complete")
		}
		r.runUntil(t, execution.StepRecoveryRequired, 500)
		if !strings.Contains(r.recoveryReason(t), "hardening fix attempts exhausted") || r.projectStatus(t) == "COMPLETED" {
			t.Fatalf("reason %q project %s", r.recoveryReason(t), r.projectStatus(t))
		}
	})
}

// p16HardeningRestartRig is a restartable hardening rig.
func p16HardeningRestartRig(t *testing.T) (*p16Rig, func() *Harness) {
	t.Helper()
	base, boot := p15RestartRig(t, []p15Phase{{1, "Foundation", "PENDING", 1}}, nil)
	r := &p16Rig{p15Rig: base}
	p16Configure(t, r, p16Options{noBootstrap: true, engine: func(s *execution.EngineSettings, h *hardening.Settings) {
		*h = hardening.DefaultSettings()
		h.GroupCheckpointPolicy = string(execution.CheckpointRequired)
		h.MinStatements = 2
		s.FinalValidation = []wbs.ValidationSpec{{Name: "go test", Program: "go", Args: []string{"test", "-count=1", "./..."}}}
	}})
	for p, c := range p16DupModule() {
		full := filepath.Join(r.dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p15Git(t, r.dir, "add", "-A")
	p15Git(t, r.dir, "commit", "-q", "-m", "module")
	r.boot(t)
	return r, func() *Harness {
		r.h.Close()
		h := boot(func(c *config.Config) { c.Engine.RecoveryDisabled = false })
		r.h = h
		return h
	}
}

// TestP16175HardeningRestart — WBS 16.175 / 16.139–16.143: the overseer dies
// at each hardening crash point and restarts; the same pass and group
// resume, completed edits are recognized from the source, no duplicate
// shared function, no lost test, no duplicate checkpoint.
func TestP16175HardeningRestart(t *testing.T) {
	for _, c := range []struct {
		name  string
		stage string
		pkg   string
		op    string
	}{
		{"during copy", "hardening-done:COPY_TO_SHARED", "", ""},
		{"after migrating half the callers", "hardening-done:UPDATE_REFERENCES", "ex/project", ""},
		{"during test consolidation", "hardening-done:CONSOLIDATE_TESTS", "", ""},
		{"during removal", "hardening-done:REMOVE_DUPLICATES", "", ""},
		{"after the group checkpoint commit", "checkpoint-committed", "", "CHECKPOINT"},
		{"after the baseline checkpoint commit", "checkpoint-committed", "", "BASELINE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, restart := p16HardeningRestartRig(t)
			r.runToHardening(t)
			crashed := false
			r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
				if crashed || stage != c.stage || a != execution.ActionFinalHardening {
					return nil
				}
				if c.pkg != "" && r.runningOpPackage(t) != c.pkg {
					return nil
				}
				if c.op == "CHECKPOINT" && r.runningOpKind(t) != "CHECKPOINT" {
					return nil
				}
				if c.op == "BASELINE" && r.count(t, "SELECT count(*) FROM hardening_passes WHERE execution_id = '"+r.execID.String()+"'") != 0 {
					return nil
				}
				crashed = true
				return errP16Crash
			})
			if _, err := r.eng().Run(context.Background(), r.execID, 400); !errors.Is(err, errP16Crash) {
				t.Fatalf("no crash: %v", err)
			}
			var passBefore string
			_ = r.h.DBScan(context.Background(), "SELECT id::text FROM hardening_passes WHERE execution_id = '"+r.execID.String()+"'", &passBefore)
			restart()
			r.runUntil(t, execution.StepCompleted, 400)
			var passAfter string
			_ = r.h.DBScan(context.Background(), "SELECT id::text FROM hardening_passes WHERE execution_id = '"+r.execID.String()+"'", &passAfter)
			if (passBefore != passAfter && c.op != "BASELINE") || r.count(t, "SELECT count(*) FROM hardening_passes WHERE execution_id = '"+r.execID.String()+"'") != 1 {
				t.Fatal("hardening pass not reused")
			}
			if n := r.count(t, "SELECT count(*) FROM hardening_groups g JOIN hardening_passes p ON p.id = g.pass_id WHERE p.execution_id = '"+r.execID.String()+"' AND g.status = 'CONSOLIDATED'"); n != 1 {
				t.Fatalf("consolidated groups = %d", n)
			}
			if src := r.read(t, "stringutil/normalize_name.go"); strings.Count(src, "func NormalizeName") != 1 {
				t.Fatalf("shared function duplicated:\n%s", src)
			}
			if n := strings.Count(r.read(t, "stringutil/normalize_name_shared_test.go"), "{name: \"case-"); n != 9 {
				t.Fatalf("lost tests: %d rows", n)
			}
			if !strings.Contains(r.read(t, "project/project_test.go"), "TestProjectCreationNormalizesName") {
				t.Fatal("caller test lost")
			}
			if out := p15Git(t, r.dir, "log", "--format=%s"); strings.Count(out, "hardening_group checkpoint") != 1 {
				t.Fatalf("group checkpoint commits:\n%s", out)
			}
		})
	}
}

// TestP16144ExternalModification — WBS 16.144: an unexpected repository
// change during hardening requires recovery instead of being overwritten.
func TestP16144ExternalModification(t *testing.T) {
	r := p16HardeningRig(t, p16DupModule(), nil)
	r.runToHardening(t)
	sharedTested, done := false, false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if stage == "hardening-done:TEST_SHARED" {
			sharedTested = true
		}
		// Between two hardening steps, someone edits the repository.
		if stage == "intent" && a == execution.ActionFinalHardening && sharedTested && !done {
			done = true
			_ = os.WriteFile(filepath.Join(r.dir, "NOTES.md"), []byte("edited by a human\n"), 0o644)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	r.runUntil(t, execution.StepRecoveryRequired, 400)
	if !strings.Contains(r.recoveryReason(t), "changed outside the hardening pass") || r.read(t, "NOTES.md") != "edited by a human\n" {
		t.Fatalf("reason %q", r.recoveryReason(t))
	}
}

func (r *p16Rig) runningOpKind(t *testing.T) string {
	t.Helper()
	var k *string
	_ = r.h.DBScan(context.Background(), `SELECT o.kind FROM hardening_operations o JOIN hardening_groups g ON g.id = o.group_id
		JOIN hardening_passes p ON p.id = g.pass_id WHERE p.execution_id = '`+r.execID.String()+`' AND o.status = 'RUNNING' LIMIT 1`, &k)
	if k == nil {
		return ""
	}
	return *k
}
