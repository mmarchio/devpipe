package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"overseer/internal/gateway/hermes"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
	"overseer/internal/worker"
)

func p12JSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return m
}

// P12-71..P12-75 (WBS 12.81-12.86): the worker REST surface.
func TestP1281WorkerAPI(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	w := r.base + "/worker"

	// No session yet: clean empty read model (T12.86.4, T12.51.3).
	code, body, _ := p9HTTP(t, http.MethodGet, w, "")
	m := p12JSON(t, body)
	if code != 200 || m["session"] != nil || m["current_request"] != nil || m["context"].(map[string]any)["known"] != false || m["provider"] != "HERMES_GATEWAY" {
		t.Fatalf("empty worker view %d %s", code, body)
	}

	// POST session/new twice (12.84): creation only.
	code, body, _ = p9HTTP(t, http.MethodPost, w+"/session/new", "")
	s1 := p12JSON(t, body)
	if code != 201 || s1["status"] != "ACTIVE" || s1["working_directory"] != nil {
		t.Fatalf("T12.84.1/T12.84.3 %d %s", code, body)
	}
	code, body, _ = p9HTTP(t, http.MethodPut, w+"/session/cwd", `{"path":"/repos/project-a"}`)
	if code != 200 || p12JSON(t, body)["working_directory"] != "/repos/project-a" {
		t.Fatalf("T12.85.1 %d %s", code, body)
	}
	code, body, _ = p9HTTP(t, http.MethodPost, w+"/session/new", "")
	s2 := p12JSON(t, body)
	if code != 201 || s2["working_directory"] != nil || s2["id"] == s1["id"] {
		t.Fatalf("second session %d %s", code, body)
	}
	if len(r.g.Messages()) != 0 {
		t.Fatal("T12.84.4 session/new dispatched work")
	}
	// PUT cwd failure (12.85.2/12.85.3): metadata unchanged.
	r.g.FailCD("/repos/missing")
	code, body, _ = p9HTTP(t, http.MethodPut, w+"/session/cwd", `{"path":"/repos/missing"}`)
	if code < 400 || !strings.Contains(body, "worker_invalid_request") {
		t.Fatalf("T12.85.2 %d %s", code, body)
	}
	if code, body, _ = p9HTTP(t, http.MethodGet, w+"/sessions/"+s2["id"].(string), ""); p12JSON(t, body)["working_directory"] != nil {
		t.Fatal("T12.85.3 failed cd changed metadata")
	}
	if code, _, _ = p9HTTP(t, http.MethodPut, w+"/session/cwd", `{"path":""}`); code != 400 {
		t.Fatal("empty path accepted")
	}
	if code, _, _ = p9HTTP(t, http.MethodPut, w+"/session/cwd", `{"path":"/x","extra":1}`); code != 400 {
		t.Fatal("unknown field accepted")
	}
	p9HTTP(t, http.MethodPut, w+"/session/cwd", `{"path":"/repos/project-a"}`)

	// Sessions chronological (12.82).
	code, body, _ = p9HTTP(t, http.MethodGet, w+"/sessions", "")
	ss := p12JSON(t, body)["sessions"].([]any)
	if code != 200 || len(ss) != 2 || ss[0].(map[string]any)["status"] != "RESET" || ss[1].(map[string]any)["status"] != "ACTIVE" {
		t.Fatalf("T12.82.1/T12.82.2 %s", body)
	}

	// Current request represented while in flight (12.86.1).
	wp := r.prompt(t, "")
	hold := make(chan struct{})
	r.g.Script(hermestest.Behavior{Hold: hold})
	done := make(chan *service.DispatchResult, 1)
	go func() {
		res, _ := r.ws().Dispatch(ctx, r.req(wp, 1))
		done <- res
	}()
	for len(r.g.Messages()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	_, body, _ = p9HTTP(t, http.MethodGet, w, "")
	cur, _ := p12JSON(t, body)["current_request"].(map[string]any)
	if cur == nil || cur["role"] != "ENGINEER" || cur["state"] != "IMPLEMENT" || cur["work_unit_id"] != r.units["11.3"].String() {
		t.Fatalf("T12.86.1 %s", body)
	}
	if !strings.Contains(body, `"request_executing":true`) {
		t.Fatal("T12.48.3 request liveness")
	}
	close(hold)
	d1 := <-done
	d2 := r.dispatch(t, wp, 2)

	// Dispatch history + filters (12.83).
	code, body, _ = p9HTTP(t, http.MethodGet, w+"/dispatches", "")
	if code != 200 || p12JSON(t, body)["total"].(float64) != 2 {
		t.Fatalf("T12.83.1 %s", body)
	}
	for q, want := range map[string]float64{"?status=COMPLETED": 2, "?state=IMPLEMENT&role=ENGINEER": 2, "?state=VALIDATE": 0,
		"?work_unit=" + r.units["11.4"].String(): 0, "?work_unit=" + r.units["11.3"].String(): 2, "?session=" + s1["id"].(string): 0,
		"?session=" + s2["id"].(string) + "&limit=1": 2, "?phase=" + r.pos.PhaseID.String(): 2} {
		_, b, _ := p9HTTP(t, http.MethodGet, w+"/dispatches"+q, "")
		if got := p12JSON(t, b)["total"].(float64); got != want {
			t.Fatalf("T12.83.2 %s → %v", q, got)
		}
	}
	if code, _, _ = p9HTTP(t, http.MethodGet, w+"/dispatches?status=BOGUS", ""); code != 400 {
		t.Fatal("invalid filter accepted")
	}
	code, body, _ = p9HTTP(t, http.MethodGet, w+"/dispatches/"+d2.Dispatch.ID.String(), "")
	det := p12JSON(t, body)
	if code != 200 || len(det["attempts"].([]any)) != 2 || det["response"] == nil {
		t.Fatalf("T12.83.3 %s", body)
	}
	if strings.Contains(body, wp.Content[:60]) || strings.Contains(body, d1.Response.Content) {
		t.Fatal("T12.83.4 raw prompt or response content exposed")
	}

	// Metrics.
	_, body, _ = p9HTTP(t, http.MethodGet, w+"/metrics", "")
	mt := p12JSON(t, body)
	if mt["successful"].(float64) != 2 || mt["sessions_created"].(float64) != 2 || mt["session_resets"].(float64) != 1 {
		t.Fatalf("metrics %s", body)
	}

	// Project isolation (T12.81.4, T12.82.3).
	_, pb, _ := p9HTTP(t, http.MethodPost, r.h.BaseURL+"/api/v1/projects", `{"name":"p12-other","description":"x"}`)
	other := r.h.BaseURL + "/api/v1/projects/" + p12JSON(t, pb)["id"].(string) + "/worker"
	if code, _, _ = p9HTTP(t, http.MethodGet, other+"/sessions/"+s2["id"].(string), ""); code != 404 {
		t.Fatal("T12.82.3 cross-project session visible")
	}
	if code, _, _ = p9HTTP(t, http.MethodGet, other+"/dispatches/"+d2.Dispatch.ID.String(), ""); code != 404 {
		t.Fatal("cross-project dispatch visible")
	}
	if code, _, _ = p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/projects/00000000-0000-0000-0000-000000000009/worker", ""); code != 404 {
		t.Fatal("T12.81.4 unknown project")
	}
	if code, _, _ = p9HTTP(t, http.MethodPost, other+"/session/new", ""); code != 409 {
		t.Fatal("unconfigured project must report worker_not_configured")
	}
	if code, _, _ = p9HTTP(t, http.MethodDelete, w+"/metrics", ""); code != 405 {
		t.Fatal("write method on read endpoint")
	}
}

