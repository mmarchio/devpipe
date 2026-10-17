package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/workerresult"
)

// P13-54 (WBS 13.67-13.69, 13.96): the result REST surface.
func TestP1367ResultAPI(t *testing.T) {
	r := p13NewRig(t)
	w := r.base + "/worker"
	dc := r.report(t, p13Complete)
	di := r.report(t, p13Incomplete)
	dp := r.report(t, p13Prose)

	code, body, _ := p9HTTP(t, http.MethodGet, w+"/results", "")
	m := p12JSON(t, body)
	if code != 200 || m["total"].(float64) != 2 {
		t.Fatalf("T13.67.1 %s", body)
	}
	for q, want := range map[string]float64{"?status=complete": 1, "?status=incomplete": 1, "?status=blocked": 0, "?state=IMPLEMENT&role=ENGINEER": 2,
		"?role=TESTER": 0, "?work_unit=" + r.units["11.3"].String(): 2, "?phase=" + r.pos.PhaseID.String(): 2,
		"?session=" + dc.SessionID.String() + "&limit=1": 2} {
		_, b, _ := p9HTTP(t, http.MethodGet, w+"/results"+q, "")
		if got := p12JSON(t, b)["total"].(float64); got != want {
			t.Fatalf("T13.67.2 %s → %v", q, got)
		}
	}
	if code, _, _ = p9HTTP(t, http.MethodGet, w+"/results?status=done", ""); code != 400 {
		t.Fatal("bad filter")
	}
	ri := r.result(t, di)
	_, body, _ = p9HTTP(t, http.MethodGet, w+"/results/"+ri.ID.String(), "")
	det := p12JSON(t, body)
	if det["continuation_status"] == nil || det["worker_report"] != "Worker Report: INCOMPLETE" || det["next_action"].(map[string]any)["label"] != "worker-reported, advisory" {
		t.Fatalf("detail %s", body)
	}
	_, body, _ = p9HTTP(t, http.MethodGet, w+"/results/current", "")
	if !strings.Contains(body, "Validation: NOT YET RUN") {
		t.Fatalf("13.70 %s", body)
	}

	// 13.68 parse attempts.
	_, body, _ = p9HTTP(t, http.MethodGet, w+"/parse-attempts?outcome=REJECTED", "")
	att := p12JSON(t, body)["attempts"].([]any)
	if len(att) != 1 || att[0].(map[string]any)["dispatch_id"] != dp.ID.String() || att[0].(map[string]any)["error_code"] != "RESULT_MISSING" {
		t.Fatalf("13.68 %s", body)
	}
	aid := att[0].(map[string]any)["id"].(string)
	if code, _, _ = p9HTTP(t, http.MethodGet, w+"/parse-attempts/"+aid, ""); code != 200 {
		t.Fatal("attempt detail")
	}

	// 13.69 reparse: stored response, no worker request, audited.
	sends := len(r.g.Messages())
	code, body, _ = p9HTTP(t, http.MethodPost, w+"/dispatches/"+dc.ID.String()+"/reparse", "")
	rp := p12JSON(t, body)
	if code != 200 || rp["attempt"].(map[string]any)["outcome"] != "DUPLICATE" || rp["attempt"].(map[string]any)["reparse"] != true || len(r.g.Messages()) != sends {
		t.Fatalf("T13.69.1-T13.69.3 %d %s", code, body)
	}

	// 13.96 settings.
	_, body, _ = p9HTTP(t, http.MethodGet, r.base+"/settings/result-protocol", "")
	if s := p12JSON(t, body); s["parse_mode"] != "STRICT" || s["repair_mode"] != "DETERMINISTIC_ONLY" || s["stored"] != false {
		t.Fatalf("T13.96.1 %s", body)
	}
	if code, _, _ = p9HTTP(t, http.MethodPut, r.base+"/settings/result-protocol", `{"max_completed_items":0}`); code != 400 {
		t.Fatal("T13.96.2")
	}
	if code, body, _ = p9HTTP(t, http.MethodPut, r.base+"/settings/result-protocol", `{"parse_mode":"TOLERANT"}`); code != 200 || p12JSON(t, body)["parse_mode"] != "TOLERANT" {
		t.Fatal("put settings")
	}
	_, body, _ = p9HTTP(t, http.MethodGet, w+"/results/readiness", "")
	if p12JSON(t, body)["ready"] != false {
		t.Fatal("readiness before ensure")
	}
	if code, _, _ = p9HTTP(t, http.MethodPost, r.base+"/settings/result-protocol/instructions", ""); code != 200 {
		t.Fatal("ensure")
	}
	if _, body, _ = p9HTTP(t, http.MethodGet, w+"/results/readiness", ""); p12JSON(t, body)["ready"] != true {
		t.Fatalf("readiness %s", body)
	}
	_, body, _ = p9HTTP(t, http.MethodGet, w+"/results/metrics", "")
	if mt := p12JSON(t, body)["overall"].(map[string]any); mt["parsed"].(float64) != 2 || mt["missing"].(float64) != 1 || mt["duplicates"].(float64) != 1 {
		t.Fatalf("metrics %s", body)
	}

	// T13.67.3 project isolation.
	_, pb, _ := p9HTTP(t, http.MethodPost, r.h.BaseURL+"/api/v1/projects", `{"name":"p13-other","description":"x"}`)
	other := r.h.BaseURL + "/api/v1/projects/" + p12JSON(t, pb)["id"].(string)
	for _, path := range []string{"/worker/results/" + ri.ID.String(), "/worker/parse-attempts/" + aid} {
		if code, _, _ = p9HTTP(t, http.MethodGet, other+path, ""); code != 404 {
			t.Fatalf("cross-project %s", path)
		}
	}
	if code, _, _ = p9HTTP(t, http.MethodPost, other+"/worker/dispatches/"+dc.ID.String()+"/reparse", ""); code != 404 {
		t.Fatal("cross-project reparse")
	}
	if code, _, _ = p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/projects/00000000-0000-0000-0000-000000000009/worker/results", ""); code != 404 {
		t.Fatal("unknown project")
	}
}

