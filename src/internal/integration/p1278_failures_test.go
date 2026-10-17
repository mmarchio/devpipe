package integration

import (
	"context"
	"testing"
	"time"

	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/persistence"
	"overseer/internal/worker"
)

// P12-78 (WBS 12.72, 12.89, 12.101): request transmitted, connection
// drops, no response. The ENGINEER prompt may already have modified the
// repository: UNKNOWN / DELIVERY_UNKNOWN, exactly one send, recovery
// delegated upward, workflow untouched, audit evidence kept.
func TestP1272DeliveryUnknown(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	s := r.newSession(t)
	r.cd(t, "/repos/project-a")
	wp := r.prompt(t, "")
	before := r.workflowRows(t)
	r.g.Script(hermestest.Behavior{Drop: true})
	res := r.dispatch(t, wp, 1)
	d := res.Dispatch
	if d.Status != worker.DispatchUnknown || *d.Delivery != worker.DeliveryUnknown || *d.RetryClass != worker.RetryRecovery {
		t.Fatalf("T12.89.2 %+v", d)
	}
	if res.Err == nil || res.Err.RetryClass() != worker.RetryRecovery || res.Err.Retryable {
		t.Fatal("T12.89.3 recovery not delegated upward")
	}
	if n := p12Sends(r.g, d.RequestID); n != 1 || len(r.g.Messages()) != 1 {
		t.Fatalf("T12.89.1 prompt sent %d times", n)
	}
	if _, err := r.ws().Response(ctx, r.pos.ProjectID, d.ID); err == nil {
		t.Fatal("fake response persisted")
	}
	if r.workflowRows(t) != before {
		t.Fatal("12.101 workflow state changed")
	}
	if !hasEvent(r.events(t), worker.EventDeliveryUnknown) {
		t.Fatal("12.72 audit event missing")
	}
	// The same attempt cannot be re-dispatched silently (idempotency).
	if _, err := r.ws().Dispatch(ctx, r.req(wp, 1)); err == nil {
		t.Fatal("T12.33.1 duplicate attempt accepted")
	}
	// Session reconciliation is a separate, explicit step; it confirms S1
	// and still sends nothing.
	rec, err := r.ws().Reconnect(ctx, r.pos.ProjectID)
	if err != nil || rec != worker.ReconcileConfirmed || len(r.g.Messages()) != 1 {
		t.Fatalf("12.101 reconciliation: %v %v", rec, err)
	}
	if active, _ := r.ws().ActiveSession(ctx, r.pos.ProjectID); active.ID != s.ID {
		t.Fatal("session replaced")
	}
	// A deliberate new attempt is allowed (T12.33.2) and keeps history
	// (T12.58.2).
	res2 := r.dispatch(t, wp, 2)
	att, _ := r.ws().GetDispatch(ctx, r.pos.ProjectID, res2.Dispatch.ID)
	if res2.Dispatch.Status != worker.DispatchCompleted || len(att.Attempts) != 2 || att.Attempts[0].Status != worker.DispatchUnknown ||
		att.Attempts[0].OperationID != att.Attempts[1].OperationID {
		t.Fatalf("T12.58.1-T12.58.3 attempts %+v", att.Attempts)
	}
}

// P12-78 (WBS 12.73): connection fails before transmission.
func TestP1273PreSendFailure(t *testing.T) {
	r := p12NewRig(t)
	r.newSession(t)
	r.cd(t, "/repos/project-a")
	wp := r.prompt(t, "")
	r.proxy.Stop()
	res := r.dispatch(t, wp, 1)
	d := res.Dispatch
	if d.Status != worker.DispatchFailed || *d.Delivery != worker.DeliveryNotSent || *d.RetryClass != worker.RetrySafe || !res.Err.Retryable {
		t.Fatalf("12.73 %+v / %+v", d, res.Err)
	}
	if len(r.g.Messages()) != 0 {
		t.Fatal("request reached the gateway")
	}
	if n := p9Count(t, r.h, "worker_responses", "dispatch_id = '"+d.ID.String()+"'"); n != 0 {
		t.Fatal("response persisted")
	}
	if !hasEvent(r.events(t), worker.EventDisconnected) {
		t.Fatal("disconnect not recorded")
	}
	// A further operation reconnects and fails: audited, nothing created.
	if _, err := r.ws().NewSession(context.Background(), r.pos.ProjectID); err == nil {
		t.Fatal("new session against an unreachable gateway")
	}
	if evs := r.events(t); !hasEvent(evs, worker.EventConnectionFailed) {
		t.Fatalf("connection failure not audited: %v", evs)
	}
	if m, _ := r.ws().Metrics(context.Background(), r.pos.ProjectID); m.ConnectionFailures < 1 {
		t.Fatal("T12.54 connection failures not counted")
	}
}

