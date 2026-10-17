package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/service"
)

// p15RestartRig builds the rig on a dedicated database; boot() starts
// another overseer instance (restart or second instance) on it.
func p15RestartRig(t *testing.T, phases []p15Phase, extra func(*config.Config)) (*p15Rig, func(func(*config.Config)) *Harness) {
	t.Helper()
	srv := providertest.New()
	t.Cleanup(srv.Close)
	g, proxy := p12Gateway(t)
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })
	boot := func(more func(*config.Config)) *Harness {
		h := NewHarnessOnWith(t, base, dbName, p15Mutate(srv, proxy.URL(), func(c *config.Config) {
			if extra != nil {
				extra(c)
			}
			if more != nil {
				more(c)
			}
		}))
		t.Cleanup(h.Close)
		return h
	}
	h := boot(nil)
	return p15Build(t, h, g, srv, phases), boot
}

func (r *p15Rig) eventCounts(t *testing.T) map[string]int {
	t.Helper()
	rows, err := r.h.App.DB().Raw().Query(context.Background(), "SELECT type, count(*) FROM execution_events WHERE "+r.where()+" GROUP BY type")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out[k] = n
	}
	return out
}

func (r *p15Rig) assertSingleBootstrap(t *testing.T) {
	t.Helper()
	w := r.where()
	for q, want := range map[string]int{
		"SELECT count(*) FROM executions WHERE " + w:                                                                     1,
		"SELECT count(*) FROM executions WHERE status = 'RUNNING' AND " + w:                                              1,
		"SELECT count(*) FROM execution_configuration_snapshots WHERE " + w:                                              1,
		"SELECT count(*) FROM runtime_state WHERE " + w:                                                                  1,
		"SELECT count(*) FROM execution_bootstrap_checkpoints c JOIN executions e ON e.id = c.execution_id WHERE e." + w: 1,
		"SELECT count(*) FROM state_transition_history WHERE outcome = 'BOOTSTRAP' AND " + w:                             1,
	} {
		if n := r.count(t, q); n != want {
			t.Fatalf("%s = %d, want %d", q, n, want)
		}
	}
	// No duplicate logical state files or versions (each derived file v1).
	if n := r.count(t, "SELECT count(*) FROM state_files WHERE current_version <> 1 AND "+w); n != 0 {
		t.Fatalf("state files with duplicate versions: %d", n)
	}
	ev := r.eventCounts(t)
	for _, typ := range []execution.EventType{execution.EventConfigurationSnapshotted, execution.EventStateFilesInitialized, execution.EventRuntimeInitialized,
		execution.EventContextPrepared, execution.EventPromptPrepared, execution.EventBootstrapCompleted, execution.EventStarted} {
		if ev[string(typ)] != 1 {
			t.Fatalf("%s events = %d (%v)", typ, ev[string(typ)], ev)
		}
	}
	r.noWorker(t)
}

// TestP15165ConcurrentBootstrap — WBS 15.165 / 15.4.4 / 15.8 / 15.7: two
// overseer instances bootstrap one project simultaneously.
func TestP15165ConcurrentBootstrap(t *testing.T) {
	r, boot := p15RestartRig(t, p15DefaultPhases, nil)
	h2 := boot(nil)
	svcs := []*service.BootstrapService{r.h.App.Bootstrap(), h2.App.Bootstrap()}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i := range svcs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svcs[i].Bootstrap(context.Background(), r.pid)
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if be, isBE := execution.AsBootstrapError(err); !isBE || be.Code != execution.CodeConcurrentBootstrap {
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok == 0 {
		t.Fatal("no instance bootstrapped")
	}
	r.assertSingleBootstrap(t)
	// Idempotent re-request (WBS 15.7): same execution, no new events.
	before := r.eventCounts(t)
	res1 := r.bootstrap(t)
	res2, err := h2.App.Bootstrap().Bootstrap(context.Background(), r.pid)
	if err != nil || res1.Execution.ID != res2.Execution.ID || res1.Outcome != service.OutcomeAlreadyRunning {
		t.Fatalf("idempotency %v %+v", err, res2)
	}
	if after := r.eventCounts(t); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("events changed %v → %v", before, after)
	}
}

// TestP15132Stress — WBS 15.132: many concurrent bootstrap requests across
// two instances against one READY project.
func TestP15132Stress(t *testing.T) {
	r, boot := p15RestartRig(t, []p15Phase{{1, "P", "EXECUTING", 2}}, nil)
	plan := r.plan(t, 1, 2, "READY")
	r.unit(t, plan, 1, "A", "DONE")
	r.unit(t, plan, 2, "B", "READY")
	h2 := boot(nil)
	svcs := []*service.BootstrapService{r.h.App.Bootstrap(), h2.App.Bootstrap()}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svcs[i%2].Bootstrap(context.Background(), r.pid)
			if err != nil {
				if be, ok := execution.AsBootstrapError(err); !ok || be.Code != execution.CodeConcurrentBootstrap {
					t.Errorf("request %d: %v", i, err)
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	// A request that lost every race may have left the execution
	// mid-flight on the claim; finish it explicitly.
	r.bootstrap(t)
	r.assertSingleBootstrap(t)
}
