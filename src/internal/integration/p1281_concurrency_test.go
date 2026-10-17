package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
	"overseer/internal/worker"
)

// P12-81 (WBS 12.55, 12.78): projects A and B dispatch concurrently, each
// through its own session; correlation, usage and CWD stay isolated.
func TestP1278MultiProject(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	g, proxy := p12Gateway(t)
	h := NewHarnessWith(t, p12Mutate(srv, proxy.URL()))
	a := p12Configure(t, p11Build(t, h, srv, true), "/repos/a")
	b := p12Configure(t, p11Build(t, h, srv, true), "/repos/b")
	a.g, b.g = g, g
	ctx := context.Background()
	sa, sb := a.newSession(t), b.newSession(t)
	a.cd(t, "/repos/a")
	b.cd(t, "/repos/b")
	wpa, wpb := a.prompt(t, ""), b.prompt(t, "")
	g.Script(hermestest.Behavior{Delay: 200 * time.Millisecond}, hermestest.Behavior{Delay: 200 * time.Millisecond})
	var wg sync.WaitGroup
	results := map[string]*service.DispatchResult{}
	var mu sync.Mutex
	for name, rr := range map[string]*p12Rig{"a": a, "b": b} {
		wg.Add(1)
		go func(name string, rr *p12Rig) {
			defer wg.Done()
			p := wpa
			if name == "b" {
				p = wpb
			}
			res, err := rr.ws().Dispatch(ctx, rr.req(p, 1))
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			mu.Lock()
			results[name] = res
			mu.Unlock()
		}(name, rr)
	}
	wg.Wait()
	ra, rb := results["a"], results["b"]
	if ra == nil || rb == nil || ra.Dispatch.Status != worker.DispatchCompleted || rb.Dispatch.Status != worker.DispatchCompleted {
		t.Fatal("concurrent dispatches failed")
	}
	// A prompt → session A, B prompt → session B, responses correlated.
	for _, c := range []struct {
		r   *p12Rig
		res *service.DispatchResult
		sid string
		wp  string
	}{{a, ra, sa.ProviderSessionID, wpa.Content}, {b, rb, sb.ProviderSessionID, wpb.Content}} {
		msgs := g.MessagesFor(c.sid)
		if len(msgs) != 1 || msgs[0].RequestID != c.res.Dispatch.RequestID.String() || msgs[0].Content != c.wp {
			t.Fatalf("T12.55.1/T12.55.2 session %s got %+v", c.sid, msgs)
		}
		if !strings.Contains(c.res.Response.Content, c.res.Dispatch.RequestID.String()) {
			t.Fatal("T12.56.3 response attached to the wrong dispatch")
		}
		obs, _ := c.r.ws().UsageObservations(ctx, c.r.pos.ProjectID, nil)
		if len(obs) != 1 || *obs[0].DispatchID != c.res.Dispatch.ID {
			t.Fatal("usage not isolated")
		}
	}
	if *a.session(t, sa.ID).WorkingDirectory != "/repos/a" || *b.session(t, sb.ID).WorkingDirectory != "/repos/b" {
		t.Fatal("CWD not isolated")
	}
	// T12.55.3: B cannot read or address A's session.
	if _, err := b.ws().Session(ctx, b.pos.ProjectID, sa.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatal("cross-project session visible")
	}
	if _, err := h.DBExec(ctx, "UPDATE worker_dispatches SET session_id = '"+sa.ID.String()+"' WHERE id = '"+rb.Dispatch.ID.String()+"'"); err == nil {
		t.Fatal("cross-project session reference accepted")
	}
}

