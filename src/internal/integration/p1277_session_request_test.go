package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/worker"
)

// P12-77 (WBS 12.65): Session A with CWD → NewSession → Session B: A RESET,
// B ACTIVE, B's CWD UNKNOWN until ChangeDirectory confirms it, then ready.
func TestP1265PostResetCWD(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	a := r.newSession(t)
	r.cd(t, "/repos/project-a")
	if rd, _ := r.ws().Readiness(ctx, r.pos.ProjectID); !rd.Ready() {
		t.Fatalf("session A not ready: %+v", rd)
	}
	b := r.newSession(t)
	if a.ProviderSessionID == b.ProviderSessionID {
		t.Fatal("T12.20.1 new session reused the previous one")
	}
	if got := r.session(t, a.ID); got.Status != worker.SessionReset || got.ClosedAt == nil {
		t.Fatalf("A = %s", got.Status)
	}
	if b.Status != worker.SessionActive || b.WorkingDirectory != nil {
		t.Fatalf("B = %s cwd %v (must be UNKNOWN)", b.Status, b.WorkingDirectory)
	}
	rd, _ := r.ws().Readiness(ctx, r.pos.ProjectID)
	if rd.Ready() || rd.CWDInitialized || !rd.SessionActive {
		t.Fatalf("T12.26.3 B ready before cd: %+v", rd)
	}
	b2 := r.cd(t, "/repos/project-a")
	if b2.WorkingDirectory == nil || *b2.WorkingDirectory != "/repos/project-a" {
		t.Fatal("B CWD not confirmed")
	}
	if rd, _ := r.ws().Readiness(ctx, r.pos.ProjectID); !rd.Ready() {
		t.Fatalf("B not ready for dispatch: %+v", rd)
	}
	hist, _ := r.ws().Sessions(ctx, r.pos.ProjectID)
	if len(hist) != 2 || hist[0].ID != a.ID || hist[1].ID != b.ID {
		t.Fatal("T12.20.3 history")
	}
	evs := r.events(t)
	if !hasEvent(evs, worker.EventConnected) || !hasEvent(evs, worker.EventSessionCreated) || !hasEvent(evs, worker.EventSessionReset) || !hasEvent(evs, worker.EventCWDChanged) {
		t.Fatalf("T12.53.1/T12.53.2 events %v", evs)
	}
}

