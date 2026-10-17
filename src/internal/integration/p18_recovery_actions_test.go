package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"overseer/internal/config"
	"overseer/internal/execution"
)

// p18CheckpointRejected installs (or removes) a pre-commit hook that
// rejects every checkpoint commit (the rig runs git hooks), so the
// required checkpoint fails until it is removed. (A held index.lock no
// longer fails a checkpoint — it waits, WBS 18.57.)
func p18CheckpointRejected(t *testing.T, dir string, on bool) {
	t.Helper()
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if !on {
		_ = os.Remove(hook)
		return
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'checkpoint rejected by policy' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// P18-27 / P18-28 / P18-29 (WBS 18.8–18.11, 18.182–18.184).
func TestP1828RecoveryActions(t *testing.T) {
	r := p17NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.RunGitHooks = true }})
	ctx := context.Background()
	pid, eid := r.pid.String(), r.execID.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18CheckpointRejected(t, r.dir, true)
	r.runUntil(t, execution.StepRecoveryRequired, 100)
	if !strings.Contains(r.recoveryReason(t), "checkpoint") {
		t.Fatalf("reason %q", r.recoveryReason(t))
	}
	base := "/api/v1/projects/" + pid + "/executions/" + eid + "/recovery"
	// 18.8 / 18.10 — the plan is deterministic and reading it writes nothing.
	before := p18Fingerprint(t, r.p16Rig)
	var p1, p2 struct {
		CurrentHash string         `json:"current_hash"`
		Current     map[string]any `json:"current"`
		Recorded    []any          `json:"recorded_plans"`
	}
	r.b.getJSON(base+"/plan", &p1)
	r.b.getJSON(base+"/plan", &p2)
	if p1.CurrentHash == "" || p1.CurrentHash != p2.CurrentHash || len(p1.Recorded) != 0 || p1.Current["classification"] == "" {
		t.Fatalf("plan: %+v", p1)
	}
	if r.count(t, "SELECT count(*) FROM recovery_plans") != 0 || p18Fingerprint(t, r.p16Rig) != before {
		t.Fatal("inspecting the plan mutated state")
	}
	// 18.183 — confirmation required for high-impact actions.
	if code, _ := r.b.api(http.MethodPost, base+"/actions", map[string]any{"action": "MANUAL_RESUME", "reason": "x", "idempotency_key": "k-manual-0001"}); code != 400 {
		t.Fatalf("unconfirmed: %d", code)
	}
	// 18.184 — dry run describes changes and performs none.
	code, raw := r.b.api(http.MethodPost, base+"/actions", map[string]any{"action": "RETRY_CHECKPOINT", "dry_run": true})
	if code != 200 || !strings.Contains(string(raw), "external_operations") || !strings.Contains(string(raw), "commits at most once") {
		t.Fatalf("dry run: %d %s", code, raw)
	}
	if p18Fingerprint(t, r.p16Rig) != before || r.count(t, "SELECT count(*) FROM recovery_actions") != 0 {
		t.Fatal("dry run mutated state")
	}
	// The idempotency key is required for real actions.
	if code, _ := r.b.api(http.MethodPost, base+"/actions", map[string]any{"action": "RETRY_CHECKPOINT"}); code != 400 {
		t.Fatalf("keyless action: %d", code)
	}
	// 18.11 — a concurrent double submit with one key executes once.
	p18CheckpointRejected(t, r.dir, false)
	req := map[string]any{"action": "RETRY_CHECKPOINT", "reason": "lock removed", "idempotency_key": "k-retry-checkpoint-1", "evidence_hash": p1.CurrentHash}
	var wg sync.WaitGroup
	codes := make([]int, 4)
	bodies := make([]string, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, b := r.b.api(http.MethodPost, base+"/actions", req)
			codes[i], bodies[i] = c, string(b)
		}(i)
	}
	wg.Wait()
	ok := 0
	for i, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			if !strings.Contains(bodies[i], "in progress") {
				t.Fatalf("unexpected conflict: %s", bodies[i])
			}
		default:
			t.Fatalf("double submit: %d %s", c, bodies[i])
		}
	}
	if ok == 0 || r.count(t, "SELECT count(*) FROM recovery_actions") != 1 || r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'EXECUTION_RECOVERY_RESOLVED'") != 1 {
		t.Fatalf("executed %d times", r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'EXECUTION_RECOVERY_RESOLVED'"))
	}
	// A later repeat returns the recorded result (replay), no effect.
	code, raw = r.b.api(http.MethodPost, base+"/actions", req)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if code != 200 || out["replayed"] != true {
		t.Fatalf("replay: %d %s", code, raw)
	}
	r.runUntil(t, execution.StepCompleted, 100)
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE kind = 'UNIT' AND status = 'CREATED'"); n != 1 {
		t.Fatalf("unit checkpoints %d", n)
	}
	// 18.9 — the plan acted on was recorded and round-trips.
	var p3 struct {
		Recorded []map[string]any `json:"recorded_plans"`
		Actions  []map[string]any `json:"actions"`
	}
	r.b.getJSON(base+"/plan", &p3)
	if len(p3.Recorded) != 1 || p3.Recorded[0]["plan_hash"] != p1.CurrentHash || len(p3.Actions) != 1 || p3.Actions[0]["status"] != "SUCCEEDED" {
		t.Fatalf("recorded: %+v", p3)
	}
	plan, _ := json.Marshal(p3.Recorded[0]["plan"])
	cur, _ := json.Marshal(p1.Current)
	if string(plan) != string(cur) {
		t.Fatalf("plan round trip:\n%s\n%s", plan, cur)
	}
	// 18.182 — the audit row carries actor, reason, evidence, action, result.
	var md string
	_ = r.h.DBScan(ctx, "SELECT metadata::text FROM audit_events WHERE action = 'RECOVERY_ACTION' AND result = 'SUCCEEDED' AND reason = 'lock removed' ORDER BY seq LIMIT 1", &md)
	for _, want := range []string{`"recovery_action": "RETRY_CHECKPOINT"`, `"evidence_hash": "` + p1.CurrentHash, `"plan_hash"`, `"idempotency_key": "k-retry-checkpoint-1"`, `"result_execution_status": "RUNNING"`} {
		if !strings.Contains(md, want) {
			t.Fatalf("audit metadata lacks %s: %s", want, md)
		}
	}
}