// P12-81 (WBS 12.56, 12.79): two sends to one session. REJECT: the second
// is refused before anything is persisted or sent; WAIT: it runs after the
// first; the gateway never sees interleaved requests.
func TestP1279SameSessionConcurrency(t *testing.T) {
	r := p12NewRig(t)
	ctx := context.Background()
	r.newSession(t)
	r.cd(t, "/repos/project-a")
	wp1, wp2 := r.prompt(t, ""), r.prompt(t, "second")
	hold := make(chan struct{})
	r.g.Script(hermestest.Behavior{Hold: hold})
	first := make(chan *service.DispatchResult, 1)
	go func() {
		res, _ := r.ws().Dispatch(ctx, r.req(wp1, 1))
		first <- res
	}()
	for len(r.g.Messages()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := r.ws().Dispatch(ctx, r.req(wp2, 1)); !errors.Is(err, service.ErrSessionBusy) {
		t.Fatalf("T12.56.1/T12.57.2 %v", err)
	}
	if n := p9Count(t, r.h, "worker_dispatches", "worker_prompt_id = '"+wp2.ID.String()+"'"); n != 0 || len(r.g.Messages()) != 1 {
		t.Fatal("rejected send persisted or transmitted")
	}
	close(hold)
	if res := <-first; res.Dispatch.Status != worker.DispatchCompleted {
		t.Fatal("first request did not keep the session")
	}

	// WAIT policy: queued behind the lease, then sent.
	if err := r.ws().UpdateWorkerSettings(ctx, r.pos.ProjectID, worker.Settings{SessionPolicy: worker.PolicySingleActive,
		ConcurrentSend: worker.SendWait, Lease: 5 * time.Second, Wait: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	r.g.Script(hermestest.Behavior{Delay: 300 * time.Millisecond}, hermestest.Behavior{})
	var wg sync.WaitGroup
	res := make([]*service.DispatchResult, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := wp1
			if i == 1 {
				p = wp2
				time.Sleep(50 * time.Millisecond)
			}
			out, err := r.ws().Dispatch(ctx, r.req(p, 2))
			if err != nil {
				t.Errorf("wait policy: %v", err)
			}
			res[i] = out
		}(i)
	}
	wg.Wait()
	if res[0] == nil || res[1] == nil || res[0].Dispatch.Status != worker.DispatchCompleted || res[1].Dispatch.Status != worker.DispatchCompleted {
		t.Fatal("T12.56.1 queued send not completed")
	}
	if !res[1].Dispatch.StartedAt.After(*res[0].Dispatch.CompletedAt) && !res[1].Dispatch.StartedAt.Equal(*res[0].Dispatch.CompletedAt) {
		t.Fatal("12.79 interleaved conversational requests")
	}
}

// P12-82 (WBS 12.80): two overseer instances share one session through
// the DB lease.
func TestP1280MultiInstanceLease(t *testing.T) {
	r, boot := p12RestartRig(t)
	ctx := context.Background()
	s := r.newSession(t)
	r.cd(t, "/repos/a")
	wp1, wp2 := r.prompt(t, ""), r.prompt(t, "b")
	hB := boot()
	b := &p12Rig{p11Rig: &p11Rig{h: hB, pos: r.pos, units: r.units}, runtime: r.runtime, g: r.g}
	hold := make(chan struct{})
	r.g.Script(hermestest.Behavior{Hold: hold})
	first := make(chan *service.DispatchResult, 1)
	go func() {
		res, _ := r.ws().Dispatch(ctx, r.req(wp1, 1))
		first <- res
	}()
	for len(r.g.Messages()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := b.ws().Dispatch(ctx, b.req(wp2, 1)); !errors.Is(err, service.ErrSessionBusy) {
		t.Fatalf("T12.80.2 instance B dispatched concurrently: %v", err)
	}
	close(hold)
	if res := <-first; res.Dispatch.Status != worker.DispatchCompleted || res.Dispatch.Owner != r.ws().Owner() {
		t.Fatal("T12.80.1")
	}
	// A crashed holder's expired lease is recoverable by instance B.
	if _, err := r.h.DBExec(ctx, `INSERT INTO worker_session_leases (session_id, project_id, owner, acquired_at, expires_at)
		VALUES ('`+s.ID.String()+`', '`+r.pos.ProjectID.String()+`', 'crashed', now() - interval '2 minutes', now() - interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	res := b.dispatch(t, wp2, 1)
	if res.Dispatch.Status != worker.DispatchCompleted || res.Dispatch.Owner != b.ws().Owner() || res.Dispatch.SessionID != s.ID {
		t.Fatalf("T12.80.3 %+v", res.Err)
	}
	// T12.80.4: every prompt reached the gateway exactly once.
	for _, wp := range []string{wp1.Content, wp2.Content} {
		n := 0
		for _, m := range r.g.Messages() {
			if m.Content == wp {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("prompt sent %d times", n)
		}
	}
}
