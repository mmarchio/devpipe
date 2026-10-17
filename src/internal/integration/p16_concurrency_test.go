package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// TestP16Concurrency — WBS 16.3–16.5, 16.22, 16.82 (P16-151).
func TestP16Concurrency(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	repo := r.engRepo()
	t.Run("driver lease: one scheduler at a time", func(t *testing.T) {
		epoch, ok, err := repo.AcquireDriver(ctx, r.execID, "other-scheduler", time.Now(), time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if _, err := r.eng().Step(ctx, r.execID); !errors.Is(err, service.ErrExecutionBusy) {
			t.Fatalf("second scheduler stepped: %v", err)
		}
		if _, ok, _ := repo.AcquireDriver(ctx, r.execID, "third", time.Now(), time.Minute); ok {
			t.Fatal("lease granted twice")
		}
		if err := repo.ReleaseDriver(ctx, r.execID, "other-scheduler", epoch); err != nil {
			t.Fatal(err)
		}
		// An expired lease is taken over with a new fencing epoch.
		e1, _, _ := repo.AcquireDriver(ctx, r.execID, "a", time.Now().Add(-2*time.Minute), time.Minute)
		e2, ok, _ := repo.AcquireDriver(ctx, r.execID, "b", time.Now(), time.Minute)
		if !ok || e2 != e1+1 {
			t.Fatalf("takeover epoch %d → %d", e1, e2)
		}
		if err := repo.CheckDriver(ctx, nil, r.execID, "a", e1, time.Now()); !errors.Is(err, persistence.ErrConflict) {
			t.Fatal("stale driver not fenced")
		}
		_ = repo.ReleaseDriver(ctx, r.execID, "b", e2)
	})
	t.Run("stale execution update fails", func(t *testing.T) {
		rec, err := r.svc().Execution(ctx, r.pid, r.execID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpdatePosition(ctx, nil, r.execID, rec.Version, persistence.PositionUpdate{WaitReason: "x", SetWait: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpdatePosition(ctx, nil, r.execID, rec.Version, persistence.PositionUpdate{WaitReason: "y", SetWait: true}); !errors.Is(err, persistence.ErrConflict) {
			t.Fatalf("stale update applied: %v", err)
		}
	})
	t.Run("second active execution rejected", func(t *testing.T) {
		r.exec(t, "UPDATE executions SET status = 'WAITING' WHERE id = '"+r.execID.String()+"'")
		defer r.exec(t, "UPDATE executions SET status = 'RUNNING' WHERE id = '"+r.execID.String()+"'")
		_, err := r.h.DBExec(ctx, "INSERT INTO executions (id, project_id, number, status, state_machine_id, state_machine_version, "+
			"state_machine_hash, initial_phase_id, wbs_version, plan_state, initial_state_key, plan_hash, bootstrap_stage, started_at, created_at, updated_at) "+
			"SELECT gen_random_uuid(), project_id, number + 1, 'RUNNING', state_machine_id, state_machine_version, state_machine_hash, initial_phase_id, "+
			"wbs_version, plan_state, initial_state_key, plan_hash, 'COMPLETE', started_at, created_at, updated_at FROM executions WHERE id = '"+r.execID.String()+"'")
		if err == nil || !strings.Contains(err.Error(), "executions_one_active_per_project") {
			t.Fatalf("second active execution not rejected by the active index: %v", err)
		}
	})
	t.Run("concurrent unit claim: one winner", func(t *testing.T) {
		plan := r.plan(t, 1, 1, "EXECUTING")
		u := r.unit(t, plan, 1, "claim-me", "PENDING")
		other := r.execID
		var wins, conflicts int
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := other
				if i%2 == 1 {
					id = uuid.New() // another (non-existent) scheduler's execution
				}
				err := repo.ClaimUnit(ctx, nil, u, id)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					wins++
				} else {
					conflicts++
				}
			}(i)
		}
		wg.Wait()
		if n := r.count(t, "SELECT count(DISTINCT claimed_execution_id) FROM work_units WHERE id = '"+u.String()+"'"); n != 1 {
			t.Fatalf("claimed by %d executions", n)
		}
		_ = wins
		_ = conflicts
		var unitStatus wbs.WorkUnitStatus
		if err := r.h.DBScan(ctx, "SELECT status FROM work_units WHERE id = '"+u.String()+"'", &unitStatus); err != nil || unitStatus != wbs.WorkUnitStatusEXECUTING {
			t.Fatalf("status %s %v", unitStatus, err)
		}
	})
	t.Run("concurrent events get unique sequences", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := r.execID
				if err := r.h.App.Bootstrap().AppendEngineEventForTest(ctx, r.pid, id, fmt.Sprint(i)); err != nil {
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		if n := r.count(t, "SELECT count(*) - count(DISTINCT exec_seq) FROM execution_events WHERE execution_id = '"+r.execID.String()+"'"); n != 0 {
			t.Fatal("duplicate execution sequence")
		}
		if n := r.count(t, "SELECT max(exec_seq) - count(*) FROM execution_events WHERE execution_id = '"+r.execID.String()+"'"); n != 0 {
			t.Fatal("sequence has gaps")
		}
	})
	t.Run("two engines step concurrently without duplicate dispatch", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		second := service.NewEngineForTest(r.h.App.Engine(), "second-instance")
		var wg sync.WaitGroup
		for _, e := range []*service.Engine{r.eng(), second} {
			wg.Add(1)
			go func(e *service.Engine) {
				defer wg.Done()
				for i := 0; i < 200 && r.execStatus(t) != "COMPLETED"; i++ {
					if _, err := e.Step(ctx, r.execID); err != nil && !errors.Is(err, service.ErrExecutionBusy) {
						t.Error(err)
						return
					}
				}
			}(e)
		}
		wg.Wait()
		if r.execStatus(t) != "COMPLETED" || len(r.g.Messages()) != 2 || countOf(r.events(t), "WORK_UNIT_DONE") != 1 {
			t.Fatalf("status %s messages %d events %v", r.execStatus(t), len(r.g.Messages()), r.events(t))
		}
	})
}

// TestP16SchedulerFairness — WBS 16.6 / 16.7 / 16.159: only eligible
// executions are scheduled and every runnable project gets turns.
func TestP16SchedulerFairness(t *testing.T) {
	h, g, srv := p15Harness(t, nil)
	var rigs []*p16Rig
	for i := 0; i < 3; i++ {
		r := &p16Rig{p15Rig: p15Build(t, h, g, srv, []p15Phase{{1, "P", "PENDING", 1}})}
		p16Configure(t, r, p16Options{})
		r.exec(t, "UPDATE project_settings SET auto_continue = true WHERE "+r.where())
		rigs = append(rigs, r)
	}
	// A paused project is never scheduled.
	paused := rigs[2]
	if _, err := h.App.Lifecycle().Pause(context.Background(), paused.pid.String()); err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]int{}
	for round := 0; round < 4; round++ {
		for _, id := range h.App.Engine().ScheduleOnce(context.Background(), 1) {
			seen[id]++
		}
	}
	if seen[rigs[0].execID] == 0 || seen[rigs[1].execID] == 0 {
		t.Fatalf("starvation: %v", seen)
	}
	if seen[paused.execID] != 0 {
		t.Fatal("paused execution scheduled")
	}
	for _, r := range rigs[:2] {
		if r.execStatus(t) == "RUNNING" && r.state(t) == "PHASE_PLAN" {
			t.Fatalf("scheduled execution did not progress (%s)", r.execStatus(t))
		}
	}
	_ = execution.ExecutionRunning
}
