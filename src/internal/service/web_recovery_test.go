package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/worker"
)

type fakeExecGet struct{ rec *persistence.ExecutionRecord }

func (f *fakeExecGet) Get(context.Context, persistence.Executor, uuid.UUID, uuid.UUID) (*persistence.ExecutionRecord, error) {
	if f.rec == nil {
		return nil, persistence.ErrNotFound
	}
	cp := *f.rec
	return &cp, nil
}

type fakeResolver struct{ got []RecoveryResolution }

func (f *fakeResolver) ResolveRecovery(_ context.Context, _, _ uuid.UUID, r RecoveryResolution) (*persistence.ExecutionRecord, error) {
	f.got = append(f.got, r)
	return &persistence.ExecutionRecord{Execution: execution.Execution{Status: execution.ExecutionRunning}}, nil
}

type fakeReconciler struct{ n int }

func (f *fakeReconciler) Reconnect(context.Context, uuid.UUID) (worker.Reconciliation, error) {
	f.n++
	return worker.ReconcileConfirmed, nil
}

// P17-43 (WBS 17.98–17.102) — only offered actions run; high-impact needs
// confirmation + reason; unknown delivery never maps to a dispatch.
func TestP1743RecoveryActions(t *testing.T) {
	unit := uuid.New()
	ex := &fakeExecGet{rec: &persistence.ExecutionRecord{Execution: execution.Execution{Status: execution.ExecutionRecovery}, CurrentWorkUnitID: &unit,
		RecoveryReason:   "worker delivery unknown: the request is not resent",
		RecoveryEvidence: map[string]any{"token": "DELIVERY_UNKNOWN", "action": "EVALUATE_RESULT"}}}
	res, rc := &fakeResolver{}, &fakeReconciler{}
	s := NewRecoveryActionService(ex, res, rc)
	ctx := context.Background()
	p, e := uuid.New(), uuid.New()

	avail, _, err := s.Available(ctx, p, e)
	if err != nil || len(avail) != 3 || avail[0].Kind != execution.RecoveryReconcileDispatch {
		t.Fatalf("available %v %v", avail, err)
	}
	// Phase 18 (WBS 18.23): resolving an unknown delivery is offered but
	// is high-impact — it always needs confirmation.
	if avail[2].Kind != execution.RecoveryResolveDelivery || !avail[2].HighImpact {
		t.Fatalf("resolve delivery offer %v", avail[2])
	}
	if _, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryRunValidation}); !errors.Is(err, ErrActionNotAvailable) {
		t.Fatalf("not offered action ran: %v", err)
	}
	out, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryReconcileDispatch})
	if err != nil || rc.n != 1 || out.Reconciliation != "CONFIRMED" || len(res.got) != 0 {
		t.Fatalf("reconcile: %v %+v", err, out)
	}
	// Unknown delivery offers no MANUAL_RESUME (Phase 18 Decision 8); the
	// resolution needs confirmation, a reason and an explicit resolution.
	if _, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryManualResume, Confirmed: true, Reason: "checked"}); !errors.Is(err, ErrActionNotAvailable) {
		t.Fatalf("manual resume offered for unknown delivery: %v", err)
	}
	if _, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryResolveDelivery, Reason: "checked",
		Resolution: execution.ResolutionResendAfterReview}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unconfirmed resolution: %v", err)
	}
	if _, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryResolveDelivery, Confirmed: true, Reason: "checked"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("resolution without a resolution kind: %v", err)
	}

	ex.rec.RecoveryReason, ex.rec.RecoveryEvidence = "required checkpoint failed", nil
	if _, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryManualResume, Reason: "checked"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unconfirmed high-impact: %v", err)
	}
	if _, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryManualResume, Confirmed: true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("reasonless high-impact: %v", err)
	}
	out, err = s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryManualResume, Confirmed: true, Reason: "gateway log shows no delivery", By: "local-admin"})
	if err != nil || len(res.got) != 1 || res.got[0].Action != RecoveryResume || res.got[0].Note != "gateway log shows no delivery" || out.ExecutionStatus != "RUNNING" {
		t.Fatalf("resume: %v %+v", err, res.got)
	}

	// Manual review → APPROVE_CRITERIA with default unverified indexes.
	ex.rec.RecoveryReason = "acceptance criteria require manual review"
	ex.rec.RecoveryEvidence = map[string]any{"acceptance": []any{
		map[string]any{"index": float64(0), "verdict": "PASS"}, map[string]any{"index": float64(2), "verdict": "UNVERIFIED"}}}
	if _, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryApproveCriteria, Confirmed: true, Reason: "reviewed"}); err != nil {
		t.Fatal(err)
	}
	last := res.got[len(res.got)-1]
	if last.Action != RecoveryApproveCriteria || last.WorkUnitID != unit.String() || len(last.Indexes) != 1 || last.Indexes[0] != 2 {
		t.Fatalf("approve: %+v", last)
	}
	// Checkpoint failure → RETRY_STEP; not high impact.
	ex.rec.RecoveryReason = "required checkpoint failed"
	if _, err := s.Perform(ctx, p, e, RecoveryRequest{Action: execution.RecoveryRetryCheckpoint}); err != nil || res.got[len(res.got)-1].Action != RecoveryRetryStep {
		t.Fatalf("retry checkpoint: %v", err)
	}
	// Not in recovery → nothing offered.
	ex.rec.Status = execution.ExecutionRunning
	if avail, _, _ := s.Available(ctx, p, e); len(avail) != 0 {
		t.Fatal("actions offered outside recovery")
	}
}
