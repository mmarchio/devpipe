package integration

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"overseer/internal/continuation"
	"overseer/internal/gateway/hermes/hermestest"
)

func (r *p14Rig) lifecycle(t *testing.T, op string) {
	t.Helper()
	if code, raw, _ := p9HTTP(t, http.MethodPost, r.base+"/"+op, ""); code != http.StatusOK {
		t.Fatalf("%s: %d %s", op, code, raw)
	}
}

// P14-76 — WBS 14.115 + 14.116 + 14.63-14.65.
func TestP14115PauseResumeCancel(t *testing.T) {
	t.Run("pause_after_session_creation", func(t *testing.T) {
		r := p14NewRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 61000)
		paused := false
		r.cs().SetStepHook(func(c *continuation.Continuation) bool {
			if c.Status == continuation.StatusRestoringContext && !paused {
				paused = true
				r.lifecycle(t, "pause")
			}
			return true
		})
		news := r.ops("new")
		r.process(t)
		c := r.unitContinuations(t)[0]
		if c.Status != continuation.StatusRestoringContext || r.ops("new") != news+1 || r.ops("msg") != 1 {
			t.Fatalf("14.64 position not preserved: %s", c.Status)
		}
		r.process(t) // still paused: nothing happens
		if r.cont(t, c.ID).Version != c.Version {
			t.Fatal("14.64 advanced while paused")
		}
		r.cs().SetStepHook(nil)
		r.lifecycle(t, "resume")
		r.reply(p14Report("complete", []string{"A", "B"}, nil, "x"), 3000)
		r.process(t)
		c2 := r.cont(t, c.ID)
		if c2.Status != continuation.StatusCompleted || r.ops("new") != news+1 {
			t.Fatalf("14.115 resume: %s, sessions %d", c2.Status, r.ops("new")-news)
		}
	})
	t.Run("cancel_while_ready", func(t *testing.T) {
		r := p14NewRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 10000)
		r.cs().SetStepHook(func(c *continuation.Continuation) bool {
			if c.Status == continuation.StatusReady {
				r.lifecycle(t, "cancel")
			}
			return true
		})
		msgs := r.ops("msg")
		_ = r.cs().ProcessProject(t.Context(), r.pos.ProjectID)
		c := r.unitContinuations(t)[0]
		if c.Status != continuation.StatusCancelled || r.ops("msg") != msgs {
			t.Fatalf("14.116 %s, %d sends", c.Status, r.ops("msg")-msgs)
		}
	})
	t.Run("pause_while_waiting", func(t *testing.T) {
		r := p14NewRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B", "C"}, "B"), 10000)
		hold := make(chan struct{})
		content := p14Report("incomplete", []string{"A", "B"}, []string{"C"}, "C")
		r.g.Script(hermestest.Behavior{Content: &content, Usage: p14Usage(12000), Hold: hold})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.cs().ProcessProject(t.Context(), r.pos.ProjectID) }()
		deadline := time.Now().Add(10 * time.Second)
		for r.ops("msg") < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		r.lifecycle(t, "pause")
		close(hold)
		wg.Wait()
		cs := r.unitContinuations(t)
		if len(cs) != 2 || cs[0].Status != continuation.StatusCompleted || cs[1].Status != continuation.StatusPending {
			t.Fatalf("14.65 response not persisted while paused: %+v", cs)
		}
		r.process(t)
		if r.cont(t, cs[1].ID).Status != continuation.StatusPending || r.ops("msg") != 2 {
			t.Fatal("14.65 continuation started while paused")
		}
		r.lifecycle(t, "resume")
		r.reply(p14Report("complete", []string{"A", "B", "C"}, nil, "x"), 13000)
		r.process(t)
		if r.cont(t, cs[1].ID).Status != continuation.StatusCompleted {
			t.Fatal("14.65 resume")
		}
	})
}
