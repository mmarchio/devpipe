package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
)

type memEvents struct {
	persistence.WebReadRepository
	mu  sync.Mutex
	evs map[uuid.UUID][]execution.Event
}

func (m *memEvents) add(p uuid.UUID, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < n; i++ {
		seq := int64(len(m.evs[p]) + 1)
		m.evs[p] = append(m.evs[p], execution.Event{ProjectID: p, ProjectSeq: seq, Type: execution.EventUnitDone,
			Payload: map[string]any{"work_unit_id": "u", "prompt": "secret prompt"}})
	}
}

func (m *memEvents) ListProjectEventsAfter(_ context.Context, p uuid.UUID, after int64, limit int) ([]execution.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []execution.Event
	for _, e := range m.evs[p] {
		if e.ProjectSeq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memEvents) MaxProjectSeq(_ context.Context, p uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.evs[p])), nil
}

func recv(t *testing.T, s *Subscription, n int) []int64 {
	t.Helper()
	var out []int64
	deadline := time.After(3 * time.Second)
	for len(out) < n {
		select {
		case e, ok := <-s.C:
			if !ok {
				return out
			}
			out = append(out, e.ProjectSeq)
		case <-deadline:
			t.Fatalf("timeout after %v", out)
		}
	}
	return out
}

// P17-42 (WBS 17.75 / 17.76 / 17.161) — project-scoped, resumable, ordered
// delivery; one poller per project; slow consumers disconnected.
func TestP1742LiveEventFeed(t *testing.T) {
	m := &memEvents{evs: map[uuid.UUID][]execution.Event{}}
	p, other := uuid.New(), uuid.New()
	m.add(p, 5)
	m.add(other, 3)
	f := NewLiveEventFeed(m, LiveFeedConfig{Interval: 5 * time.Millisecond, Buffer: 8})
	defer f.Stop()

	// Resume from seq 2 replays 3..5, then live events follow.
	s, err := f.Subscribe(context.Background(), p, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := recv(t, s, 3)
	if len(got) != 3 || got[0] != 3 || got[2] != 5 {
		t.Fatalf("replay %v", got)
	}
	m.add(p, 2)
	got = recv(t, s, 2)
	if got[0] != 6 || got[1] != 7 {
		t.Fatalf("live %v", got)
	}
	// A second subscriber shares the project poller.
	s2, _ := f.Subscribe(context.Background(), p, 7)
	if f.Pollers() != 1 || f.Subscribers(p) != 2 {
		t.Fatalf("pollers %d subs %d", f.Pollers(), f.Subscribers(p))
	}
	m.add(p, 1)
	if g := recv(t, s2, 1); g[0] != 8 {
		t.Fatalf("s2 %v", g)
	}
	if g := recv(t, s, 1); g[0] != 8 {
		t.Fatalf("s %v", g)
	}

	// Slow consumer: never reads; more events than its buffer → closed.
	slow, _ := f.Subscribe(context.Background(), p, 8)
	drained := make(chan []int64)
	go func() { drained <- recv(t, s, 20) }()
	m.add(p, 20)
	deadline := time.Now().Add(3 * time.Second)
	for slow.Reason() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if slow.Reason() != FeedClosedSlowConsumer {
		t.Fatalf("slow reason %q", slow.Reason())
	}
	// The healthy subscriber still receives everything in order.
	got = <-drained
	if len(got) != 20 {
		t.Fatalf("healthy subscriber got %v (%s)", got, s.Reason())
	}
	for i, seq := range got {
		if seq != int64(9+i) {
			t.Fatalf("order broken: %v", got)
		}
	}
	// Payload minimized.
	m.add(p, 1)
	select {
	case e := <-s.C:
		if e.Refs["work_unit_id"] != "u" {
			t.Fatalf("%+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	s.Close()
	s2.Close()
	deadline = time.Now().Add(time.Second)
	for f.Pollers() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.Pollers() != 0 {
		t.Fatal("poller kept after last subscriber left")
	}
	// Other projects' events never arrive on p.
	s3, _ := f.Subscribe(context.Background(), other, 0)
	if g := recv(t, s3, 3); len(g) != 3 {
		t.Fatal("other project")
	}
	f.Stop()
	if _, ok := <-s3.C; ok {
		t.Fatal("not closed on stop")
	}
	if s3.Reason() != FeedClosedShutdown {
		t.Fatal(s3.Reason())
	}
}
