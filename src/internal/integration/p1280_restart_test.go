package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
	"overseer/internal/worker"
)

func p12Paths(g *hermestest.Gateway, from int) []string { return g.Paths()[from:] }

// P12-80 (WBS 12.43, 12.44, 12.75, 12.76, 12.77, 12.102): restart with
// S1 ACTIVE (CWD /repos/a), D1 COMPLETED and D2 WAITING without response.
func TestP1280RestartRecovery(t *testing.T) {
	r, boot := p12RestartRig(t)
	ctx := context.Background()
	s1 := r.newSession(t)
	r.cd(t, "/repos/a")
	wp := r.prompt(t, "")
	d1 := r.dispatch(t, wp, 1).Dispatch
	r.svc().Wait()
	// D2: the overseer died while waiting (its lease expired long ago).
	d2 := uuid.New()
	pid := r.pos.ProjectID.String()
	if _, err := r.h.DBExec(ctx, `INSERT INTO worker_dispatches (id, project_id, session_id, phase_id, work_unit_id, worker_prompt_id, prompt_hash,
		runtime_state_id, state_key, role_key, request_id, operation_id, attempt, status, owner, created_at, started_at, updated_at)
		SELECT '`+d2.String()+`', project_id, session_id, phase_id, work_unit_id, worker_prompt_id, prompt_hash, runtime_state_id, state_key, role_key,
		gen_random_uuid(), operation_id, 2, 'WAITING', 'dead-instance', now(), now(), now() FROM worker_dispatches WHERE id = '`+d1.ID.String()+`'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.DBExec(ctx, `INSERT INTO worker_session_leases (session_id, project_id, owner, dispatch_id, acquired_at, expires_at)
		VALUES ('`+s1.ID.String()+`', '`+pid+`', 'dead-instance', '`+d2.String()+`', now() - interval '10 minutes', now() - interval '9 minutes')`); err != nil {
		t.Fatal(err)
	}
	sends, gwSessions := len(r.g.Messages()), len(r.g.Sessions())
	before := r.workflowRows(t)

	// Restart #1 with the gateway unreachable (T12.43.3): nothing
	// invented — S1 unchanged (UNKNOWN), D2 still reclassified UNKNOWN.
	r.proxy.Stop()
	r.h.Close()
	r.h = boot()
	if got := r.session(t, s1.ID); got.Status != worker.SessionActive {
		t.Fatalf("T12.43.3 S1 = %s", got.Status)
	}
	dd2, _ := r.ws().GetDispatch(ctx, r.pos.ProjectID, d2)
	if dd2.Dispatch.Status != worker.DispatchUnknown || *dd2.Dispatch.Delivery != worker.DeliveryUnknown || *dd2.Dispatch.RetryClass != worker.RetryRecovery {
		t.Fatalf("T12.44.3 D2 = %+v", dd2.Dispatch)
	}
	if h, _ := r.ws().Health(ctx, r.pos.ProjectID); h.ConnectionState == worker.ConnConnected {
		t.Fatal("unreachable gateway reported healthy")
	}

	// Restart #2, gateway reachable: S1 CONFIRMED, CWD reconciled, no
	// unnecessary "new", D1 COMPLETED, D2 not resent.
	r.proxy.Start(t)
	pathsBefore := len(r.g.Paths())
	seenBefore := r.session(t, s1.ID).LastSeenAt
	r.h.Close()
	r.h = boot()
	got := r.session(t, s1.ID)
	if got.Status != worker.SessionActive || got.WorkingDirectory == nil || *got.WorkingDirectory != "/repos/a" || !got.LastSeenAt.After(seenBefore) {
		t.Fatalf("12.75 S1 not reconciled: %+v", got)
	}
	for _, p := range p12Paths(r.g, pathsBefore) {
		if p == "POST /v1/sessions" {
			t.Fatal("12.75 unnecessary new session")
		}
	}
	if len(r.g.Messages()) != sends || len(r.g.Sessions()) != gwSessions {
		t.Fatal("T12.44.2 / 12.102 D2 resent or session created")
	}
	if dd1, _ := r.ws().GetDispatch(ctx, r.pos.ProjectID, d1.ID); dd1.Dispatch.Status != worker.DispatchCompleted || dd1.Response == nil {
		t.Fatal("12.102 D1 not retained")
	}
	if r.workflowRows(t) != before {
		t.Fatal("12.102 workflow state changed")
	}
	if n := p9Count(t, r.h, "worker_session_leases", "session_id = '"+s1.ID.String()+"'"); n != 0 {
		t.Fatal("T12.57.4 crashed lease not released")
	}
	// The confirmed session is usable without a new session.
	if res := r.dispatch(t, r.prompt(t, "after-restart"), 1); res.Dispatch.Status != worker.DispatchCompleted || res.Dispatch.SessionID != s1.ID {
		t.Fatalf("post-restart dispatch %+v", res.Err)
	}

	// Restart #3, gateway lost S1 (12.76). Chat completions keep no
	// server-side session to look up, so the restart adopts S1; the
	// gateway's rejection of the next send makes it LOST — no automatic
	// new session, later orchestration sees the recovery condition.
	r.g.KillSession(s1.ProviderSessionID)
	gwSessions = len(r.g.Sessions())
	r.h.Close()
	r.h = boot()
	if got := r.session(t, s1.ID); got.Status != worker.SessionActive {
		t.Fatalf("12.76 S1 = %s after restart, want ACTIVE (adopted)", got.Status)
	}
	if res, _ := r.ws().Dispatch(ctx, r.req(r.prompt(t, "after-loss"), 1)); res == nil || res.Err == nil || res.Err.Code != worker.CodeSessionLost {
		t.Fatalf("12.76 send on a lost session: %+v", res)
	}
	if got := r.session(t, s1.ID); got.Status != worker.SessionLost {
		t.Fatalf("12.76 S1 = %s", got.Status)
	}
	if len(r.g.Sessions()) != gwSessions {
		t.Fatal("12.76 new session created")
	}
	if rd, _ := r.ws().Readiness(ctx, r.pos.ProjectID); rd.Ready() || rd.SessionActive {
		t.Fatal("lost session reported ready")
	}
	if _, err := r.ws().Dispatch(ctx, r.req(wp, 3)); !errors.Is(err, service.ErrNoActiveSession) {
		t.Fatalf("12.76 recovery condition: %v", err)
	}
}

// P12-70 (WBS 12.66/12.67): graceful shutdown while a dispatch is in
// flight — the worker is closed before PostgreSQL and the dispatch's final
// status is persisted.
func TestP1267ShutdownPersistsInFlight(t *testing.T) {
	r, boot := p12RestartRig(t)
	ctx := context.Background()
	s := r.newSession(t)
	r.cd(t, "/repos/a")
	wp := r.prompt(t, "")
	r.g.Script(hermestest.Behavior{Delay: 20 * time.Second})
	done := make(chan *service.DispatchResult, 1)
	go func() {
		res, _ := r.ws().Dispatch(ctx, r.req(wp, 1))
		done <- res
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.g.Messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	r.h.Close()
	if time.Since(start) > 10*time.Second {
		t.Fatal("T12.67.3 shutdown not bounded")
	}
	res := <-done
	if res == nil || !res.Dispatch.Status.Terminal() {
		t.Fatalf("T12.67.2 in-flight dispatch not finalized: %+v", res)
	}
	r.h = boot()
	dd, err := r.ws().GetDispatch(ctx, r.pos.ProjectID, res.Dispatch.ID)
	if err != nil || dd.Dispatch.Status != res.Dispatch.Status || dd.Dispatch.Status == worker.DispatchCompleted {
		t.Fatalf("T12.67.1/T12.67.2 persisted %+v (returned %s)", dd, res.Dispatch.Status)
	}
	if got := r.session(t, s.ID); got.Status != worker.SessionActive {
		t.Fatal("T12.66.4 close marked the external session destroyed")
	}
	if len(r.g.Messages()) != 1 {
		t.Fatal("resent after restart")
	}
}
