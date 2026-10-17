package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/execution"
)

// T18.214 — the worker provider is lost mid-run: the execution waits (no
// failure, no unsafe action), the console, audit and history stay usable;
// when the provider returns the project completes.
func TestT18214WorkerProviderLost(t *testing.T) {
	r := p17NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	px := proxyOf(t, r.g)
	stopped := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionDispatch && s == "intent" && !stopped {
			stopped = true
			px.Stop()
		}
		return nil
	})
	_, _ = r.eng().Run(context.Background(), r.execID, 20)
	r.eng().SetStepHook(nil)
	if s := r.execStatus(t); s != "WAITING" {
		t.Fatalf("execution %s (%s) while the worker is lost", s, r.recoveryReason(t))
	}
	pid := r.pid.String()
	for _, path := range []string{"/api/v1/projects/" + pid, "/api/v1/audit?project=" + pid,
		"/api/v1/projects/" + pid + "/console/executions/" + r.execID.String() + "/timeline", "/api/v1/projects/" + pid + "/overview"} {
		if code, _ := r.b.api(http.MethodGet, path, nil); code != http.StatusOK {
			t.Fatalf("GET %s: %d during the outage", path, code)
		}
	}
	if code, _ := r.b.page("/projects/" + pid); code != http.StatusOK {
		t.Fatalf("console: %d during the outage", code)
	}
	px.Start(t)
	deadline := time.Now().Add(60 * time.Second)
	for r.execStatus(t) != "COMPLETED" {
		if _, err := r.eng().Run(context.Background(), r.execID, 50); err != nil && time.Now().After(deadline) {
			t.Fatalf("after the provider returned: %v", err)
		}
		if s := r.execStatus(t); s == "RECOVERY_REQUIRED" || s == "FAILED" || time.Now().After(deadline) {
			t.Fatalf("execution %s (%s)", s, r.recoveryReason(t))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := len(r.g.Messages()); n != 2 {
		t.Fatalf("%d worker requests (planner + unit expected)", n)
	}
}

// T18.217 — a damaged repository object: the overseer stops with a
// repository condition before any further side effect instead of
// committing on top of a corrupt repository.
func TestT18217CorruptedRepositoryObject(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	broken := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionUnitGates && s == "intent" && !broken {
			broken = true
			// Corrupt the object HEAD's commit is stored in.
			head := strings.TrimSpace(p15Git(t, r.dir, "rev-parse", "HEAD"))
			obj := filepath.Join(r.dir, ".git", "objects", head[:2], head[2:])
			_ = os.Chmod(obj, 0o644)
			if err := os.WriteFile(obj, []byte("not a git object"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	_, _ = r.eng().Run(context.Background(), r.execID, 50)
	r.eng().SetStepHook(nil)
	if !broken {
		t.Fatal("setup: unit gates not reached")
	}
	if r.execStatus(t) == "COMPLETED" {
		t.Fatal("completed on a corrupt repository")
	}
	if r.execStatus(t) != "RECOVERY_REQUIRED" {
		t.Fatalf("execution %s", r.execStatus(t))
	}
	cond := p18RecoveryCondition(t, r)
	if !strings.HasPrefix(cond, "REPOSITORY_") && cond != "CHECKPOINT_REJECTED" && cond != "PENDING_CHECKPOINT" {
		t.Fatalf("condition %q (%s), want a repository condition", cond, r.recoveryReason(t))
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE project_id = '"+r.pid.String()+"' AND status = 'CREATED' AND kind = 'UNIT'"); n != 0 {
		t.Fatal("a checkpoint was recorded on a corrupt repository")
	}
}
