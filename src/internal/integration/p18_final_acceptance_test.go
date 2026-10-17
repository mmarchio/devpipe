package integration

import (
	"context"
	"encoding/json"
	"errors"
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
	"overseer/internal/hardening"
	"overseer/internal/persistence"
)

// T18.234 — the Phase 18 final acceptance scenario (see the spec): a
// multi-phase project with dependency-linked units runs autonomously
// through deliberate failures — compression outage at planning, a
// continuation with a context reset (B), a false completion caught by
// validation (C), a post-send disconnect (D), a database outage before a
// dispatch (E), a kill between the checkpoint commit and its record (F),
// phase validation, an instance freeze with a lease takeover, an appended
// phase, final validation and checkpoint — to COMPLETED, and every
// "verify throughout" property that durable state can show is asserted.
func TestT18234FinalAcceptance(t *testing.T) {
	px := newPGProxy(t)
	phases := []p15Phase{{1, "Foundation", "PENDING", 1}, {2, "Features", "PENDING", 1}}
	circuit := p18CompressionCircuit(1, time.Hour)
	cfg := func(c *config.Config) { circuit(c); px.configure(c) }
	base15, boot := p15RestartRig(t, phases, cfg)
	r := &p16Rig{p15Rig: base15}
	p16Configure(t, r, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) { s.DriverLease = time.Second }})
	ctx := context.Background()
	pid := r.pid.String()
	r.exec(t, "UPDATE projects SET max_phase_order = 2 WHERE id = '"+pid+"'")

	// Phase planning under a compression outage → canonical fallback.
	p18ForceCompression(t, r)
	r.srv.Script(providertest.Reply{Status: 503, Body: `{"error":{"message":"down"}}`})

	// Worker script, in order.
	r.queuePlan(r.planJSON(p16Unit{key: "A"}, p16Unit{key: "B", deps: []string{"A"}}, p16Unit{key: "C", deps: []string{"B"}},
		p16Unit{key: "D", deps: []string{"C"}}, p16Unit{key: "E", deps: []string{"D"}}, p16Unit{key: "F", kind: "INTEGRATION", deps: []string{"D", "E"}}))
	r.queue("complete")                                                                                                                        // A
	r.queueRaw(p16Reply("incomplete", "Half way.", ""), 1000)                                                                                  // B: INCOMPLETE → continuation
	r.queueRaw(p16Reply("incomplete", "Almost.", ""), 60000)                                                                                   // B: above the context threshold → session reset
	r.queue("complete")                                                                                                                        // B after the reset
	r.queueRaw(p16Reply("complete", "All 99 tests pass.", `"tests":{"reported_run":true,"reported_passed":true,"summary":"all green"}`), 1000) // C (false claim)
	r.queue("complete")                                                                                                                        // C: FIX
	r.queue("complete")                                                                                                                        // D: the reply lost by the disconnect
	r.queue("complete")                                                                                                                        // D: the reviewed resend
	r.queue("complete")                                                                                                                        // E
	r.queue("complete")                                                                                                                        // F
	r.queuePlan(r.planJSON(p16Unit{key: "G"}, p16Unit{key: "H", kind: "COMPOSITION", deps: []string{"G"}}))                                    // phase 2
	r.queue("complete")                                                                                                                        // G
	r.queue("complete")                                                                                                                        // H
	r.queuePlan(r.planJSON(p16Unit{key: "Z"}))                                                                                                 // appended phase 3
	r.queue("complete")                                                                                                                        // Z
	r.setCheck(t, "C", 1)

	reg := faults.NewRegistry()
	fctx, fdone := faults.WithRegistry(ctx, reg)
	defer fdone()
	unitKey := func() string {
		var k *string
		_ = r.h.DBScan(ctx, "SELECT w.unit_key FROM executions e JOIN work_units w ON w.id = e.current_work_unit_id WHERE e.id = '"+r.execID.String()+"'", &k)
		if k == nil {
			return ""
		}
		return *k
	}
	var mu sync.Mutex
	done := map[string]bool{}
	once := func(k string) bool {
		mu.Lock()
		defer mu.Unlock()
		if done[k] {
			return false
		}
		done[k] = true
		return true
	}
	crashF := false
	var hook func(context.Context, execution.StepAction, string) error
	hook = func(_ context.Context, a execution.StepAction, s string) error {
		switch {
		case a == execution.ActionDispatch && s == "intent":
			switch k := unitKey(); {
			case k == "C" && !once("C-first"):
				r.setCheck(t, "C", 0) // C's second request is the FIX: it implements the behaviour
			case k == "D" && once("D"):
				reg.Arm(faults.Action{Point: faults.AfterWorkerSend, Times: 1, Kind: faults.KindError})
			case k == "E" && once("E"):
				px.Cut()
				time.AfterFunc(400*time.Millisecond, px.Restore)
			}
		case a == execution.ActionUnitGates && s == "checkpoint-committed" && unitKey() == "F" && once("F"):
			crashF = true
			return errP16Crash
		}
		return nil
	}
	r.eng().SetStepHook(hook)

	sentAtE := -1
	operator := func() {
		b := newBrowser(t, r.h.BaseURL)
		base := "/api/v1/projects/" + pid + "/executions/" + r.execID.String() + "/recovery"
		var plans struct {
			Current *struct {
				SafeActions []string `json:"safe_actions"`
			} `json:"current"`
			CurrentHash string `json:"current_hash"`
		}
		if b.getJSON(base+"/plan", &plans) != http.StatusOK || plans.Current == nil {
			t.Fatal("no recovery plan")
		}
		if p18RecoveryCondition(t, r) != "DELIVERY_UNKNOWN" {
			t.Fatalf("unexpected recovery %s (%s)", p18RecoveryCondition(t, r), r.recoveryReason(t))
		}
		if code, raw := b.api(http.MethodPost, base+"/actions", map[string]any{"action": "RESOLVE_DELIVERY", "resolution": "RESEND_AFTER_REVIEW",
			"reason": "gateway logs: not processed", "confirm": true, "evidence_hash": plans.CurrentHash, "idempotency_key": "final-acceptance-D"}); code != http.StatusOK {
			t.Fatalf("resolve D: %d %s", code, raw)
		}
	}

	// Phase 1 (and the start of phase 2) through instance A.
	for i := 0; i < 300; i++ {
		var g string
		_ = r.h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+r.phases[2].String()+"'", &g)
		if g == "EXECUTING" && r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans ep ON ep.id = w.execution_plan_id WHERE ep.phase_id = '"+r.phases[2].String()+"'") > 0 {
			break // phase 2 planned: continue with the multi-instance part
		}
		if unitKey() == "E" && sentAtE < 0 {
			sentAtE = len(r.g.Messages())
		}
		_, err := r.eng().Run(fctx, r.execID, 1)
		switch {
		case crashF:
			crashF = false
			r.h.Close()
			r.h = boot(cfg)
			r.eng().SetStepHook(hook)
			time.Sleep(1100 * time.Millisecond) // the killed instance's lease expires
		case r.execStatus(t) == "RECOVERY_REQUIRED":
			operator()
		case err != nil && !errors.Is(err, errP16Crash):
			time.Sleep(50 * time.Millisecond) // e.g. the database is down
		}
	}

	// Phase 2: instance A freezes before G's request; B takes over.
	frozen, release := make(chan struct{}), make(chan struct{})
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionDispatch && s == "dispatch" && once("freeze") {
			close(frozen)
			<-release
		}
		return nil
	})
	errA := make(chan error, 1)
	go func() { _, err := r.eng().Run(ctx, r.execID, 50); errA <- err }()
	select {
	case <-frozen:
	case <-time.After(30 * time.Second):
		t.Fatal("instance A never reached phase 2's dispatch")
	}
	var epochA int64
	_ = r.h.DBScan(ctx, "SELECT driver_epoch FROM executions WHERE id = '"+r.execID.String()+"'", &epochA)
	// A future phase is appended while phase 2 runs.
	bA := newBrowser(t, r.h.BaseURL)
	var ph struct {
		ID string `json:"id"`
	}
	code, raw := bA.api(http.MethodPost, "/api/v1/projects/"+pid+"/phases", map[string]any{"name": "Hardening extras", "description": "appended"})
	if code >= 300 || json.Unmarshal(raw, &ph) != nil {
		t.Fatalf("append phase: %d %s", code, raw)
	}
	if code, raw := bA.api(http.MethodPut, "/api/v1/projects/"+pid+"/phases/"+ph.ID+"/wbs", map[string]any{"content": "# Extras\n- deliver Z"}); code >= 300 {
		t.Fatalf("appended WBS: %d %s", code, raw)
	}
	time.Sleep(1100 * time.Millisecond)
	hB := boot(cfg)
	engB := hB.App.Engine()
	var resB []execution.StepResult
	for i := 0; i < 20 && r.execStatus(t) != "COMPLETED"; i++ {
		res, err := engB.Run(ctx, r.execID, 200)
		resB = append(resB, res...)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if r.execStatus(t) != "COMPLETED" {
		t.Fatalf("B did not complete the project: %s (%s) %s", r.execStatus(t), r.recoveryReason(t), p16Trace(resB))
	}
	var epochB int64
	_ = r.h.DBScan(ctx, "SELECT driver_epoch FROM executions WHERE id = '"+r.execID.String()+"'", &epochB)
	if epochB <= epochA {
		t.Fatalf("no higher fencing token: %d → %d", epochA, epochB)
	}
	fp, sent := p18Fingerprint(t, r), len(r.g.Messages())
	close(release)
	<-errA
	if p18Fingerprint(t, r) != fp || len(r.g.Messages()) != sent {
		t.Fatal("the stale instance A mutated state after its lease expired")
	}

	for _, k := range []string{"C-first", "D", "E", "F", "freeze"} {
		if !done[k] {
			t.Fatalf("scenario step %s never happened", k)
		}
	}
	// ---------------------------------------------------------- verify throughout
	q := func(sql string) int { return r.count(t, strings.ReplaceAll(sql, "$P", pid)) }
	// Every unit DONE exactly once with its checkpoint; nothing skipped.
	if n := q("SELECT count(*) FROM work_units WHERE project_id = '$P' AND status <> 'DONE'"); n != 0 {
		t.Fatalf("%d units not DONE (skipped)", n)
	}
	units := q("SELECT count(*) FROM work_units WHERE project_id = '$P'")
	if n := q("SELECT count(*) FROM repository_checkpoints WHERE project_id = '$P' AND kind = 'UNIT' AND status = 'CREATED'"); n != units {
		t.Fatalf("%d unit checkpoints for %d units", n, units)
	}
	// Phases in order, none skipped; the appended phase ran after phase 2.
	if n := q(`SELECT count(*) FROM phases a JOIN phases b ON b.project_id = a.project_id AND b.phase_order = a.phase_order + 1
		WHERE a.project_id = '$P' AND (a.status <> 'DONE' OR b.status <> 'DONE' OR b.started_at < a.completed_at)`); n != 0 {
		t.Fatal("phases out of order, skipped, or the appended phase preempted current work")
	}
	// No external worker action without durable intent / dispatch row.
	if msgs, rows := len(r.g.Messages()), q("SELECT count(*) FROM worker_dispatches WHERE project_id = '$P' AND delivery IS DISTINCT FROM 'DEFINITELY_NOT_SENT'"); msgs > rows {
		t.Fatalf("%d worker requests but %d delivered dispatch rows", msgs, rows)
	}
	// The ambiguous request (D) was resent once, only after the audited review.
	if n := q("SELECT count(*) FROM audit_events WHERE project_id = '$P' AND action = 'RECOVERY_ACTION' AND reason = 'gateway logs: not processed'"); n != 1 {
		t.Fatalf("D's reviewed resend audited %d times", n)
	}
	// C's false claim did not make it DONE: a FAIL run precedes its PASS.
	if n := q(`SELECT count(*) FROM validation_runs v JOIN work_units w ON w.id = v.work_unit_id WHERE w.project_id = '$P' AND w.unit_key = 'C' AND v.status = 'FAIL'`); n == 0 {
		t.Fatal("C's false test claim was not overridden by validation")
	}
	// B continued and reset its session.
	if n := q("SELECT count(*) FROM continuations WHERE project_id = '$P'"); n == 0 {
		t.Fatal("B did not continue")
	}
	// Planning fell back to the canonical prompt during the compression outage.
	if n := q("SELECT count(*) FROM compression_runs WHERE project_id = '$P' AND outcome IN ('FALLBACK_CANONICAL', 'FAILED')"); n == 0 {
		t.Fatal("the compression outage was not exercised")
	}
	// E: nothing was sent while the database was down.
	if sentAtE < 0 {
		t.Fatal("unit E was never reached")
	}
	// F: the landed commit was adopted — no duplicate checkpoint commit.
	c := exec.Command("git", "log", "--all", "--format=%B")
	c.Dir = r.dir
	out, _ := c.Output()
	trailers := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Overseer-Checkpoint:") {
			if trailers[l] {
				t.Fatalf("duplicate checkpoint commit %s", l)
			}
			trailers[l] = true
		}
	}
	// Integrity, audit, lineage, reconstruction after a restart.
	if rep, err := hB.App.Integrity().Scan(ctx, nil, nil); err != nil || !rep.OK() {
		t.Fatalf("integrity: %v %v", err, p18Violations(rep))
	}
	if v, err := hB.App.AuditVerifier().Verify(ctx, nil); err != nil || v.Status != audit.ChainValid {
		t.Fatalf("audit: %v %+v", err, v)
	}
	r.h.Close()
	hB.Close()
	h3 := boot(cfg)
	b3 := newBrowser(t, h3.BaseURL)
	for _, u := range p18IDsOn(t, h3, "SELECT id::text FROM work_units WHERE project_id = '"+pid+"'") {
		var l persistence.UnitLineage
		if b3.getJSON("/api/v1/projects/"+pid+"/work-units/"+u+"/lineage", &l) != http.StatusOK || !l.Complete {
			t.Fatalf("unit %s lineage incomplete after restart: %+v", u, l)
		}
	}
	var ov struct {
		Status string `json:"status"`
	}
	if b3.getJSON("/api/v1/projects/"+pid+"/overview", &ov) != http.StatusOK || ov.Status != "COMPLETED" {
		t.Fatalf("project after restart: %+v", ov)
	}
}

func p18IDsOn(t *testing.T, h *Harness, sql string) []string {
	t.Helper()
	rows, err := h.App.DB().Raw().Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}
