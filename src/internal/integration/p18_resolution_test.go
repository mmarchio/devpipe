package integration

import (
	"context"
	"net/http"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
)

// p18DeliveryUnknown drives an execution into DELIVERY_UNKNOWN recovery.
func p18DeliveryUnknown(t *testing.T) *p17Rig {
	t.Helper()
	r := p17NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.g.Script(hermestest.Behavior{Drop: true})
	r.runUntil(t, execution.StepRecoveryRequired, 50)
	return r
}

func (r *p17Rig) recoveryAPI(t *testing.T, body map[string]any) (int, string) {
	t.Helper()
	code, raw := r.b.api(http.MethodPost, "/api/v1/projects/"+r.pid.String()+"/executions/"+r.execID.String()+"/recovery/actions", body)
	return code, string(raw)
}

// T18.23 / Decision 8 — validation-assisted reconciliation supplies
// evidence without changing the delivery status; ACCEPT_REPOSITORY_STATE
// routes to deterministic validation without resending; the resolution
// applies once.
func TestT1823ValidationAssistedAccept(t *testing.T) {
	r := p18DeliveryUnknown(t)
	sends := len(r.g.Messages())
	code, raw := r.recoveryAPI(t, map[string]any{"action": "VALIDATE_REPOSITORY_STATE", "idempotency_key": "t1823-validate-1"})
	if code != 200 || r.execStatus(t) != "RECOVERY_REQUIRED" {
		t.Fatalf("validate: %d %s (%s)", code, raw, r.execStatus(t))
	}
	if r.count(t, "SELECT count(*) FROM validation_runs WHERE label LIKE 'recovery evidence:%' AND status = 'PASS'") != 1 {
		t.Fatal("evidence run not recorded")
	}
	if r.count(t, "SELECT count(*) FROM worker_dispatches WHERE delivery = 'DELIVERY_UNKNOWN'") != 1 {
		t.Fatal("delivery status rewritten")
	}
	code, raw = r.recoveryAPI(t, map[string]any{"action": "RESOLVE_DELIVERY", "resolution": "ACCEPT_REPOSITORY_STATE", "confirm": true,
		"reason": "repository holds the work; validate it", "idempotency_key": "t1823-accept-1"})
	if code != 200 {
		t.Fatalf("accept: %d %s", code, raw)
	}
	r.runUntil(t, execution.StepCompleted, 100)
	if len(r.g.Messages()) != sends {
		t.Fatal("ACCEPT resent the request")
	}
	if r.count(t, "SELECT count(*) FROM worker_dispatches WHERE delivery = 'DELIVERY_UNKNOWN'") != 1 ||
		r.count(t, "SELECT count(*) FROM dispatch_resolutions WHERE consumed_at IS NOT NULL") != 1 {
		t.Fatal("history / resolution")
	}
	// The unit is DONE only through deterministic validation.
	if r.count(t, "SELECT count(*) FROM work_units w JOIN validation_runs v ON v.id = w.done_validation_run_id WHERE w.status = 'DONE' AND v.status = 'PASS'") != 1 {
		t.Fatal("DONE without validation")
	}
	// A second resolution of the same dispatch is refused.
	if code, _ := r.recoveryAPI(t, map[string]any{"action": "RESOLVE_DELIVERY", "resolution": "RESEND_AFTER_REVIEW", "confirm": true,
		"reason": "again", "idempotency_key": "t1823-accept-2"}); code == 200 {
		t.Fatal("second resolution accepted")
	}
}

// Decision 8 — RESEND_AFTER_REVIEW sends exactly one new attempt, even
// when the engine re-runs.
func TestP1832ResendAfterReview(t *testing.T) {
	r := p18DeliveryUnknown(t)
	sends := len(r.g.Messages())
	r.queue("complete")
	code, raw := r.recoveryAPI(t, map[string]any{"action": "RESOLVE_DELIVERY", "resolution": "RESEND_AFTER_REVIEW", "confirm": true,
		"reason": "gateway logs show the request never reached the model", "idempotency_key": "p1832-resend-1"})
	if code != 200 {
		t.Fatalf("resend: %d %s", code, raw)
	}
	r.runUntil(t, execution.StepCompleted, 100)
	for i := 0; i < 2; i++ {
		_, _ = r.eng().Run(context.Background(), r.execID, 10)
	}
	if got := len(r.g.Messages()) - sends; got != 1 {
		t.Fatalf("resent %d times", got)
	}
	if r.count(t, "SELECT count(*) FROM worker_dispatches WHERE delivery = 'DELIVERY_UNKNOWN'") != 1 {
		t.Fatal("original dispatch rewritten")
	}
}
