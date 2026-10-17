package integration

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/readmodel"
)

// P17-43 / P17-60 / P17-68 / P17-117 / P17-137 (WBS 17.96–17.100, 17.172,
// 17.189) — an unknown delivery surfaces RECOVERY REQUIRED with evidence and
// only backend-approved actions; no blind retry exists; high-impact actions
// need confirmation + reason (audited); nothing is ever resent.
func TestP17137RecoveryDeliveryUnknown(t *testing.T) {
	r := p17NewRig(t)
	pid, eid := r.pid.String(), r.execID.String()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.g.Script(hermestest.Behavior{Drop: true})
	r.runUntil(t, execution.StepRecoveryRequired, 50)
	sends := len(r.g.Messages())

	var rec readmodel.Recovery
	if code := r.b.getJSON("/api/v1/projects/"+pid+"/executions/"+eid+"/recovery", &rec); code != 200 {
		t.Fatal(code)
	}
	if !rec.Required || !rec.AutomaticStopped || !strings.Contains(rec.Reason, "delivery unknown") || rec.Dispatch == nil ||
		rec.Dispatch.DeliveryTag != "DELIVERY UNKNOWN (not resent)" || rec.PromptHash == "" {
		t.Fatalf("recovery: %+v", rec)
	}
	var kinds []string
	for _, a := range rec.Actions {
		kinds = append(kinds, a.Kind)
	}
	// Phase 18 (Decision 8): reviewed resolution instead of MANUAL_RESUME.
	if strings.Join(kinds, ",") != "RECONCILE_DISPATCH,VALIDATE_REPOSITORY_STATE,RESOLVE_DELIVERY" {
		t.Fatalf("actions: %v", kinds)
	}
	// Overview: normal controls restricted (17.96 / 17.172).
	var ov readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	if !ov.Recovery || ov.HasAction("START") || ov.HasAction("RESUME") || ov.HasAction("PAUSE") || ov.HasAction("STEP") || !ov.HasAction("RESOLVE_RECOVERY") {
		t.Fatalf("overview actions: %v", ov.Actions)
	}
	// Pages: prominent banner everywhere, evidence, no blind retry.
	body := r.b.mustPage("/projects/" + pid)
	if !strings.Contains(body, "RECOVERY REQUIRED") {
		t.Fatal("overview lacks the recovery banner")
	}
	body = r.b.mustPage("/projects/" + pid + "/executions/" + eid + "/recovery")
	text := textOf(body)
	for _, want := range []string{"RECOVERY REQUIRED", "Reconcile Dispatch", "Resolve Unknown Delivery", "DELIVERY UNKNOWN", "Evidence"} {
		if !strings.Contains(text, want) {
			t.Fatalf("recovery page lacks %q", want)
		}
	}
	for _, m := range regexp.MustCompile(`<button[^>]*>([^<]*)</button>`).FindAllStringSubmatch(body, -1) {
		if strings.Contains(strings.ToLower(m[1]), "retry") {
			t.Fatalf("blind retry button offered: %q", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`name="action" value="([A-Z_]+)"`).FindAllStringSubmatch(body, -1) {
		if m[1] != "RECONCILE_DISPATCH" && m[1] != "VALIDATE_REPOSITORY_STATE" && m[1] != "RESOLVE_DELIVERY" {
			t.Fatalf("unapproved action form %s", m[1])
		}
	}
	// Actions not offered are refused (no resend path exists at all).
	base := "/api/v1/projects/" + pid + "/executions/" + eid + "/recovery/actions"
	if code, raw := r.b.api(http.MethodPost, base, map[string]any{"action": "RUN_VALIDATION"}); code != 409 || !strings.Contains(string(raw), "ACTION_NOT_AVAILABLE") {
		t.Fatalf("unoffered action: %d %s", code, raw)
	}
	// High impact without confirmation / reason: rejected (page form too).
	if code, _ := r.b.api(http.MethodPost, base, map[string]any{"action": "RESOLVE_DELIVERY", "reason": "x", "resolution": "ACCEPT_REPOSITORY_STATE"}); code != 400 {
		t.Fatalf("unconfirmed: %d", code)
	}
	if code, _, _ := r.b.form("/projects/"+pid+"/executions/"+eid+"/recovery", url.Values{"action": {"RESOLVE_DELIVERY"}}); code != 400 {
		t.Fatalf("unconfirmed form: %d", code)
	}
	// Reconcile (Phase 12 reconciliation only; never sends).
	if code, raw := r.b.api(http.MethodPost, base, map[string]any{"action": "RECONCILE_DISPATCH", "reason": "check gateway",
		"idempotency_key": "p17137-reconcile-1"}); code != 200 {
		t.Fatalf("reconcile: %d %s", code, raw)
	}
	if len(r.g.Messages()) != sends {
		t.Fatal("reconciliation sent a request")
	}
	// Confirmed, reviewed resolution through the page form, with a reason.
	code, _, final := r.b.form("/projects/"+pid+"/executions/"+eid+"/recovery",
		url.Values{"action": {"RESOLVE_DELIVERY"}, "resolution": {"ACCEPT_REPOSITORY_STATE"}, "confirm": {"yes"},
			"reason": {"gateway logs show no delivery"}, "idempotency_key": {"p17137-resolve-1"}})
	if code != 200 || !strings.HasSuffix(final, "/recovery") {
		t.Fatalf("resume form: %d %s", code, final)
	}
	if n := r.count(t, "SELECT count(*) FROM audit_events WHERE action = 'RECOVERY_ACTION' AND result = 'SUCCEEDED' AND reason = 'gateway logs show no delivery'"); n != 1 {
		t.Fatalf("recovery audit rows = %d", n)
	}
	// The engine continues without resending the ambiguous request
	// (WBS 17.99): ACCEPT_REPOSITORY_STATE routes to validation.
	_, _ = r.eng().Run(context.Background(), r.execID, 50)
	if len(r.g.Messages()) != sends {
		t.Fatal("unknown delivery resent after an ACCEPT resolution")
	}
}
