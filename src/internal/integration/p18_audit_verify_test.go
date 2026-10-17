package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"overseer/internal/audit"
	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/service"
)

func p18AuditHistory(t *testing.T, r *p17Rig) string {
	t.Helper()
	var s *string
	if err := r.h.DBScan(t.Context(), `SELECT string_agg(id::text || COALESCE(event_hash, '-') || reason, ',' ORDER BY seq) || '|' ||
		(SELECT count(*) FROM audit_failure_acknowledgements)::text FROM audit_events`, &s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func p18Verify(t *testing.T, r *p17Rig, query string) service.AuditVerification {
	t.Helper()
	code, raw := r.b.api(http.MethodGet, "/api/v1/audit/verify"+query, nil)
	var v service.AuditVerification
	if code != http.StatusOK || json.Unmarshal(raw, &v) != nil {
		t.Fatalf("verify: %d %.300s", code, raw)
	}
	return v
}

func p18ChainResult(v service.AuditVerification, chain string) audit.ChainResult {
	for _, c := range v.Chains {
		if c.Chain == chain {
			return c
		}
	}
	return audit.ChainResult{}
}

// T18.108 / T18.109 / T18.110 / T18.230 + decision U9 — a valid history
// verifies VALID; verification never modifies it; a tampered row is
// INVALID at its exact chain_seq and a deleted row INCOMPLETE with the
// precise gap. Either failure blocks new work (no worker request is sent)
// until an operator acknowledges exactly that failure through the audited
// ACKNOWLEDGE_AUDIT_FAILURE action; a different failure blocks again.
func TestT18108AuditVerification(t *testing.T) {
	r := p17NewRig(t)
	ctx := context.Background()
	repo := dbrepo.NewAuditRepository(r.h.App.DB().Raw())
	pid := r.pid
	for i := 0; i < 5; i++ {
		if err := repo.AppendAudit(ctx, nil, p18AuditEvent(&pid, i)); err != nil {
			t.Fatal(err)
		}
		if err := repo.AppendAudit(ctx, nil, p18AuditEvent(nil, 100+i)); err != nil {
			t.Fatal(err)
		}
	}
	projectChain := audit.ChainKey(&pid)

	// 18.108 / 18.110: VALID, and verifying changes nothing.
	before := p18AuditHistory(t, r)
	if v := p18Verify(t, r, ""); v.Status != audit.ChainValid || v.BlocksNewWork || len(v.Chains) < 2 {
		t.Fatalf("valid history: %+v", v)
	}
	if v := p18Verify(t, r, "?project="+pid.String()); v.Status != audit.ChainValid || len(v.Chains) != 1 || v.Chains[0].Chain != projectChain {
		t.Fatalf("project scope: %+v", v)
	}
	if p18AuditHistory(t, r) != before {
		t.Fatal("verification modified the audit history")
	}
	if code, _ := r.b.api(http.MethodPost, "/api/v1/audit/verify/acknowledge", map[string]any{"reason": "nothing wrong"}); code != http.StatusConflict {
		t.Fatalf("acknowledging a VALID history: %d", code)
	}

	// 18.230: a tampered row (triggers bypassed, as an attacker with
	// database access would) is detected at its chain_seq.
	r.exec(t, `SET session_replication_role = replica;
		UPDATE audit_events SET reason = 'rewritten history' WHERE chain_key = '`+projectChain+`' AND chain_seq = 3;
		SET session_replication_role = origin`)
	v := p18Verify(t, r, "")
	pc := p18ChainResult(v, projectChain)
	if v.Status != audit.ChainInvalid || pc.Status != audit.ChainInvalid || pc.FirstFailing != 3 || !v.BlocksNewWork || v.Fingerprint == "" {
		t.Fatalf("tampering not located: %+v", v)
	}
	if p18ChainResult(v, audit.SystemChain).Status != audit.ChainValid {
		t.Fatal("an untouched chain was reported failing")
	}

	// U9: no new work while unacknowledged — the execution waits, nothing
	// is sent.
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	res, _ := r.eng().Run(ctx, r.execID, 20)
	if n := len(r.g.Messages()); n != 0 {
		t.Fatalf("%d worker requests sent while audit verification failed; %s", n, p16Trace(res))
	}
	var status, wait string
	_ = r.h.DBScan(ctx, "SELECT status, COALESCE(wait_reason, '') FROM executions WHERE id = '"+r.execID.String()+"'", &status, &wait)
	if status != string(execution.ExecutionWaiting) || !strings.Contains(wait, "new work blocked") || !strings.Contains(wait, "INVALID") {
		t.Fatalf("execution %s / %q", status, wait)
	}
	var sys struct {
		AuditVerification string   `json:"audit_verification"`
		NewWorkBlocked    []string `json:"new_work_blocked"`
	}
	if code, raw := r.b.api(http.MethodGet, "/api/v1/system/status", nil); code != 200 || json.Unmarshal(raw, &sys) != nil ||
		sys.AuditVerification != "INVALID" || len(sys.NewWorkBlocked) != 1 {
		t.Fatalf("system status: %d %s", code, raw)
	}
	if _, page := r.b.page("/audit"); !strings.Contains(page, "new work is blocked") {
		t.Fatal("the audit page does not show the failure")
	}

	// Acknowledgement: reason required; a stale fingerprint is refused; the
	// accepted one is audited and opens the gate. History is unchanged.
	if code, _ := r.b.api(http.MethodPost, "/api/v1/audit/verify/acknowledge", map[string]any{"reason": " "}); code != http.StatusBadRequest {
		t.Fatalf("blank reason: %d", code)
	}
	if code, _ := r.b.api(http.MethodPost, "/api/v1/audit/verify/acknowledge",
		map[string]any{"reason": "investigated", "fingerprint": strings.Repeat("0", 64)}); code != http.StatusConflict {
		t.Fatalf("stale fingerprint: %d", code)
	}
	code, raw := r.b.api(http.MethodPost, "/api/v1/audit/verify/acknowledge", map[string]any{"reason": "investigated: test tampering", "fingerprint": v.Fingerprint})
	if code != http.StatusOK {
		t.Fatalf("acknowledge: %d %s", code, raw)
	}
	if n := r.count(t, "SELECT count(*) FROM audit_events WHERE action = 'ACKNOWLEDGE_AUDIT_FAILURE' AND result = 'SUCCEEDED'"); n != 1 {
		t.Fatalf("acknowledgement audited %d times", n)
	}
	if after := p18Verify(t, r, ""); after.Status != audit.ChainInvalid || !after.Acknowledged || after.BlocksNewWork {
		t.Fatalf("after acknowledgement: %+v", after)
	}
	if b := r.h.App.WorkGate().Blocked(); b != "" {
		t.Fatalf("gate still closed: %s", b)
	}
	r.runUntil(t, execution.StepCompleted, 200)

	// 18.109: a deleted row is a different failure — INCOMPLETE with the
	// precise gap — and blocks new work again.
	r.exec(t, `SET session_replication_role = replica;
		DELETE FROM audit_events WHERE chain_key = 'system' AND chain_seq = 2;
		SET session_replication_role = origin`)
	v2 := p18Verify(t, r, "")
	sc := p18ChainResult(v2, audit.SystemChain)
	if sc.Status != audit.ChainIncomplete || len(sc.Gaps) != 1 || sc.Gaps[0] != (audit.Gap{From: 2, To: 2}) || sc.FirstFailing != 2 {
		t.Fatalf("deleted row: %+v", sc)
	}
	if v2.Status != audit.ChainInvalid || !v2.BlocksNewWork || v2.Fingerprint == v.Fingerprint {
		t.Fatalf("a new failure must block again: %+v", v2)
	}
	if r.h.App.WorkGate().Blocked() == "" {
		t.Fatal("gate open after a new failure")
	}

}
