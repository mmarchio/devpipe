package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/service"
)

// T18.12–T18.15 — leases: a live lease is never stolen; after expiry a new
// instance continues with a higher epoch; a frozen old holder that
// resumes cannot perform its side effect or mutate; epochs never decrease.
func TestT1814SplitBrain(t *testing.T) {
	base15, boot := p15RestartRig(t, []p15Phase{{1, "Foundation", "PENDING", 1}}, nil)
	a := &p16Rig{p15Rig: base15}
	p16Configure(t, a, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) { s.DriverLease = time.Second }})
	ctx := context.Background()
	b := boot(nil) // instance B on the same database
	engA, engB := a.eng(), b.App.Engine()
	if engA.Owner() == engB.Owner() {
		t.Fatal("instances share an owner")
	}
	a.queuePlan(a.planJSON(p16Unit{key: "u"}))
	a.queue("complete")
	epochs := []int64{}
	epoch := func() int64 {
		var e int64
		_ = a.h.DBScan(ctx, "SELECT driver_epoch FROM executions WHERE id = '"+a.execID.String()+"'", &e)
		return e
	}
	// A freezes right before sending the unit's dispatch (past its TTL).
	frozen := make(chan struct{})
	release := make(chan struct{})
	engA.SetStepHook(func(ctx context.Context, act execution.StepAction, stage string) error {
		if act == execution.ActionDispatch && stage == "dispatch" {
			close(frozen)
			<-release
		}
		return nil
	})
	defer engA.SetStepHook(nil)
	errA := make(chan error, 1)
	var resA []execution.StepResult
	go func() { var err error; resA, err = engA.Run(ctx, a.execID, 100); errA <- err }()
	<-frozen
	epochs = append(epochs, epoch())
	sends := len(a.g.Messages())
	// 18.13 — while A's lease is live, B cannot drive the execution.
	resB, err := engB.Run(ctx, a.execID, 5)
	if err != nil && !strings.Contains(err.Error(), "another scheduler") {
		t.Fatalf("B run during live lease: %v", err)
	}
	if len(a.g.Messages()) != sends || epoch() != epochs[0] {
		t.Fatalf("B stole a live lease: %v", p16Trace(resB))
	}
	// 18.12 — after expiry B takes over with a higher epoch and continues.
	time.Sleep(1200 * time.Millisecond)
	resB, err = engB.Run(ctx, a.execID, 100)
	if err != nil || len(resB) == 0 || resB[len(resB)-1].Outcome != execution.StepCompleted {
		var ev string
		_ = a.h.DBScan(ctx, "SELECT recovery_evidence::text FROM executions WHERE id = '"+a.execID.String()+"'", &ev)
		t.Fatalf("B takeover: %v %s %s", err, p16Trace(resB), ev)
	}
	epochs = append(epochs, epoch())
	if epochs[1] <= epochs[0] {
		t.Fatalf("epoch did not increase: %v", epochs)
	}
	// 18.14 — A resumes: fenced, no send, no mutation.
	afterB := len(a.g.Messages())
	fp := p18Fingerprint(t, a)
	close(release)
	// A either reports the lost lease or yields with the execution's
	// current status; in both cases it performs nothing (checked below).
	if err := <-errA; err != nil && !errors.Is(err, service.ErrFenced) && !strings.Contains(strings.ToLower(errString(err)), "lease") {
		t.Fatalf("A after takeover: %v %s", err, p16Trace(resA))
	}
	if n := len(resA); n > 0 && resA[n-1].Outcome == execution.StepProgressed {
		t.Fatalf("A progressed after losing the lease: %s", p16Trace(resA))
	}
	if len(a.g.Messages()) != afterB || p18Fingerprint(t, a) != fp {
		t.Fatal("stale instance acted after losing the lease")
	}
	// Exactly one unit dispatch happened (no duplicate send).
	if n := a.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL"); n != 1 {
		t.Fatalf("unit dispatches %d", n)
	}
	// 18.15 — the epoch can never be lowered.
	if _, err := a.h.DBExec(ctx, "UPDATE executions SET driver_epoch = driver_epoch - 1 WHERE id = '"+a.execID.String()+"'"); err == nil {
		t.Fatal("epoch decreased")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