// P12-84 (WBS 12.98): fresh project session — Connect → NewSession →
// ChangeDirectory → Send(WP1), nothing else.
func TestP1298FreshSession(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	wp := r.prompt(t, "")
	before := r.workflowRows(t)
	s := r.newSession(t)
	r.cd(t, "")
	res := r.dispatch(t, wp, 1)
	h, _ := r.ws().Health(ctx, r.pos.ProjectID)
	if h.ConnectionState != worker.ConnConnected || r.session(t, s.ID).Status != worker.SessionActive || *r.session(t, s.ID).WorkingDirectory != "/repos/project-a" {
		t.Fatal("gateway / session / cwd")
	}
	if res.Dispatch.PromptHash != wp.Hash || res.Dispatch.Status != worker.DispatchCompleted || res.Response == nil || res.Usage == nil {
		t.Fatal("dispatch / response / usage")
	}
	if raw, _ := r.ws().Response(ctx, r.pos.ProjectID, res.Dispatch.ID); raw.Content != "OK "+res.Dispatch.RequestID.String() {
		t.Fatal("raw response not returned")
	}
	if r.workflowRows(t) != before {
		t.Fatal("workflow transition performed")
	}
	// The handshake, the session-start "cd" prompt, the message.
	want := []string{"GET /v1/models", "POST /v1/chat/completions", "POST /v1/chat/completions"}
	if got := r.g.Paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("only the explicit Phase 12 operations: %v", got)
	}
}

