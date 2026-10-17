package resilience

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// T18.94 — the limiter bounds concurrent holders.
func TestT1894LimiterBounds(t *testing.T) {
	l := NewPriorityLimiter(2)
	ctx := context.Background()
	_, r1, err := l.Acquire(ctx, Active)
	if err != nil {
		t.Fatal(err)
	}
	_, r2, err := l.Acquire(ctx, Active)
	if err != nil {
		t.Fatal(err)
	}
	if l.InUse() != 2 {
		t.Fatalf("in use %d", l.InUse())
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, _, err := l.Acquire(short, Active); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third active admitted past capacity: %v", err)
	}
	r1()
	r1() // idempotent
	if _, r3, err := l.Acquire(ctx, Active); err != nil {
		t.Fatal(err)
	} else {
		r3()
	}
	r2()
	if l.InUse() != 0 {
		t.Fatalf("in use after release %d", l.InUse())
	}
}

// T18.95 — speculative work is refused when the limiter is full or active
// work is waiting, and is preempted (cancelled first) when active work
// needs its slot.
func TestT1895SpeculativeYields(t *testing.T) {
	l := NewPriorityLimiter(1)
	ctx := context.Background()
	sctx, sRelease, err := l.Acquire(ctx, Speculative)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Acquire(ctx, Speculative); !errors.Is(err, ErrSaturated) {
		t.Fatalf("second speculative: %v", err)
	}
	got := make(chan error, 1)
	go func() {
		_, r, err := l.Acquire(ctx, Active)
		if err == nil {
			r()
		}
		got <- err
	}()
	select {
	case <-sctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("speculative holder not preempted")
	}
	if !errors.Is(context.Cause(sctx), ErrPreempted) {
		t.Fatalf("cause %v", context.Cause(sctx))
	}
	// While the active request waits, new speculative work is refused.
	if _, _, err := l.Acquire(ctx, Speculative); !errors.Is(err, ErrSaturated) {
		t.Fatalf("speculative admitted while active waits: %v", err)
	}
	sRelease()
	if err := <-got; err != nil {
		t.Fatalf("active: %v", err)
	}
	// Active work is never preempted.
	actx, aRelease, _ := l.Acquire(ctx, Active)
	if _, _, err := l.Acquire(ctx, Speculative); !errors.Is(err, ErrSaturated) {
		t.Fatal(err)
	}
	if actx.Err() != nil {
		t.Fatal("active holder cancelled")
	}
	aRelease()
}

// Speculative priority travels on the context.
func TestPriorityFromContext(t *testing.T) {
	ctx := context.Background()
	if PriorityOf(ctx) != Active || PriorityOf(WithSpeculative(ctx)) != Speculative {
		t.Fatal("priority from context")
	}
}

// Capacity 0 is unlimited; a nil limiter admits everything.
func TestLimiterUnlimited(t *testing.T) {
	var nl *PriorityLimiter
	for _, l := range []*PriorityLimiter{NewPriorityLimiter(0), nl} {
		for i := 0; i < 10; i++ {
			if _, r, err := l.Acquire(context.Background(), Speculative); err != nil {
				t.Fatal(err)
			} else {
				defer r()
			}
		}
	}
}

// Concurrent mixed use never exceeds capacity (run with -race).
func TestLimiterConcurrent(t *testing.T) {
	l := NewPriorityLimiter(3)
	var mu sync.Mutex
	cur, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := Active
			if i%2 == 0 {
				p = Speculative
			}
			ctx, r, err := l.Acquire(context.Background(), p)
			if err != nil {
				return
			}
			mu.Lock()
			cur++
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Millisecond):
			}
			mu.Lock()
			cur--
			mu.Unlock()
			r()
		}(i)
	}
	wg.Wait()
	if peak > 3 || l.InUse() != 0 {
		t.Fatalf("peak %d in use %d", peak, l.InUse())
	}
}
