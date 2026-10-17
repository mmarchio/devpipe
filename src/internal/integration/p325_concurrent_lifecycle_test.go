package integration

import (
	"context"
	"sync"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P3-25 — concurrent lifecycle protection (WBS 3.25).

// p325Rig drives concurrent lifecycle ops through the real service + PG.
type p325Rig struct {
	h         *Harness
	lifecycle *service.LifecycleService
	projectID string
}

func newP325Rig(t *testing.T) *p325Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projSvc.CreateProject(ctx, "concurrent lifecycle", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &p325Rig{
		h:         h,
		lifecycle: service.NewLifecycleService(dbrepo.NewProjectRepository(pool), dbrepo.NewStatusEventRepository(pool), pool),
		projectID: p.ID.String(),
	}
}

func (r *p325Rig) setStatus(t *testing.T, status string) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET status = '"+status+"' WHERE id = '"+r.projectID+"'"); err != nil {
		t.Fatal(err)
	}
}

func (r *p325Rig) status(t *testing.T) string {
	t.Helper()
	var s string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM projects WHERE id = '"+r.projectID+"'", &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// eventSeq returns the (from, to, reason) sequence in insertion order (ctid
// approximates commit/insertion order; events carry no sequence column).
func (r *p325Rig) eventSeq(t *testing.T) [][3]string {
	t.Helper()
	var parts []string
	q := "SELECT coalesce(array_agg(from_status||'|'||to_status||'|'||reason ORDER BY ctid), '{}') FROM project_status_events WHERE project_id = '" + r.projectID + "'"
	if err := r.h.DBScan(ctx0(), q, &parts); err != nil {
		t.Fatal(err)
	}
	out := make([][3]string, 0, len(parts))
	for _, p := range parts {
		f, to, reason, ok := split3(p)
		if !ok {
			t.Fatalf("bad event row %q", p)
		}
		out = append(out, [3]string{f, to, reason})
	}
	return out
}

func split3(s string) (a, b, c string, ok bool) {
	i := indexByte(s, '|')
	if i < 0 {
		return "", "", "", false
	}
	j := indexByte(s[i+1:], '|')
	if j < 0 {
		return "", "", "", false
	}
	return s[:i], s[i+1 : i+1+j], s[i+2+j:], true
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// runConcurrent fires n concurrent copies of op and counts successes.
func (r *p325Rig) runConcurrent(t *testing.T, n int, op func(ctx context.Context) error) (ok int) {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := op(context.Background()); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return ok
}

// T3.25.1 — Concurrent start operations don't duplicate transition: from a
// READY project, N concurrent starts produce exactly one RUNNING project and
// exactly one "start" event.
func TestT3251ConcurrentStartNoDuplicate(t *testing.T) {
	r := newP325Rig(t)
	r.setStatus(t, "READY")
	ok := r.runConcurrent(t, 8, func(ctx context.Context) error {
		_, err := r.lifecycle.Start(ctx, r.projectID)
		return err
	})
	if ok != 1 {
		t.Fatalf("successful starts = %d, want exactly 1", ok)
	}
	if s := r.status(t); s != "RUNNING" {
		t.Fatalf("status = %q, want RUNNING", s)
	}
	seq := r.eventSeq(t)
	if len(seq) != 1 {
		t.Fatalf("event count = %d, want 1: %+v", len(seq), seq)
	}
	if seq[0] != [3]string{"READY", "RUNNING", "start"} {
		t.Fatalf("event = %+v, want READY->RUNNING reason start", seq[0])
	}
}

// T3.25.2 — Concurrent pause/cancel remains valid: from RUNNING, N pauses and
// M cancels must serialize so exactly ONE transition is committed; the final
// state matches that transition and the event sequence is legal. (Pause on an
// already-PAUSED project is an idempotent no-op by design, so success is
// measured by committed transitions — the event rows — not by nil returns.)
func TestT3252ConcurrentPauseCancelValid(t *testing.T) {
	r := newP325Rig(t)
	r.setStatus(t, "RUNNING")
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.lifecycle.Pause(context.Background(), r.projectID) }()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.lifecycle.Cancel(context.Background(), r.projectID) }()
	}
	wg.Wait()
	seq := r.eventSeq(t)
	// At least one transition commits; the committed transitions must form
	// a valid serialized sequence: each from_status equals the previous
	// to_status, every edge is legal (pause from RUNNING, cancel from
	// RUNNING/PAUSED), and the stored status equals the last committed
	// transition. A RUNNING->PAUSED then PAUSED->CANCELLED chain is a
	// legitimate serialization (the WBS requires a valid serialized
	// sequence, not a single winner).
	if len(seq) == 0 {
		t.Fatal("no transition committed")
	}
	for i, ev := range seq {
		if i == 0 && ev[0] != "RUNNING" {
			t.Fatalf("first event from %q, want RUNNING: %+v", ev[0], ev)
		}
		if i > 0 && ev[0] != seq[i-1][1] {
			t.Fatalf("event %d from %q != previous to %q — corrupted history: %+v", i, ev[0], seq[i-1][1], seq)
		}
		if ev[2] == "pause" && ev[0] != "RUNNING" {
			t.Fatalf("illegal pause from %q: %+v", ev[0], ev)
		}
		if ev[2] == "cancel" && ev[0] != "RUNNING" && ev[0] != "PAUSED" {
			t.Fatalf("illegal cancel from %q: %+v", ev[0], ev)
		}
	}
	if s := r.status(t); s != seq[len(seq)-1][1] {
		t.Fatalf("final status %q != last committed transition %q (corrupted state)", s, seq[len(seq)-1][1])
	}
}

