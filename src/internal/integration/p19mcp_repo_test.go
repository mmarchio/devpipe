package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// p19mcpDispatch runs the statefile workflow's first step (the skill
// command) so the repository tests have a real execution and dispatch.
func p19mcpDispatch(t *testing.T) (*p16Rig, uuid.UUID) {
	t.Helper()
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	r.g.Responder = team.respond
	if _, err := r.eng().Run(context.Background(), r.execID, 2); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := r.h.DBScan(context.Background(), "SELECT id FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' ORDER BY created_at LIMIT 1", &id); err != nil {
		t.Fatal(err)
	}
	return r, id
}

// T2.5 / T2.7 / T3.8 / T3.9 — tokens, idempotent requests and task reports:
// duplicates are conflicts, evidence is immutable.
func TestP19MCPRepository(t *testing.T) {
	r, dispatch := p19mcpDispatch(t)
	ctx := context.Background()
	repo := dbrepo.NewMCPRepository(r.h.App.DB().Raw())
	now := time.Now().UTC().Truncate(time.Microsecond)

	var prompt uuid.UUID
	if err := r.h.DBScan(ctx, "SELECT worker_prompt_id FROM worker_dispatches WHERE id = '"+dispatch.String()+"'", &prompt); err != nil {
		t.Fatal(err)
	}
	// The skill command carried the overseer's own token; the test's
	// replaces it.
	if _, err := r.h.DBExec(ctx, "DELETE FROM mcp_dispatch_tokens WHERE worker_prompt_id = '"+prompt.String()+"'"); err != nil {
		t.Fatal(err)
	}
	tok := &persistence.MCPTokenRecord{TokenHash: strings.Repeat("a", 64), WorkerPromptID: prompt, ProjectID: r.pid, ExecutionID: r.execID,
		RoleKey: "PROJECT_MANAGER", StateKey: "SKILL", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := repo.CreateToken(ctx, nil, tok); err != nil {
		t.Fatal(err)
	}
	second := *tok
	second.TokenHash = strings.Repeat("b", 64)
	if err := repo.CreateToken(ctx, nil, &second); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("second token for the prompt: %v", err)
	}
	got, err := repo.GetToken(ctx, nil, tok.TokenHash)
	if err != nil || got.WorkerPromptID != prompt || got.RevokedAt != nil || !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := repo.GetToken(ctx, nil, strings.Repeat("c", 64)); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
	// T9.7: the dispatch finished, so nothing is in flight for the prompt.
	if _, err := repo.InFlightDispatch(ctx, nil, prompt); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("in-flight dispatch of a finished prompt: %v", err)
	}
	if err := repo.RevokeExecutionTokens(ctx, nil, r.execID, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetToken(ctx, nil, tok.TokenHash); got.RevokedAt == nil {
		t.Fatal("token not revoked")
	}

	req := &persistence.MCPRequestRecord{ID: uuid.New(), DispatchID: dispatch, Tool: "submit_result", IdempotencyKey: "k1",
		PayloadHash: strings.Repeat("d", 64), Result: json.RawMessage(`{"ok":true}`), CreatedAt: now}
	if err := repo.CreateRequest(ctx, nil, req); err != nil {
		t.Fatal(err)
	}
	dup := *req
	dup.ID = uuid.New()
	if err := repo.CreateRequest(ctx, nil, &dup); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("duplicate key: %v", err)
	}
	if got, err := repo.GetRequest(ctx, nil, dispatch, "submit_result", "k1"); err != nil || string(got.Result) != `{"ok": true}` {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := r.h.DBExec(ctx, "UPDATE mcp_requests SET result = '{}'"); err == nil {
		t.Fatal("an MCP request was updated")
	}

	rep := &persistence.TaskReportRecord{ID: uuid.New(), ProjectID: r.pid, ExecutionID: r.execID, DispatchID: dispatch, RoleKey: "TESTER",
		StateKey: "TEST", Kind: persistence.ReportTest, Status: persistence.ReportComplete, CriteriaMet: []bool{true, false},
		Summary: "tested", Evidence: map[string]any{"tests": "pass"}, CreatedAt: now}
	if err := repo.CreateTaskReport(ctx, nil, rep); err != nil {
		t.Fatal(err)
	}
	again := *rep
	again.ID = uuid.New()
	if err := repo.CreateTaskReport(ctx, nil, &again); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("second report of the kind: %v", err)
	}
	gr, err := repo.GetTaskReport(ctx, nil, dispatch, persistence.ReportTest)
	if err != nil || len(gr.CriteriaMet) != 2 || !gr.CriteriaMet[0] || gr.CriteriaMet[1] || gr.Evidence["tests"] != "pass" {
		t.Fatalf("%+v %v", gr, err)
	}
	if _, err := repo.GetTaskReport(ctx, nil, dispatch, persistence.ReportPlan); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing kind: %v", err)
	}
	if _, err := r.h.DBExec(ctx, "UPDATE task_reports SET status = 'FAILED'"); err == nil {
		t.Fatal("a task report was updated")
	}
	orphan := *rep
	orphan.ID, orphan.DispatchID = uuid.New(), uuid.New()
	if err := repo.CreateTaskReport(ctx, nil, &orphan); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("report of an unknown dispatch: %v", err)
	}
	// The new event types are accepted.
	ex := r.execID
	if err := dbrepo.NewExecutionRepository(r.h.App.DB().Raw()).AppendEvent(ctx, nil, &execution.Event{ID: uuid.New(), ProjectID: r.pid,
		ExecutionID: &ex, Type: execution.EventMCPResult, Payload: map[string]any{}, CreatedAt: now}); err != nil {
		t.Fatalf("MCP_RESULT event: %v", err)
	}
}

