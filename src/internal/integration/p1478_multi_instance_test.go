package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"overseer/internal/continuation"
	"overseer/internal/dbrepo"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/persistence"
)

// P14-78 — WBS 14.54 + 14.92 + 14.93.
func TestP1492MultiInstance(t *testing.T) {
	t.Run("one_owner", func(t *testing.T) {
		r, boot := p14RestartRig(t)
		hB := boot()
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 10000)
		hold := make(chan struct{})
		content := p14Report("complete", []string{"A", "B"}, nil, "x")
		r.g.Script(hermestest.Behavior{Content: &content, Usage: p14Usage(11000), Hold: hold})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.cs().ProcessProject(context.Background(), r.pos.ProjectID) }()
		deadline := time.Now().Add(10 * time.Second)
		for r.ops("msg") < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		paths := len(r.g.Paths())
		// Instance B finds the continuation leased by A (in flight): it
		// claims nothing and never calls Hermes.
		if err := hB.App.Continuations().ProcessProject(context.Background(), r.pos.ProjectID); err != nil {
			t.Fatal(err)
		}
		if len(r.g.Paths()) != paths {
			t.Fatalf("14.92 instance B called Hermes: %v", r.g.Paths()[paths:])
		}
		close(hold)
		wg.Wait()
		if c := r.unitContinuations(t)[0]; c.Status != continuation.StatusCompleted || r.ops("msg") != 2 {
			t.Fatalf("14.92 %s", c.Status)
		}
	})
	t.Run("lease_expiry", func(t *testing.T) {
		r, boot := p14RestartRig(t)
		hB := boot()
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 57600)
		c := r.stopAt(t, continuation.StatusReady) // A confirmed S2 + CWD, then died holding the lease
		r.force(t, "UPDATE continuations SET lease_owner = 'instance-a', lease_expires_at = now() + interval '1 hour' WHERE id = '"+c.ID.String()+"'")
		news, msgs := r.ops("new"), r.ops("msg")
		if err := hB.App.Continuations().ProcessProject(context.Background(), r.pos.ProjectID); err != nil {
			t.Fatal(err)
		}
		if r.cont(t, c.ID).Status != continuation.StatusReady || r.ops("msg") != msgs {
			t.Fatal("14.92 B executed a leased continuation")
		}
		r.force(t, "UPDATE continuations SET lease_expires_at = now() - interval '1 second' WHERE id = '"+c.ID.String()+"'")
		r.reply(p14Report("complete", []string{"A", "B"}, nil, "x"), 3000)
		if err := hB.App.Continuations().ProcessProject(context.Background(), r.pos.ProjectID); err != nil {
			t.Fatal(err)
		}
		got := r.cont(t, c.ID)
		if got.Status != continuation.StatusCompleted || r.ops("new") != news || r.ops("msg") != msgs+1 || got.LeaseOwner != "" {
			t.Fatalf("14.93 %s new=%d msg=%d", got.Status, r.ops("new")-news, r.ops("msg")-msgs)
		}
	})
	t.Run("versioned_advance", func(t *testing.T) { // 14.54
		r, boot := p14RestartRig(t)
		hB := boot()
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 10000)
		c := r.unitContinuations(t)[0]
		repos := []persistence.ContinuationRepository{dbrepo.NewContinuationRepository(r.h.App.DB().Raw()), dbrepo.NewContinuationRepository(hB.App.DB().Raw())}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, repo := range repos {
			wg.Add(1)
			go func(i int, repo persistence.ContinuationRepository) {
				defer wg.Done()
				_, errs[i] = repo.Advance(context.Background(), c.ID, c.Version, continuation.StatusDeciding, "race", persistence.ContinuationMutation{}, time.Now())
			}(i, repo)
		}
		wg.Wait()
		ok, stale := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, persistence.ErrStaleVersion):
				stale++
			}
		}
		if ok != 1 || stale != 1 {
			t.Fatalf("14.54 %v", errs)
		}
		if h := r.history(t, c.ID); len(h) != 2 {
			t.Fatalf("14.53 history %v", h)
		}
	})
}
