package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/worker"
)

// P12-43..P12-56: schema guards and repository behaviour.
func TestP1243WorkerRepositories(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	raw := r.h.App.DB().Raw()
	sessions := dbrepo.NewWorkerSessionRepository(raw)
	dispatches := dbrepo.NewWorkerDispatchRepository(raw)
	responses := dbrepo.NewWorkerResponseRepository(raw)
	leases := dbrepo.NewWorkerLeaseRepository(raw)
	settings := dbrepo.NewWorkerSettingsRepository(raw)
	pid := r.pos.ProjectID
	now := time.Now().UTC()
	mk := func(provID string, policy worker.SessionPolicy) *persistence.WorkerSessionRecord {
		return &persistence.WorkerSessionRecord{ID: uuid.New(), ProjectID: pid, Provider: worker.ProviderHermesGateway, ProviderSessionID: provID,
			Status: worker.SessionActive, Policy: policy, CreatedAt: now, LastSeenAt: now}
	}

	// Settings defaults + upsert (T12.22.3).
	if st, stored, _ := settings.Get(ctx, pid); stored || st.SessionPolicy != worker.PolicySingleActive || st.ConcurrentSend != worker.SendReject {
		t.Fatal("settings defaults")
	}
	settings.Upsert(ctx, pid, worker.Settings{SessionPolicy: worker.PolicyMultipleActive, ConcurrentSend: worker.SendWait, Lease: 2 * time.Second, Wait: time.Second})
	if st, stored, _ := settings.Get(ctx, pid); !stored || st.SessionPolicy != worker.PolicyMultipleActive || st.Lease != 2*time.Second {
		t.Fatal("T12.22.3 policy persisted")
	}

	// Single active per project (T12.21.3) vs MULTIPLE_ACTIVE (T12.22.2).
	a := mk("s-a", worker.PolicySingleActive)
	if err := sessions.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Create(ctx, mk("s-b", worker.PolicySingleActive)); !errors.Is(err, persistence.ErrConflict) {
		t.Fatalf("T12.22.1 two single-policy ACTIVE sessions: %v", err)
	}
	m1, m2 := mk("m-1", worker.PolicyMultipleActive), mk("m-2", worker.PolicyMultipleActive)
	if sessions.Create(ctx, m1) != nil || sessions.Create(ctx, m2) != nil {
		t.Fatal("T12.22.2 multiple active not representable")
	}
	sessions.MarkClosed(ctx, pid, m1.ID, "test", now)
	sessions.MarkClosed(ctx, pid, m2.ID, "test", now)
	// Transactional replacement; a failing insert rolls back (T12.64.2).
	dup := mk("s-a", worker.PolicySingleActive)
	if err := sessions.ReplaceActive(ctx, dup, &a.ID, now); err == nil {
		t.Fatal("duplicate provider session accepted")
	}
	if got, _ := sessions.Get(ctx, pid, a.ID); got.Status != worker.SessionActive {
		t.Fatal("T12.21.4/T12.63.1 failed replacement changed the old session")
	}
	b := mk("s-b", worker.PolicySingleActive)
	if err := sessions.ReplaceActive(ctx, b, &a.ID, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := sessions.Get(ctx, pid, a.ID); got.Status != worker.SessionReset {
		t.Fatal("old session not RESET")
	}
	if act, _ := sessions.GetActiveForProject(ctx, pid); act.ID != b.ID {
		t.Fatal("T12.18.2")
	}
	// Final statuses are final; history retained (T12.17.4, T12.18.3).
	if _, err := r.h.DBExec(ctx, "UPDATE worker_sessions SET status = 'ACTIVE', closed_at = NULL WHERE id = '"+a.ID.String()+"'"); err == nil {
		t.Fatal("RESET session reactivated")
	}
	if all, _ := sessions.ListForProject(ctx, pid); len(all) != 4 {
		t.Fatal("history")
	}
	// Cross-project access rejected (T12.18.4).
	if _, err := sessions.Get(ctx, uuid.New(), b.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal("cross-project read")
	}
	if err := sessions.UpdateWorkingDirectory(ctx, uuid.New(), b.ID, ptrS("/x"), now); err == nil {
		t.Fatal("cross-project update")
	}

	// Dispatch idempotency + transitions.
	wp := r.prompt(t, "")
	d := worker.WorkerDispatch{ID: uuid.New(), ProjectID: pid, SessionID: b.ID, WorkerPromptID: wp.ID, PromptHash: wp.Hash, RuntimeStateID: r.runtime,
		RequestID: uuid.New(), OperationID: worker.LogicalOperationID(r.runtime, nil, wp.ID), Attempt: 1, Status: worker.DispatchPending, CreatedAt: now}
	if err := dispatches.Create(ctx, &d); err != nil {
		t.Fatal(err)
	}
	d2 := d
	d2.ID, d2.RequestID = uuid.New(), uuid.New()
	if err := dispatches.Create(ctx, &d2); !errors.Is(err, persistence.ErrConflict) {
		t.Fatal("T12.33.3 duplicate attempt persisted")
	}
	if ok, _ := dispatches.Transition(ctx, d.ID, worker.DispatchWaiting, worker.DispatchCompleted, persistence.DispatchUpdate{At: now}); ok {
		t.Fatal("CAS ignored current status")
	}
	dispatches.Transition(ctx, d.ID, worker.DispatchPending, worker.DispatchSending, persistence.DispatchUpdate{At: now})
	dispatches.Transition(ctx, d.ID, worker.DispatchSending, worker.DispatchWaiting, persistence.DispatchUpdate{At: now})
	rec := worker.WorkerResponseRecord{ID: uuid.New(), DispatchID: d.ID, ProjectID: pid, Content: "raw\r\n bytes ", ContentHash: worker.ContentHash("raw\r\n bytes "),
		StartedAt: now, CompletedAt: now, CreatedAt: now}
	del := worker.DeliveryReceived
	if ok, err := responses.CompleteWithResponse(ctx, &rec, persistence.DispatchUpdate{Delivery: &del, CompletedAt: &now, At: now}); !ok || err != nil {
		t.Fatal(err)
	}
	if got, _ := responses.GetByDispatch(ctx, pid, d.ID); got.Content != "raw\r\n bytes " {
		t.Fatal("T12.31.1 raw response not exact")
	}
	if _, err := r.h.DBExec(ctx, "UPDATE worker_responses SET content = 'parsed' WHERE id = '"+rec.ID.String()+"'"); err == nil {
		t.Fatal("T12.31.3 raw response mutable")
	}
	if _, err := r.h.DBExec(ctx, "UPDATE worker_dispatches SET status = 'PENDING' WHERE id = '"+d.ID.String()+"'"); err == nil {
		t.Fatal("terminal dispatch reopened")
	}
	if ok, _ := responses.CompleteWithResponse(ctx, &worker.WorkerResponseRecord{ID: uuid.New(), DispatchID: d.ID, ProjectID: pid, ContentHash: worker.ContentHash(""),
		StartedAt: now, CompletedAt: now, CreatedAt: now}, persistence.DispatchUpdate{At: now}); ok {
		t.Fatal("second response attached")
	}

	// Usage observations ordered; estimate flag kept (T12.50.x).
	for i, src := range []worker.UsageSource{worker.SourceProviderActual, worker.SourceEstimated} {
		o := &persistence.WorkerUsageObservation{ID: uuid.New(), ProjectID: pid, SessionID: b.ID, Usage: worker.WorkerUsage{ContextTokens: worker.Int64(int64(1000 * (i + 1)))},
			Source: src, ObservedAt: now}
		if err := responses.InsertObservation(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	obs, _ := responses.ListObservations(ctx, pid, &b.ID)
	if len(obs) != 2 || obs[0].Seq >= obs[1].Seq || obs[0].Usage.Estimated || !obs[1].Usage.Estimated {
		t.Fatalf("observations %+v", obs)
	}
	if last, _ := responses.LatestObservation(ctx, pid, b.ID); *last.Usage.ContextTokens != 2000 {
		t.Fatal("latest observation")
	}

	// Leases (T12.57.x).
	l := persistence.WorkerSessionLease{SessionID: b.ID, ProjectID: pid, Owner: "A", AcquiredAt: now, ExpiresAt: now.Add(time.Minute)}
	if ok, _ := leases.Acquire(ctx, l); !ok {
		t.Fatal("T12.57.1")
	}
	l2 := l
	l2.Owner = "B"
	if ok, _ := leases.Acquire(ctx, l2); ok {
		t.Fatal("T12.57.2")
	}
	if ok, _ := leases.Renew(ctx, b.ID, "B", now.Add(2*time.Minute)); ok {
		t.Fatal("foreign renew")
	}
	if ok, _ := leases.Renew(ctx, b.ID, "A", now.Add(2*time.Minute)); !ok {
		t.Fatal("renew")
	}
	l2.AcquiredAt, l2.ExpiresAt = now.Add(3*time.Minute), now.Add(4*time.Minute)
	if ok, _ := leases.Acquire(ctx, l2); !ok {
		t.Fatal("T12.57.3 expired lease not reclaimable")
	}
	if ok, _ := leases.Release(ctx, b.ID, "A"); ok {
		t.Fatal("stale owner released the lease")
	}
	if n, _ := leases.ReleaseExpired(ctx, now.Add(10*time.Minute)); n != 1 {
		t.Fatal("T12.57.4 expired lease not released")
	}
}