// T10.4 / T10.5 / T10.7 — the outbox: a dedupe key is queued once, a
// claimed row is invisible to other claimers until its lease expires, a
// failure retries later, past the limit it is DEAD.
func TestP19MCPOutbox(t *testing.T) {
	r, _ := p19mcpDispatch(t)
	ctx := context.Background()
	repo := dbrepo.NewMCPRepository(r.h.App.DB().Raw())
	now := time.Now().UTC()
	ex := r.execID
	for i := 0; i < 2; i++ {
		if err := repo.EnqueueOutbox(ctx, nil, &persistence.OutboxRecord{ID: uuid.New(), ProjectID: r.pid, ExecutionID: &ex,
			Kind: persistence.OutboxEngineWake, DedupeKey: "wake-1", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if l, _ := repo.ListOutbox(ctx, r.pid, 10); len(l) != 1 {
		t.Fatalf("%d rows for one dedupe key", len(l))
	}
	a, err := repo.ClaimOutbox(ctx, "a", now, time.Minute, 10)
	if err != nil || len(a) != 1 {
		t.Fatalf("%d %v", len(a), err)
	}
	if b, _ := repo.ClaimOutbox(ctx, "b", now, time.Minute, 10); len(b) != 0 {
		t.Fatal("a leased row was claimed twice")
	}
	if err := repo.FailOutbox(ctx, a[0].ID, "b", "x", now, false, now); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("fail by a non-owner: %v", err)
	}
	if err := repo.FailOutbox(ctx, a[0].ID, "a", "runtime unavailable", now.Add(time.Second), false, now); err != nil {
		t.Fatal(err)
	}
	if b, _ := repo.ClaimOutbox(ctx, "b", now, time.Minute, 10); len(b) != 0 {
		t.Fatal("a row was claimed before its retry time")
	}
	// A crashed claimer: the lease expires and another instance claims it.
	b, _ := repo.ClaimOutbox(ctx, "b", now.Add(2*time.Second), time.Second, 10)
	if len(b) != 1 || b[0].Attempts != 1 || b[0].LastError != "runtime unavailable" {
		t.Fatalf("%+v", b)
	}
	c, _ := repo.ClaimOutbox(ctx, "c", now.Add(4*time.Second), time.Minute, 10)
	if len(c) != 1 {
		t.Fatal("an expired lease was not reclaimed")
	}
	if err := repo.FinishOutbox(ctx, c[0].ID, "b", now); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("finish after losing the lease: %v", err)
	}
	if err := repo.FailOutbox(ctx, c[0].ID, "c", "gave up", now, true, now); err != nil {
		t.Fatal(err)
	}
	l, _ := repo.ListOutbox(ctx, r.pid, 10)
	if l[0].Status != persistence.OutboxDead || l[0].FinishedAt == nil {
		t.Fatalf("%+v", l[0])
	}
}

// T10.3 / T10.6 / T10.7 — the dispatcher performs committed actions, a
// failing one is retried with backoff and moves to DEAD once the policy's
// attempts are spent; the others are unaffected.
func TestP19MCPOutboxDispatcher(t *testing.T) {
	r, _ := p19mcpDispatch(t)
	ctx := context.Background()
	repo := dbrepo.NewMCPRepository(r.h.App.DB().Raw())
	ex := r.execID
	now := time.Now().UTC()
	for _, k := range []string{persistence.OutboxBlockerNotify, persistence.OutboxContextReset} {
		if err := repo.EnqueueOutbox(ctx, nil, &persistence.OutboxRecord{ID: uuid.New(), ProjectID: r.pid, ExecutionID: &ex, Kind: k,
			Payload: map[string]any{"reason": "x"}, DedupeKey: k, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	d := service.NewOutboxDispatcher(repo, r.eng(), "test", nil)
	clock := now
	d.SetClock(func() time.Time { return clock })
	d.Handle(persistence.OutboxContextReset, func(context.Context, *persistence.OutboxRecord) error { return errors.New("runtime unavailable") })
	if n, err := d.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("first run: %d %v", n, err)
	}
	for i := 0; i < service.OutboxPolicy.MaxAttempts; i++ {
		clock = clock.Add(service.OutboxPolicy.Max + time.Second)
		if _, err := d.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]*persistence.OutboxRecord{}
	l, _ := repo.ListOutbox(ctx, r.pid, 10)
	for _, o := range l {
		got[o.Kind] = o
	}
	if b := got[persistence.OutboxBlockerNotify]; b.Status != persistence.OutboxDone || b.Attempts != 1 {
		t.Fatalf("blocker %+v", b)
	}
	if c := got[persistence.OutboxContextReset]; c.Status != persistence.OutboxDead || c.Attempts != service.OutboxPolicy.MaxAttempts || c.LastError != "runtime unavailable" {
		t.Fatalf("reset %+v", c)
	}
}
