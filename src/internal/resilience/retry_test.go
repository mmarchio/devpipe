package resilience

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }
func (f *fakeClock) sleep(_ context.Context, d time.Duration) error {
	f.t = f.t.Add(d)
	return nil
}

func reg(t *testing.T, p Policy, b *Budget) (*Registry, *fakeClock) {
	t.Helper()
	r, err := NewRegistry([]Policy{p}, b)
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeClock{t: time.Unix(0, 0)}
	r.SetClock(c.sleep, c.now)
	return r, c
}

var errTransient = Mark(errors.New("blip"), Transient)

// T18.191 — backoff stays within the configured maximum.
func TestT18191BackoffBounded(t *testing.T) {
	p := Policy{Name: "p", MaxAttempts: 10, Base: 10 * time.Millisecond, Max: 300 * time.Millisecond, Jitter: 1}
	rng := rand.New(rand.NewSource(1))
	prev := time.Duration(0)
	for a := 1; a <= 20; a++ {
		d := p.Backoff(a, rng)
		if d > p.Max || d < 0 {
			t.Fatalf("attempt %d: %s", a, d)
		}
		prev = d
	}
	_ = prev
	if (Policy{Name: "q", MaxAttempts: 1, Base: 10, Max: 80}).Backoff(3, nil) != 40 {
		t.Fatal("exponential")
	}
}

// T18.189 — counts and backoff are consistent across uses of a policy.
func TestT18189PolicyConsistent(t *testing.T) {
	r, _ := reg(t, Policy{Name: "p", MaxAttempts: 3, Base: time.Millisecond, Max: 4 * time.Millisecond}, nil)
	for i := 0; i < 3; i++ {
		calls := 0
		res, err := r.Do(context.Background(), "p", func(context.Context, int) error { calls++; return errTransient })
		if !errors.Is(err, errTransient) || res.Attempts != 3 || calls != 3 || res.Waited != 3*time.Millisecond {
			t.Fatalf("run %d: %+v %v", i, res, err)
		}
	}
	if _, err := r.Do(context.Background(), "nope", nil); err == nil {
		t.Fatal("unknown policy accepted")
	}
	for _, p := range DefaultPolicies() {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewRegistry([]Policy{{Name: "bad", MaxAttempts: 0}}, nil); err == nil {
		t.Fatal("invalid policy accepted")
	}
}

// T18.190 — ambiguous (and non-transient) errors bypass retry.
func TestT18190DoBypassesAmbiguous(t *testing.T) {
	r, _ := reg(t, Policy{Name: "p", MaxAttempts: 5, Base: time.Millisecond, Max: time.Millisecond}, nil)
	for _, e := range []error{Mark(errors.New("sent?"), Ambiguous), Mark(errors.New("p"), Permanent), errors.New("plain"),
		Mark(errors.New("disk"), OperatorRequired)} {
		calls := 0
		_, err := r.Do(context.Background(), "p", func(context.Context, int) error { calls++; return e })
		if calls != 1 || !errors.Is(err, e) {
			t.Fatalf("%v retried %d times", e, calls)
		}
	}
	calls := 0
	res, err := r.Do(context.Background(), "p", func(_ context.Context, a int) error {
		calls++
		if a < 3 {
			return errTransient
		}
		return nil
	})
	if err != nil || res.Attempts != 3 {
		t.Fatalf("eventual success: %+v %v", res, err)
	}
}

// T18.192 — a persistent outage cannot create unbounded retry traffic: the
// shared budget stops retries across operations.
func TestT18192RetryBudget(t *testing.T) {
	b := NewBudget(5, 0)
	r, c := reg(t, Policy{Name: "p", MaxAttempts: 100, Base: time.Millisecond, Max: time.Millisecond}, b)
	b.SetNow(c.now)
	total := 0
	for i := 0; i < 10; i++ {
		_, err := r.Do(context.Background(), "p", func(context.Context, int) error { total++; return errTransient })
		if err == nil {
			t.Fatal("success")
		}
	}
	if total != 10+5 {
		t.Fatalf("calls %d, want 15 (10 first attempts + 5 budgeted retries)", total)
	}
	_, err := r.Do(context.Background(), "p", func(context.Context, int) error { return errTransient })
	if !errors.Is(err, ErrBudgetExhausted) || !errors.Is(err, errTransient) {
		t.Fatalf("budget error %v", err)
	}
}

// T18.193 — five retries cannot exceed the overall time budget.
func TestT18193OverallBudget(t *testing.T) {
	r, c := reg(t, Policy{Name: "p", MaxAttempts: 6, Base: 100 * time.Millisecond, Max: time.Second, OverallBudget: 500 * time.Millisecond}, nil)
	start := c.t
	res, err := r.Do(context.Background(), "p", func(context.Context, int) error {
		c.t = c.t.Add(50 * time.Millisecond) // each attempt takes time too
		return errTransient
	})
	if !errors.Is(err, ErrBudgetExhausted) || c.t.Sub(start) >= 500*time.Millisecond || res.Attempts >= 6 {
		t.Fatalf("elapsed %s attempts %d err %v", c.t.Sub(start), res.Attempts, err)
	}
}

func TestDoHonoursContext(t *testing.T) {
	r, err := NewRegistry([]Policy{{Name: "p", MaxAttempts: 5, Base: time.Hour, Max: time.Hour}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := r.Do(ctx, "p", func(context.Context, int) error { return errTransient })
	if res.Attempts != 1 || !errors.Is(err, errTransient) {
		t.Fatalf("%+v %v", res, err)
	}
}