// P12-79 (WBS 12.74, 12.61): Hermes reports S1 does not exist.
func TestP1274SessionLost(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	s := r.newSession(t)
	r.cd(t, "/repos/project-a")
	wp := r.prompt(t, "")
	before := r.workflowRows(t)
	r.g.KillSession(s.ProviderSessionID)
	gwSessions := len(r.g.Sessions())
	res := r.dispatch(t, wp, 1)
	if res.Err == nil || res.Err.Code != worker.CodeSessionLost || res.Dispatch.Status != worker.DispatchFailed {
		t.Fatalf("T12.61.1 %+v", res.Err)
	}
	if got := r.session(t, s.ID); got.Status != worker.SessionLost {
		t.Fatalf("T12.61.2 S1 = %s", got.Status)
	}
	if _, err := r.ws().ActiveSession(ctx, r.pos.ProjectID); err == nil || len(r.g.Sessions()) != gwSessions {
		t.Fatal("T12.61.3 replacement session created")
	}
	if r.workflowRows(t) != before {
		t.Fatal("workflow changed")
	}
	if !hasEvent(r.events(t), worker.EventSessionLost) {
		t.Fatal("SESSION_LOST event")
	}
	h, _ := r.ws().Health(ctx, r.pos.ProjectID)
	if h.SessionState == nil || *h.SessionState != worker.SessionLost || h.LastError == nil || h.LastError.Code != worker.CodeSessionLost {
		t.Fatalf("T12.49.2 %+v", h)
	}
	// Later dispatches fail clearly; nothing is created implicitly.
	if _, err := r.ws().Dispatch(ctx, r.req(wp, 2)); err == nil {
		t.Fatal("dispatch without an active session")
	}
}

// P12-83 (WBS 12.87 + 12.88): content and telemetry never drive workflow.
func TestP1287NoWorkflowAuthority(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	s := r.newSession(t)
	r.cd(t, "/repos/project-a")
	wp := r.prompt(t, "")
	r.svc().Wait()
	before := r.workflowRows(t)
	r.g.Script(hermestest.Behavior{Content: ptrS(`{"status":"complete"}`), Usage: &hermestest.Usage{ContextTokens: hermestest.I64(63000), ContextWindow: hermestest.I64(64000)}})
	res := r.dispatch(t, wp, 1)
	time.Sleep(200 * time.Millisecond)
	r.svc().Wait()
	if r.workflowRows(t) != before {
		t.Fatal("T12.87.1/T12.87.2 work unit or runtime changed")
	}
	ds, total, _ := r.ws().Dispatches(ctx, persistence.DispatchFilter{ProjectID: r.pos.ProjectID})
	if total != 1 || len(ds) != 1 || len(r.g.Messages()) != 1 {
		t.Fatal("T12.87.3 another dispatch occurred")
	}
	raw, err := r.ws().Response(ctx, r.pos.ProjectID, res.Dispatch.ID)
	if err != nil || raw.Content != `{"status":"complete"}` {
		t.Fatal("T12.87.4 response not available to Phase 13")
	}
	// 12.88: 63,000 / 64,000 reported, no "new" issued.
	u, _ := res.Usage.Usage.Utilization()
	if u.Percent != 98.44 {
		t.Fatalf("T12.88.2 usage %v", u.Percent)
	}
	active, _ := r.ws().ActiveSession(ctx, r.pos.ProjectID)
	sessions, _ := r.ws().Sessions(ctx, r.pos.ProjectID)
	if active.ID != s.ID || len(sessions) != 1 || len(r.g.Sessions()) != 1 {
		t.Fatal("T12.88.1 session changed autonomously")
	}
	for _, p := range r.g.Paths() {
		if p == "POST /v1/sessions" && len(r.g.Sessions()) != 1 {
			t.Fatal("adapter issued new")
		}
	}
	cp, _ := r.ws().ContextProgress(ctx, r.pos.ProjectID)
	if cp.Percent != 98.44 {
		t.Fatal("T12.88.3 usage not available to the later context state machine")
	}
}
