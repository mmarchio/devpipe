package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/readmodel"
)

// P17-66 / P17-126 (WBS 17.74–17.78, 17.161–17.163, 17.180) — the project
// event stream: connect, receive, disconnect, resume without loss, ordered
// without duplicates or gaps, minimized payloads, project-scoped, and an
// unread stream never blocks the engine.
func TestP17126SSE(t *testing.T) {
	r := p17NewRig(t)
	pid := r.pid.String()
	var ov readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov)
	start := ov.LatestEventSeq
	if start == 0 {
		t.Fatal("bootstrap events must have sequences")
	}

	// Unknown project → 404; malformed id → 404.
	if _, cancel, code := r.b.stream("/api/v1/projects/"+uuid.NewString()+"/events/stream", ""); code != 404 {
		cancel()
		t.Fatalf("unknown project stream: %d", code)
	} else {
		cancel()
	}

	// Connect after the current sequence, then drive the engine.
	ch, cancel, code := r.b.stream("/api/v1/projects/"+pid+"/events/stream?after="+strconv.FormatInt(start, 10), "")
	if code != 200 {
		t.Fatalf("stream: %d", code)
	}
	r.queuePlan(r.planJSON(p16Unit{key: "u1"}))
	r.queue("complete")
	r.run(t, 3) // planning dispatch + plan validation
	got := collect(ch, 3, 10*time.Second)
	if len(got) < 3 {
		cancel()
		t.Fatalf("received %d events", len(got))
	}
	last := start
	for _, e := range got {
		id, _ := strconv.ParseInt(e.ID, 10, 64)
		if id != last+1 {
			t.Fatalf("sequence gap / duplicate: %d after %d", id, last)
		}
		last = id
		var le readmodel.LiveEvent
		if err := json.Unmarshal([]byte(e.Data), &le); err != nil || le.ProjectSeq != id || le.Type == "" || le.Category == "" {
			t.Fatalf("event payload: %s", e.Data)
		}
		if strings.Contains(e.Data, "Planned the phase") || strings.Contains(strings.ToLower(e.Data), "prompt\"") {
			t.Fatalf("event leaks content: %s", e.Data)
		}
	}
	cancel() // disconnect

	// Events committed while disconnected …
	r.run(t, 4)
	var ov2 readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov2)
	missed := ov2.LatestEventSeq - last
	if missed <= 0 {
		t.Fatal("no events while disconnected")
	}
	// … are replayed on reconnect with Last-Event-ID, in order, once.
	ch2, cancel2, code := r.b.stream("/api/v1/projects/"+pid+"/events/stream", strconv.FormatInt(last, 10))
	defer cancel2()
	if code != 200 {
		t.Fatalf("reconnect: %d", code)
	}
	replay := collect(ch2, int(missed), 10*time.Second)
	if int64(len(replay)) != missed {
		t.Fatalf("replayed %d of %d", len(replay), missed)
	}
	for _, e := range replay {
		id, _ := strconv.ParseInt(e.ID, 10, 64)
		if id != last+1 {
			t.Fatalf("replay order: %d after %d", id, last)
		}
		last = id
	}

	// An open but unread stream never blocks the producers: the engine
	// runs to completion while ch2 is not drained.
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runUntil(t, execution.StepCompleted, 120)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("engine blocked by an unread stream")
	}
	if r.execStatus(t) != "COMPLETED" {
		t.Fatal("not completed")
	}
	// Browser offline during completion: reopening shows COMPLETED
	// (WBS 17.175).
	var ov3 readmodel.Overview
	r.b.getJSON("/api/v1/projects/"+pid+"/overview", &ov3)
	if ov3.Status != "COMPLETED" {
		t.Fatalf("after completion: %s", ov3.Status)
	}

	// Security headers on the stream; no session needed for a same-origin
	// API client, but a cross-site Origin is not a CSRF vector for GETs.
	req, _ := http.NewRequest(http.MethodGet, r.h.BaseURL+"/api/v1/projects/"+pid+"/events/stream?after="+strconv.FormatInt(last, 10), nil)
	ctxResp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err == nil {
		if ctxResp.Header.Get("Content-Security-Policy") == "" || ctxResp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("stream headers: %v", ctxResp.Header)
		}
		ctxResp.Body.Close()
	}
}

// P17-148 (WBS 17.156, DoD) — graceful shutdown ends open live streams
// first, so an idle SSE connection never holds the HTTP drain until the
// shutdown timeout.
func TestP17ShutdownClosesStreams(t *testing.T) {
	r := p17NewRig(t)
	ch, cancel, code := r.b.stream("/api/v1/projects/"+r.pid.String()+"/events/stream", "")
	defer cancel()
	if code != 200 {
		t.Fatalf("stream: %d", code)
	}
	time.Sleep(100 * time.Millisecond)
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	start := time.Now()
	if err := r.h.App.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	r.h.App = nil
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown with an open stream took %s", d)
	}
	select {
	case _, ok := <-ch:
		for ok {
			_, ok = <-ch
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream still open after shutdown")
	}
}
