package integration

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/app"
	"overseer/internal/audit"
	"overseer/internal/backup"
	"overseer/internal/execution"
)

// T18.226 — backup / restore acceptance: during representative use (one
// completed project, one active mid-execution) a backup of the database
// and the repositories is taken; everything is destroyed; the database
// and repositories are restored. The completed project's history —
// prompts, dispatches, results, validation, checkpoints, events — is all
// there; the audit chain verifies; nothing external happens before the
// operator enables execution; afterwards the active project resumes
// through normal reconciliation and completes.
func TestT18226BackupRestoreAcceptance(t *testing.T) {
	r, restart := p16RestartRig(t, p16Options{config: p17Config(nil)})
	ctx := context.Background()
	// Completed project.
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 200)
	// Active project: planned, its unit not dispatched yet.
	act := &p16Rig{p15Rig: p15Build(t, r.h, r.g, r.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, act, p16Options{})
	act.queuePlan(act.planJSON(p16Unit{key: "x"}))
	if _, err := act.eng().Run(ctx, act.execID, 2); err != nil {
		t.Fatal(err)
	}
	// Representative audit history.
	b := newBrowser(t, r.h.BaseURL)
	b.page("/")
	_, _ = b.api(http.MethodPost, "/api/v1/projects/"+act.pid.String()+"/pause", nil)
	_, _ = b.api(http.MethodPost, "/api/v1/projects/"+act.pid.String()+"/resume", nil)

	history := func(pid string) map[string]int {
		out := map[string]int{}
		for name, q := range map[string]string{
			"worker_prompts":     "SELECT count(*) FROM worker_prompts WHERE project_id = '%s'",
			"canonical_prompts":  "SELECT count(*) FROM canonical_prompts WHERE project_id = '%s'",
			"worker_dispatches":  "SELECT count(*) FROM worker_dispatches WHERE project_id = '%s'",
			"worker_results":     "SELECT count(*) FROM worker_results WHERE project_id = '%s'",
			"validation_runs":    "SELECT count(*) FROM validation_runs WHERE project_id = '%s'",
			"checkpoints":        "SELECT count(*) FROM repository_checkpoints WHERE project_id = '%s'",
			"execution_events":   "SELECT count(*) FROM execution_events WHERE project_id = '%s'",
			"audit_events":       "SELECT count(*) FROM audit_events WHERE project_id = '%s'",
			"execution_complete": "SELECT count(*) FROM execution_completions WHERE project_id = '%s'",
		} {
			out[name] = r.count(t, p18Fmt(q, pid))
		}
		return out
	}
	before := history(r.pid.String())

	// Backup: database + repositories.
	dir := filepath.Join(t.TempDir(), "db")
	if _, err := backup.Backup(ctx, r.h.App.DB().Raw(), dir, backup.Options{AppVersion: app.Version, Profile: "test"}); err != nil {
		t.Fatal(err)
	}
	repos := map[string]string{}
	for _, repo := range []string{r.dir, act.dir} {
		cp := filepath.Join(t.TempDir(), "repo")
		if out, err := exec.Command("cp", "-a", repo+"/.", cp).CombinedOutput(); err != nil {
			t.Fatalf("copy repo: %v %s", err, out)
		}
		repos[repo] = cp
	}
	sent := len(r.g.Messages())

	// Disaster: the database and the repositories are gone.
	name := r.h.DBName()
	r.h.keepDB = true
	r.h.Close()
	for repo := range repos {
		if err := os.RemoveAll(repo); err != nil {
			t.Fatal(err)
		}
	}
	// Restore.
	p18RestoreOver(t, name, dir, "")
	for repo, cp := range repos {
		if out, err := exec.Command("cp", "-a", cp+"/.", repo).CombinedOutput(); err != nil {
			t.Fatalf("restore repo: %v %s", err, out)
		}
	}
	h := restart()
	act.h = h
	b = newBrowser(t, h.BaseURL)
	if st := p18RestoreStatus(t, b); st.State == nil || st.State.Status != "VALIDATED" {
		t.Fatalf("restore gate: %+v", st)
	}
	// Completed project reconstructed.
	if got := history(r.pid.String()); !p18SameCounts(got, before) {
		t.Fatalf("history after restore %v, before %v", got, before)
	}
	for _, path := range []string{"/api/v1/projects/" + r.pid.String() + "/overview",
		"/api/v1/projects/" + r.pid.String() + "/console/executions/" + r.execID.String() + "/timeline"} {
		if code, _ := b.api(http.MethodGet, path, nil); code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, code)
		}
	}
	if v, err := h.App.AuditVerifier().Verify(ctx, nil); err != nil || v.Status != audit.ChainValid {
		t.Fatalf("audit after restore: %v %+v", err, v)
	}
	// No external action before the operator enables execution.
	_, _ = act.eng().Run(ctx, act.execID, 10)
	if len(r.g.Messages()) != sent {
		t.Fatal("a worker request was sent before execution was enabled")
	}
	if code, body := b.api(http.MethodPost, "/api/v1/system/restore/enable", map[string]any{"reason": "restore reviewed"}); code != http.StatusOK {
		t.Fatalf("enable: %d %s", code, body)
	}
	act.queue("complete")
	act.runUntil(t, execution.StepCompleted, 200)
}

func p18Fmt(q, a string) string { return strings.ReplaceAll(q, "%s", a) }

func p18SameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