// P12-77 (WBS 12.70 + 12.71): successful request with provider telemetry
// 48,000 / 64,000.
func TestP1270SuccessfulRequestAndTelemetry(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	s := r.newSession(t)
	r.cd(t, "/repos/project-a")
	wp := r.prompt(t, "")
	before := r.workflowRows(t)
	r.g.Script(hermestest.Behavior{Content: ptrS("COMPLETE\nunit 11.3 implemented"), Usage: &hermestest.Usage{InputTokens: hermestest.I64(30000),
		OutputTokens: hermestest.I64(1200), ContextTokens: hermestest.I64(48000), ContextWindow: hermestest.I64(64000)}})
	res := r.dispatch(t, wp, 1)
	d := res.Dispatch
	if res.Err != nil || d.Status != worker.DispatchCompleted || *d.Delivery != worker.DeliveryReceived {
		t.Fatalf("dispatch %+v err %v", d, res.Err)
	}
	if d.WorkerPromptID != wp.ID || d.PromptHash != wp.Hash || d.SessionID != s.ID || d.Attempt != 1 {
		t.Fatal("T12.30.4 references")
	}
	msgs := r.g.Messages()
	if len(msgs) != 1 || msgs[0].RequestID != d.RequestID.String() || msgs[0].SessionID != s.ProviderSessionID || msgs[0].Content != wp.Content {
		t.Fatalf("T12.32.1 correlation/session %+v", msgs)
	}
	if msgs[0].Auth != "Bearer "+p12Token || msgs[0].Model != "hermes-worker" {
		t.Fatal("auth/model not applied")
	}
	raw, err := r.ws().Response(ctx, r.pos.ProjectID, d.ID)
	if err != nil || raw.Content != "COMPLETE\nunit 11.3 implemented" || raw.ContentHash != worker.ContentHash(raw.Content) {
		t.Fatalf("raw response for Phase 13: %+v %v", raw, err)
	}
	if active, _ := r.ws().ActiveSession(ctx, r.pos.ProjectID); active.ID != s.ID {
		t.Fatal("session changed by a send")
	}
	// Telemetry (12.71).
	if res.Usage == nil || res.Usage.Source != worker.SourceProviderActual || res.Usage.Usage.Estimated {
		t.Fatalf("usage %+v", res.Usage)
	}
	u, _ := res.Usage.Usage.Utilization()
	if u.Percent != 75 {
		t.Fatalf("utilization %v", u.Percent)
	}
	obs, _ := r.ws().UsageObservations(ctx, r.pos.ProjectID, nil)
	if len(obs) != 1 || *obs[0].Usage.ContextTokens != 48000 {
		t.Fatal("observation not persisted")
	}
	cp, _ := r.ws().ContextProgress(ctx, r.pos.ProjectID)
	if !cp.Known || cp.Tokens != 48000 || cp.Window != 64000 || cp.Percent != 75 || cp.Label != "Hermes" {
		t.Fatalf("read model %+v", cp)
	}
	code, body, _ := p9HTTP(t, http.MethodGet, r.base+"/worker", "")
	var st struct {
		Connection string `json:"connection"`
		Session    struct {
			Status           string `json:"status"`
			WorkingDirectory string `json:"working_directory"`
		} `json:"session"`
		Context struct {
			Tokens     int64   `json:"tokens"`
			Window     int64   `json:"window"`
			Percentage float64 `json:"percentage"`
			Estimated  bool    `json:"estimated"`
		} `json:"context"`
	}
	json.Unmarshal([]byte(body), &st)
	if code != 200 || st.Connection != "CONNECTED" || st.Session.Status != "ACTIVE" || st.Session.WorkingDirectory != "/repos/project-a" ||
		st.Context.Tokens != 48000 || st.Context.Window != 64000 || st.Context.Percentage != 75 || st.Context.Estimated {
		t.Fatalf("T12.81.1-T12.81.3 %d %s", code, body)
	}
	if r.workflowRows(t) != before {
		t.Fatal("12.71 no context-threshold workflow decision may occur")
	}
	evs := r.events(t)
	if !hasEvent(evs, worker.EventRequestStarted) || !hasEvent(evs, worker.EventRequestCompleted) || !hasEvent(evs, worker.EventContextObserved) {
		t.Fatalf("T12.53.3 %v", evs)
	}
	// T12.53.4 / T12.94.4: no prompt/response content or token in events.
	all := p10Rows(t, r.h, "worker_events", "project_id = '"+r.pos.ProjectID.String()+"'")
	if strings.Contains(all, "unit 11.3 implemented") || strings.Contains(all, wp.Content[:40]) {
		t.Fatal("event payload carries content")
	}
	p12NoSecret(t, "worker_events", all)
	p12NoSecret(t, "worker_dispatches", p10Rows(t, r.h, "worker_dispatches", "project_id = '"+r.pos.ProjectID.String()+"'"))
	p12NoSecret(t, "worker_sessions", p10Rows(t, r.h, "worker_sessions", "project_id = '"+r.pos.ProjectID.String()+"'"))
	// P12-69: the dispatch fed Phase 11's WORKER_DISPATCH trigger.
	r.svc().Wait()
	if n := p9Count(t, r.h, "precompute_jobs", "project_id = '"+r.pos.ProjectID.String()+"' AND trigger = 'WORKER_DISPATCH'"); n < 1 {
		t.Fatal("precompute not notified of the dispatch")
	}
}

func ptrS(s string) *string { return &s }
