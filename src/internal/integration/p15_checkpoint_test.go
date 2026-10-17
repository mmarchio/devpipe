package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// TestP15150CheckpointVerification — WBS 15.150 / 15.151 / 15.153 / 15.154
// / 15.155: a changed or missing referenced artifact fails verification;
// activation without a verified checkpoint is refused and leaves project
// and execution READY together; re-activation of a RUNNING execution is a
// no-op without events.
func TestP15150CheckpointVerification(t *testing.T) {
	r := p15NewRig(t)
	ctx := context.Background()
	e := r.bootstrap(t).Execution
	id := e.ID.String()
	if probs, _ := r.svc().VerifyBootstrapCheckpoint(ctx, e.ID); len(probs) != 0 {
		t.Fatalf("clean checkpoint reported %v", probs)
	}
	// Idempotent activation: no new events.
	before := r.eventCounts(t)
	if _, err := r.svc().Activate(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if after := r.eventCounts(t); after[string(execution.EventStarted)] != before[string(execution.EventStarted)] {
		t.Fatal("duplicate EXECUTION_STARTED")
	}
	// Missing referenced artifact (FINAL baseline deleted).
	r.exec(t, "DELETE FROM execution_repository_baselines WHERE kind = 'FINAL' AND execution_id = '"+id+"'")
	probs, err := r.svc().VerifyBootstrapCheckpoint(ctx, e.ID)
	if err != nil || len(probs) == 0 || !strings.Contains(strings.Join(probs, ";"), "baseline") {
		t.Fatalf("missing artifact not detected: %v %v", probs, err)
	}
	// Activation without a checkpoint is refused (status alignment kept).
	r.exec(t, "UPDATE projects SET status = 'READY' WHERE id = '"+r.pid.String()+"'")
	r.exec(t, "UPDATE executions SET status = 'READY' WHERE id = '"+id+"'")
	r.exec(t, "DELETE FROM execution_bootstrap_checkpoints WHERE execution_id = '"+id+"'")
	if _, err := r.svc().Activate(ctx, e.ID); err == nil {
		t.Fatal("activated without a checkpoint")
	}
	var es string
	_ = r.h.DBScan(ctx, "SELECT status FROM executions WHERE id = '"+id+"'", &es)
	if es != "READY" || r.projectStatus(t) != "READY" {
		t.Fatalf("misaligned statuses: execution %s project %s", es, r.projectStatus(t))
	}
}