// P12-84 (WBS 12.99): the caller (not Phase 12 policy) resets a high-
// context session; the reconstruction prompt goes out only after cd.
func TestP1299SessionReset(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	s1 := r.newSession(t)
	r.cd(t, "/repos/project-a")
	r.g.SetContext(s1.ProviderSessionID, 60000)
	r.g.Script(hermestest.Behavior{Usage: &hermestest.Usage{ContextTokens: hermestest.I64(61000), ContextWindow: hermestest.I64(64000)}})
	r.dispatch(t, r.prompt(t, ""), 1)
	if cp, _ := r.ws().ContextProgress(ctx, r.pos.ProjectID); cp.Tokens != 61000 {
		t.Fatal("context")
	}
	s2 := r.newSession(t)
	pathsAfterNew := len(r.g.Paths())
	// The reconstruction prompt cannot go out before S2's CWD is confirmed.
	r2 := r.prompt(t, "reconstruct")
	if _, err := r.ws().Dispatch(ctx, r.req(r2, 1)); !errors.Is(err, service.ErrWorkingDirectoryNotReady) {
		t.Fatalf("dispatch before cd: %v", err)
	}
	if len(r.g.MessagesFor(s2.ProviderSessionID)) != 0 || p9Count(t, r.h, "worker_dispatches", "session_id = '"+s2.ID.String()+"'") != 0 {
		t.Fatal("prompt sent or persisted before cd")
	}
	rd, _ := r.ws().Readiness(ctx, r.pos.ProjectID)
	if rd.WorkingDirectoryReady {
		t.Fatal("S2 ready before cd")
	}
	r.cd(t, "/repos/project-a")
	if rd, _ := r.ws().Readiness(ctx, r.pos.ProjectID); !rd.Ready() {
		t.Fatal("S2 not ready after cd")
	}
	res := r.dispatch(t, r2, 1)
	if r.session(t, s1.ID).Status != worker.SessionReset || r.session(t, s2.ID).Status != worker.SessionActive || res.Dispatch.SessionID != s2.ID {
		t.Fatal("S1 RESET / S2 ACTIVE")
	}
	if msgs := r.g.MessagesFor(s2.ProviderSessionID); len(msgs) != 1 || msgs[0].Messages[0].Content != hermes.ChangeDirectoryPrompt("/repos/project-a") {
		t.Fatalf("reconstruction prompt without the cd: %+v (paths %v)", msgs, r.g.Paths()[pathsAfterNew:])
	}
	if hist, _ := r.ws().Sessions(ctx, r.pos.ProjectID); len(hist) != 2 {
		t.Fatal("session histories")
	}
}

// P12-84 (WBS 12.100): gateway fails before the next send → reconnect →
// reconcile → S1 confirmed → the request is sent once.
func TestP12100GatewayFailureRecovery(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	s1 := r.newSession(t)
	r.cd(t, "/repos/project-a")
	wp := r.prompt(t, "")
	r.proxy.Stop()
	failed := r.dispatch(t, wp, 1)
	if failed.Dispatch.Status != worker.DispatchFailed || *failed.Dispatch.Delivery != worker.DeliveryNotSent {
		t.Fatalf("pre-send failure %+v", failed.Err)
	}
	r.proxy.Start(t)
	rec, err := r.ws().Reconnect(ctx, r.pos.ProjectID)
	if err != nil || rec != worker.ReconcileConfirmed {
		t.Fatalf("reconcile %v %v", rec, err)
	}
	res := r.dispatch(t, wp, 2)
	if res.Dispatch.Status != worker.DispatchCompleted || res.Dispatch.SessionID != s1.ID || p12Sends(r.g, res.Dispatch.RequestID) != 1 || len(r.g.Messages()) != 1 {
		t.Fatal("request not sent exactly once on the retained session")
	}
	if !strings.Contains(res.Response.Content, res.Dispatch.RequestID.String()) {
		t.Fatal("response not correlated")
	}
	if len(r.g.Sessions()) != 1 {
		t.Fatal("unnecessary new session")
	}
	evs := r.events(t)
	if !hasEvent(evs, worker.EventDisconnected) || !hasEvent(evs, worker.EventReconnected) {
		t.Fatalf("connection history %v", evs)
	}
	m, _ := r.ws().Metrics(ctx, r.pos.ProjectID)
	if m.Reconnections < 1 || m.Failed != 1 || m.Successful != 1 {
		t.Fatalf("metrics %+v", m)
	}
}