// P13-55 (WBS 13.98): complete worker report end to end.
func TestP1398CompleteReport(t *testing.T) {
	r := p13NewRig(t)
	ctx := context.Background()
	before := r.boundary(t)
	d := r.report(t, p13Complete)
	raw, _ := r.ws().Response(ctx, r.pos.ProjectID, d.ID)
	res := r.result(t, d)
	a := r.attempts(t, d)
	if raw.Content != p13Complete || res.Result.Metadata.RawResponseHash != raw.ContentHash || res.Result.ProtocolVersion != workerresult.ProtocolV1_0 ||
		len(a) != 1 || a[0].Outcome != workerresult.OutcomeAccepted || res.Result.NormalizedHash == "" || res.Result.NormalizedHash != workerresult.NormalizedHash(res.Result) {
		t.Fatal("13.98 raw / hashes / protocol / result")
	}
	if _, err := res.Result.Completion(); err != nil {
		t.Fatal("CompletionReport")
	}
	if r.boundary(t) != before || len(r.g.Messages()) != 1 {
		t.Fatal("13.98 unit, WBS, tests or state changed")
	}
}

// P13-55 (WBS 13.99): INCOMPLETE + 58,000 / 64,000 telemetry — Phase 13
// prepares the continuation inputs and decides nothing.
func TestP1399IncompleteWithTelemetry(t *testing.T) {
	r := p13NewRig(t)
	ctx := context.Background()
	sessions := len(r.g.Sessions())
	inc := `{"protocol_version":"1.0","status":"incomplete","summary":"Implemented parser but tests remain.","completed":["Parser"],"remaining":["Tests"],"blocking":[],"next_action":"Implement tests."}`
	d := r.report(t, inc, hermestest.Behavior{Usage: &hermestest.Usage{ContextTokens: hermestest.I64(58000), ContextWindow: hermestest.I64(64000)}})
	c, err := r.result(t, d).Result.Continuation()
	if err != nil || c.Remaining[0] != "Tests" {
		t.Fatal("ContinuationStatus")
	}
	cp, _ := r.ws().ContextProgress(ctx, r.pos.ProjectID)
	if cp.Tokens != 58000 || cp.Percent != 90.63 {
		t.Fatalf("telemetry not retained separately: %+v", cp)
	}
	if len(r.g.Sessions()) != sessions || len(r.g.Messages()) != 1 {
		t.Fatal("13.99 reset or continue performed")
	}
}

// P13-56 (WBS 13.103): Phase 13 acceptance.
func TestP13103Acceptance(t *testing.T) {
	r := p13NewRig(t)
	ctx := context.Background()
	before := r.boundary(t)
	sessions := len(r.g.Sessions())
	dA, dB, dC, dD := r.report(t, p13Complete), r.report(t, p13Incomplete), r.report(t, p13Blocked), r.report(t, p13Prose)

	a, b, c := r.result(t, dA).Result, r.result(t, dB).Result, r.result(t, dC).Result
	if !a.IsComplete() || !b.IsIncomplete() || !c.IsBlocked() {
		t.Fatal("A/B/C statuses")
	}
	if _, err := a.Completion(); err != nil {
		t.Fatal("A CompletionReport")
	}
	if _, err := b.Continuation(); err != nil {
		t.Fatal("B ContinuationStatus")
	}
	if _, err := c.Blocked(); err != nil {
		t.Fatal("C BlockedStatus")
	}
	if at := r.attempts(t, dD); len(at) != 1 || at[0].Outcome != workerresult.OutcomeRejected {
		t.Fatal("D protocol failure")
	}
	if _, err := r.rs().ResultByDispatch(ctx, r.pos.ProjectID, dD.ID); err == nil {
		t.Fatal("D fabricated a result")
	}
	for i, pair := range []struct {
		d   string
		res workerresult.WorkerResult
	}{{dA.ID.String(), a}, {dB.ID.String(), b}, {dC.ID.String(), c}} {
		m := pair.res.Metadata
		if m.DispatchID.String() != pair.d || m.ProjectID != r.pos.ProjectID || *m.PhaseID != r.pos.PhaseID || *m.WorkUnitID != r.units["11.3"] ||
			m.StateKey != "IMPLEMENT" || m.RoleKey != "ENGINEER" || m.RawResponseHash == "" || pair.res.NormalizedHash != workerresult.NormalizedHash(pair.res) {
			t.Fatalf("binding / provenance / hash of result %d", i)
		}
	}
	if r.boundary(t) != before {
		t.Fatal("ACTIVE_TASK / unit / WBS / state changed")
	}
	if len(r.g.Sessions()) != sessions || len(r.g.Messages()) != 4 {
		t.Fatal("session reset or extra Hermes request")
	}
	p12NoSecret(t, "events", p10Rows(t, r.h, "worker_events", "project_id = '"+r.pos.ProjectID.String()+"'"))
	if strings.Contains(p10Rows(t, r.h, "worker_events", "project_id = '"+r.pos.ProjectID.String()+"' AND type LIKE 'WORKER_RESULT%'"), "Implemented worker result parsing") {
		t.Fatal("T13.64.3 result text in audit events")
	}
}