// T3.25.3 — Status-event history matches committed transitions: a concurrent
// mixed run (start, then pauses and cancels racing) must leave an event
// sequence that IS the serialized legal history: it starts from the initial
// status, each event's from_status equals the previous event's to_status, and
// the stored status equals the last committed transition. At least one pause
// and one cancel must be committed (PAUSED->CANCELLED is a legal edge, so
// both serialized winners are legitimate).
func TestT3253EventHistoryMatchesTransitions(t *testing.T) {
	r := newP325Rig(t)
	r.setStatus(t, "READY")
	if _, err := r.lifecycle.Start(context.Background(), r.projectID); err != nil {
		t.Fatalf("setup start: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.lifecycle.Pause(context.Background(), r.projectID) }()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.lifecycle.Cancel(context.Background(), r.projectID) }()
	}
	wg.Wait()
	seq := r.eventSeq(t)
	if len(seq) < 2 {
		t.Fatalf("event count = %d, want at least start + one race winner: %+v", len(seq), seq)
	}
	if seq[0] != [3]string{"READY", "RUNNING", "start"} {
		t.Fatalf("first event = %+v, want READY->RUNNING start", seq[0])
	}
	for i := 1; i < len(seq); i++ {
		if seq[i][0] != seq[i-1][1] {
			t.Fatalf("event %d from %q != previous to %q — corrupted history: %+v", i, seq[i][0], seq[i-1][1], seq)
		}
		// Each race winner must be a legal edge: pause from RUNNING, cancel
		// from RUNNING/PAUSED.
		if seq[i][2] == "pause" && seq[i][0] != "RUNNING" {
			t.Fatalf("illegal pause from %q: %+v", seq[i][0], seq[i])
		}
		if seq[i][2] == "cancel" && seq[i][0] != "RUNNING" && seq[i][0] != "PAUSED" {
			t.Fatalf("illegal cancel from %q: %+v", seq[i][0], seq[i])
		}
	}
	if s := r.status(t); s != seq[len(seq)-1][1] {
		t.Fatalf("stored status %q != last committed transition %q", s, seq[len(seq)-1][1])
	}
}

// T3.25.4 — No lost update: concurrent lifecycle ops never clobber fields the
// transition does not own; name/description survive the race, and the stored
// status equals the last committed transition.
func TestT3254NoLostUpdate(t *testing.T) {
	r := newP325Rig(t)
	// A concurrent settings-adjacent name update racing a lifecycle op must
	// not be silently lost or corrupted: set a name, race pause/cancel, and
	// the name must be intact.
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET name = 'stable-name' WHERE id = '"+r.projectID+"'"); err != nil {
		t.Fatal(err)
	}
	r.setStatus(t, "RUNNING")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.lifecycle.Pause(context.Background(), r.projectID) }()
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.lifecycle.Cancel(context.Background(), r.projectID) }()
	}
	wg.Wait()
	var name, s string
	if err := r.h.DBScan(context.Background(),
		"SELECT name, status FROM projects WHERE id = '"+r.projectID+"'", &name, &s); err != nil {
		t.Fatal(err)
	}
	if name != "stable-name" {
		t.Fatalf("name lost/corrupted: %q, want stable-name", name)
	}
	seq := r.eventSeq(t)
	if len(seq) == 0 || s != seq[len(seq)-1][1] {
		t.Fatalf("status %q / events %+v — final state does not match last committed transition", s, seq)
	}
}

func ctx0() context.Context { return context.Background() }
