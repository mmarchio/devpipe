package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
	"overseer/internal/worker"
)

// P12-85 (WBS 12.103 + DoD): Phase 12 acceptance.
func TestP12103Acceptance(t *testing.T) {
	r, boot := p12RestartRig(t)
	ctx := context.Background()
	pid := r.pos.ProjectID.String()
	wpA := r.prompt(t, "")
	wpB := r.prompt(t, "B")
	before := r.workflowRows(t)

	// Create worker → Connect → Capabilities.
	caps, err := r.ws().Capabilities(ctx, r.pos.ProjectID)
	if err != nil || caps.Provider != worker.ProviderHermesGateway || !caps.Sessions || !caps.NewSession || !caps.ChangeDirectory || !caps.ContextTelemetry {
		t.Fatalf("capabilities %+v %v", caps, err)
	}
	if err := r.ws().ValidateConfiguration(ctx, r.pos.ProjectID, worker.WorkflowRequirements{NewSession: true, ChangeDirectory: true, ContextTelemetry: true}); err != nil {
		t.Fatalf("configuration: %v", err)
	}
	if r.ws().ConnectionState(r.pos.ProjectID) != worker.ConnConnected {
		t.Fatal("not connected")
	}

	// S1 → cd → Prompt A → Response A → context.
	s1 := r.newSession(t)
	r.cd(t, "/repos/a")
	resA := r.dispatch(t, wpA, 1)
	if resA.Dispatch.Status != worker.DispatchCompleted || resA.Usage == nil || resA.Usage.Source != worker.SourceProviderActual {
		t.Fatal("prompt A")
	}

	// S2 → cd → Prompt B (63k context reported: no autonomous reset).
	s2 := r.newSession(t)
	if s1.ProviderSessionID == s2.ProviderSessionID {
		t.Fatal("S1 and S2 share a provider session id")
	}
	got1 := r.session(t, s1.ID)
	if got1.Status != worker.SessionReset || got1.ClosedAt == nil || got1.ClosedAt.Before(r.session(t, s2.ID).CreatedAt) {
		t.Fatal("S1 must become RESET only once S2 exists")
	}
	if s2.WorkingDirectory != nil {
		t.Fatal("S2 inherited S1's working directory")
	}
	r.cd(t, "/repos/a")
	r.g.Script(hermestest.Behavior{Usage: &hermestest.Usage{ContextTokens: hermestest.I64(63000), ContextWindow: hermestest.I64(64000)}})
	resB := r.dispatch(t, wpB, 1)
	if resB.Dispatch.Status != worker.DispatchCompleted || resB.Dispatch.SessionID != s2.ID {
		t.Fatal("prompt B")
	}
	if ss, _ := r.ws().Sessions(ctx, r.pos.ProjectID); len(ss) != 2 || len(r.g.Sessions()) != 2 {
		t.Fatal("context percentage caused an autonomous reset")
	}

	// Provider errors map to generic worker errors.
	r.g.Script(hermestest.Behavior{Status: 429, ErrorCode: "rate_limited", RetryAfter: 9})
	rl := r.dispatch(t, wpB, 2)
	if rl.Err == nil || rl.Err.Code != worker.CodeRateLimited || *rl.Dispatch.RetryAfter != 9*time.Second || *rl.Dispatch.RetryClass != worker.RetrySafe {
		t.Fatalf("rate limit mapping %+v", rl.Err)
	}
	// An ambiguous request is never blindly resent.
	r.g.Script(hermestest.Behavior{Drop: true})
	amb := r.dispatch(t, wpB, 3)
	if amb.Dispatch.Status != worker.DispatchUnknown || p12Sends(r.g, amb.Dispatch.RequestID) != 1 {
		t.Fatal("ambiguous request")
	}

	// Both prompts were hash-verified: a tampered prompt never leaves.
	if _, err := r.h.DBExec(ctx, "ALTER TABLE worker_prompts DISABLE TRIGGER worker_prompts_immutable"); err != nil {
		t.Fatal(err)
	}
	r.h.DBExec(ctx, "UPDATE worker_prompts SET content = content || ' tampered' WHERE id = '"+wpA.ID.String()+"'")
	r.h.DBExec(ctx, "ALTER TABLE worker_prompts ENABLE TRIGGER worker_prompts_immutable")
	sends := len(r.g.Messages())
	if _, err := r.ws().Dispatch(ctx, r.req(wpA, 2)); !errors.Is(err, service.ErrPromptIntegrity) || len(r.g.Messages()) != sends {
		t.Fatalf("T12.29.2 tampered prompt dispatched: %v", err)
	}
	if n := p9Count(t, r.h, "worker_dispatches", "worker_prompt_id = '"+wpA.ID.String()+"' AND attempt = 2"); n != 0 {
		t.Fatal("tampered prompt persisted a dispatch")
	}

	// Every dispatch persisted; every raw response + telemetry persisted;
	// PostgreSQL reconstructs project → session → dispatch → prompt →
	// response → usage.
	if n := p9Count(t, r.h, "worker_dispatches", "project_id = '"+pid+"'"); n != 4 {
		t.Fatalf("dispatches %d", n)
	}
	var chain int
	if err := r.h.DBScan(ctx, `SELECT count(*) FROM projects p
		JOIN worker_sessions s ON s.project_id = p.id
		JOIN worker_dispatches d ON d.session_id = s.id AND d.project_id = p.id
		JOIN worker_prompts wp ON wp.id = d.worker_prompt_id AND wp.hash = d.prompt_hash
		JOIN worker_responses rs ON rs.dispatch_id = d.id
		JOIN worker_usage_observations u ON u.dispatch_id = d.id AND u.session_id = s.id
		WHERE p.id = '`+pid+`'`, &chain); err != nil || chain != 2 {
		t.Fatalf("persistence chain %d %v", chain, err)
	}
	if r.workflowRows(t) != before {
		t.Fatal("a worker response changed WBS / workflow state")
	}

	// Application restart reconciles persisted sessions.
	r.h.Close()
	r.h = boot()
	if got := r.session(t, s2.ID); got.Status != worker.SessionActive || got.WorkingDirectory == nil || *got.WorkingDirectory != "/repos/a" {
		t.Fatal("restart reconciliation")
	}
	if got := r.session(t, s1.ID); got.Status != worker.SessionReset {
		t.Fatal("history changed by restart")
	}
	p12NoSecret(t, "worker tables", p10Rows(t, r.h, "worker_events", "project_id = '"+pid+"'")+p10Rows(t, r.h, "worker_dispatches", "project_id = '"+pid+"'")+
		p10Rows(t, r.h, "worker_responses", "project_id = '"+pid+"'"))
}
