package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"overseer/internal/audit"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/health"
)

// T18.221 — full chaos acceptance: one project runs through a compression
// outage, a worker failure before send, a post-send disconnect, a
// validation failure, a database restart, an overseer crash, a git
// checkpoint failure, a context reset (continuation), a WBS amendment, an
// appended phase, a disk warning and a provider outage + recovery. It ends
// COMPLETED (every stop on the way was an explicit, recoverable state an
// operator resolved) with no silent corruption: clean invariant scan,
// VALID audit chain, one checkpoint per DONE unit, unique checkpoint
// commits, no unit request delivered twice without a reviewed resend.
func TestT18221ChaosAcceptance(t *testing.T) {
	px := newPGProxy(t)
	phases := []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}}
	circuit := p18CompressionCircuit(2, time.Hour)
	// Every instance reaches PostgreSQL through the fault proxy (the
	// "database restart" target).
	r, restart := p16RestartRig(t, p16Options{phases: phases, config: func(c *config.Config) { circuit(c); px.configure(c) }})
	ctx := context.Background()
	// The rig seeds phases directly: keep the project's phase-order mark in
	// step so the API can append a phase; continuations run automatically.
	r.exec(t, "UPDATE projects SET max_phase_order = (SELECT max(phase_order) FROM phases WHERE project_id = '"+r.pid.String()+"') WHERE id = '"+r.pid.String()+"'")
	r.exec(t, "UPDATE project_settings SET auto_continue = true WHERE project_id = '"+r.pid.String()+"'")

	// Compression outage for the whole run (canonical prompts fit).
	p18ForceCompression(t, r)
	r.srv.Script(providertest.Reply{Status: 503, Body: `{"error":{"message":"down"}}`})
	// Disk warning (alert only).
	r.h.App.DiskMonitor().SetStatFS(func(string) (uint64, uint64, error) { return 1000, 100, nil })

	// Request-aware worker: planner replies per phase; unit b first reports
	// INCOMPLETE (context reset / continuation), everything else completes.
	var mu sync.Mutex
	seen := map[string]int{}
	plan := p16Reply("complete", "Planned the phase.", `"plan":`+r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	done := p16Reply("complete", "Worker reports complete.", "")
	incomplete := p16Reply("incomplete", "Half way there.", "")
	r.g.Responder = func(m hermestest.Message) *hermestest.Behavior {
		mu.Lock()
		defer mu.Unlock()
		if m.Metadata["state_key"] == "PHASE_PLAN" {
			return &hermestest.Behavior{Content: &plan}
		}
		seen["unit"]++
		if seen["unit"] == 5 {
			// One unit reports INCOMPLETE: a continuation follows.
			return &hermestest.Behavior{Content: &incomplete, Usage: p14Usage(1000)}
		}
		return &hermestest.Behavior{Content: &done, Usage: p14Usage(1000)}
	}
	r.setCheck(t, "a", 1) // validation failure of unit a's first result

	reg := faults.NewRegistry()
	fctx, fdone := faults.WithRegistry(ctx, reg)
	defer fdone()
	intents, dispatches := 0, 0
	crash := false
	var applied []string
	hook := func(_ context.Context, a execution.StepAction, s string) error {
		if s != "intent" {
			return nil
		}
		intents++
		switch {
		case a == execution.ActionDispatch:
			dispatches++
			switch dispatches {
			case 1:
				applied = append(applied, "worker failure before send")
				reg.Arm(faults.Action{Point: faults.BeforeWorkerSend, Times: 1, Kind: faults.KindError})
			case 3:
				r.setCheck(t, "a", 0) // the fix implements the behaviour
			case 4:
				applied = append(applied, "post-send disconnect")
				reg.Arm(faults.Action{Point: faults.AfterWorkerSend, Times: 1, Kind: faults.KindError})
			case 6:
				applied = append(applied, "provider outage")
				proxyOf(t, r.g).Stop()
				time.AfterFunc(400*time.Millisecond, func() { proxyOf(t, r.g).Start(t) })
			}
		case a == execution.ActionUnitGates && !strings.Contains(strings.Join(applied, ","), "git checkpoint failure"):
			applied = append(applied, "git checkpoint failure")
			reg.Arm(faults.Action{Point: faults.BeforeGitCommit, Times: 1, Kind: faults.KindError})
		case a == execution.ActionMoreWork && !strings.Contains(strings.Join(applied, ","), "database restart"):
			applied = append(applied, "database restart")
			px.Cut()
			time.AfterFunc(300*time.Millisecond, px.Restore)
		case a == execution.ActionPhaseValidation && !strings.Contains(strings.Join(applied, ","), "overseer crash"):
			applied = append(applied, "overseer crash")
			crash = true
			return errP16Crash
		case a == execution.ActionMorePhases && !strings.Contains(strings.Join(applied, ","), "appended phase"):
			applied = append(applied, "appended phase", "WBS amendment")
			b := newBrowser(t, r.h.BaseURL)
			var ph struct {
				ID string `json:"id"`
			}
			code, raw := b.api(http.MethodPost, "/api/v1/projects/"+r.pid.String()+"/phases", map[string]any{"name": "Three", "description": "appended"})
			if code >= 300 || json.Unmarshal(raw, &ph) != nil {
				t.Errorf("append phase: %d %s", code, raw)
				return nil
			}
			_, _ = b.api(http.MethodPut, "/api/v1/projects/"+r.pid.String()+"/phases/"+ph.ID+"/wbs", map[string]any{"content": "# Three\n- deliver"})
			_, _ = b.api(http.MethodPut, "/api/v1/projects/"+r.pid.String()+"/phases/"+r.phases[2].String()+"/wbs",
				map[string]any{"content": "# Two (amended)\n- deliver more"})
		}
		return nil
	}
	r.eng().SetStepHook(hook)

	base := func() string {
		return "/api/v1/projects/" + r.pid.String() + "/executions/" + r.execID.String() + "/recovery"
	}
	ops, resends, alertSeen := 0, 0, false
	for i := 0; i < 500 && r.execStatus(t) != "COMPLETED"; i++ {
		_, err := r.eng().Run(fctx, r.execID, 50)
		if crash {
			crash = false
			restart()
			r.h.App.DiskMonitor().SetStatFS(func(string) (uint64, uint64, error) { return 1000, 100, nil })
			r.eng().SetStepHook(hook)
			continue
		}
		if err != nil {
			time.Sleep(50 * time.Millisecond)
		}
		if !alertSeen {
			alertSeen = p18Alert(r.h.App.Alerts().Evaluate(ctx), health.AlertDiskThreshold).Active
		}
		switch r.execStatus(t) {
		case "WAITING":
			time.Sleep(50 * time.Millisecond)
		case "RECOVERY_REQUIRED":
			if ops++; ops > 12 {
				t.Fatalf("too many interventions; last %s (%s); applied %v", p18RecoveryCondition(t, r), r.recoveryReason(t), applied)
			}
			b := newBrowser(t, r.h.BaseURL)
			var plans struct {
				Current *struct {
					SafeActions []string `json:"safe_actions"`
				} `json:"current"`
				CurrentHash string `json:"current_hash"`
			}
			if b.getJSON(base()+"/plan", &plans) != http.StatusOK || plans.Current == nil {
				t.Fatal("no plan")
			}
			safe := strings.Join(plans.Current.SafeActions, ",")
			body := map[string]any{"reason": "chaos acceptance", "confirm": true, "evidence_hash": plans.CurrentHash,
				"idempotency_key": "chaos-acceptance-" + itoa(ops)}
			switch {
			case strings.Contains(safe, "RESOLVE_DELIVERY"):
				body["action"], body["resolution"] = "RESOLVE_DELIVERY", "RESEND_AFTER_REVIEW"
				resends++
			case strings.Contains(safe, "RETRY_CHECKPOINT"):
				body["action"] = "RETRY_CHECKPOINT"
			case strings.Contains(safe, "RUN_VALIDATION"):
				body["action"] = "RUN_VALIDATION"
			default:
				body["action"] = "MANUAL_RESUME"
			}
			if code, raw := b.api(http.MethodPost, base()+"/actions", body); code != http.StatusOK {
				t.Fatalf("operator %s on %s: %d %s", body["action"], p18RecoveryCondition(t, r), code, raw)
			}
		}
	}
	r.eng().SetStepHook(nil)
	if r.execStatus(t) != "COMPLETED" {
		t.Fatalf("not completed: %s (%s); applied %v", r.execStatus(t), r.recoveryReason(t), applied)
	}
	t.Logf("applied %v; %d operator interventions, %d reviewed resends", applied, ops, resends)
	for _, want := range []string{"worker failure before send", "post-send disconnect", "provider outage", "git checkpoint failure",
		"database restart", "overseer crash", "appended phase", "WBS amendment"} {
		if !strings.Contains(strings.Join(applied, ","), want) {
			t.Errorf("%s was not exercised", want)
		}
	}
	if !alertSeen {
		t.Error("the disk warning never raised DISK_THRESHOLD")
	}
	if n := r.count(t, "SELECT count(*) FROM continuations WHERE project_id = '"+r.pid.String()+"'"); n == 0 {
		t.Errorf("no context reset / continuation happened: %d CONTINUATION steps, results %s", r.count(t, "SELECT count(*) FROM execution_steps WHERE action = 'CONTINUATION' AND project_id = '"+r.pid.String()+"'"),
			p18FirstID(t, r.h, "SELECT string_agg(status, ',' ORDER BY created_at) FROM worker_results WHERE project_id = '"+r.pid.String()+"'"))
	}
	if n := r.count(t, "SELECT count(*) FROM compression_runs WHERE project_id = '"+r.pid.String()+"' AND outcome IN ('FALLBACK_CANONICAL', 'FAILED')"); n == 0 {
		t.Error("the compression outage was not exercised")
	}
	// No silent corruption.
	rep, err := r.h.App.Integrity().Scan(ctx, nil, nil)
	if err != nil || !rep.OK() {
		t.Fatalf("invariant scan: %v %v", err, p18Violations(rep))
	}
	if v, err := r.h.App.AuditVerifier().Verify(ctx, nil); err != nil || v.Status != audit.ChainValid {
		t.Fatalf("audit: %v %+v", err, v)
	}
	pid := r.pid.String()
	if d, c := r.count(t, "SELECT count(*) FROM work_units WHERE project_id = '"+pid+"' AND status = 'DONE'"),
		r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE project_id = '"+pid+"' AND kind = 'UNIT' AND status = 'CREATED'"); d != c || d == 0 {
		t.Fatalf("%d DONE units, %d unit checkpoints", d, c)
	}
	if n := r.count(t, `SELECT COALESCE(max(n), 0) FROM (SELECT count(*) AS n FROM worker_dispatches WHERE project_id = '`+pid+`'
		AND work_unit_id IS NOT NULL AND status = 'COMPLETED' GROUP BY work_unit_id) x`); n > 2+resends {
		t.Fatalf("a unit's request completed %d times", n)
	}
	c := exec.Command("git", "log", "--all", "--format=%B")
	c.Dir = r.dir
	out, _ := c.Output()
	trailers := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Overseer-Checkpoint:") {
			if trailers[l] {
				t.Fatalf("checkpoint committed twice: %s", l)
			}
			trailers[l] = true
		}
	}
}
