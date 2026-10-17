package resilience

import (
	"context"
	"errors"
	"sync"
)

// Priority of work competing for a bounded resource (WBS 18.94 / 18.95).
type Priority int

const (
	// Active work serves a running execution.
	Active Priority = iota
	// Speculative work (Phase 11 precompute) yields to active work.
	Speculative
)

type speculativeKey struct{}

// WithSpeculative marks ctx as carrying speculative work.
func WithSpeculative(ctx context.Context) context.Context {
	return context.WithValue(ctx, speculativeKey{}, true)
}

// PriorityOf returns the priority ctx carries (Active unless marked).
func PriorityOf(ctx context.Context) Priority {
	if v, _ := ctx.Value(speculativeKey{}).(bool); v {
		return Speculative
	}
	return Active
}

var (
	// ErrSaturated: no capacity for speculative work right now (full, or
	// active work is waiting). The caller defers the work.
	ErrSaturated = errors.New("resilience: capacity reserved for active work")
	// ErrPreempted is the cancellation cause of a speculative holder whose
	// slot active work needs.
	ErrPreempted = errors.New("resilience: speculative work preempted by active work")
)

// PriorityLimiter bounds concurrent use of a resource. Active work waits
// for a slot (bounded by its context) and preempts speculative holders;
// speculative work never waits — it is refused when the limiter is full or
// active work is waiting, and it is cancelled first under pressure.
// Capacity ≤ 0 (or a nil limiter) is unlimited.
type PriorityLimiter struct {
	mu         sync.Mutex
	capacity   int
	inUse      int
	waiting    int // active requests waiting for a slot
	preempting int // preempted speculative holders not yet released
	spec       map[uint64]context.CancelCauseFunc
	next       uint64
	free       chan struct{} // closed (and replaced) on every release
}

// NewPriorityLimiter builds a limiter with the given capacity.
func NewPriorityLimiter(capacity int) *PriorityLimiter {
	return &PriorityLimiter{capacity: capacity, spec: map[uint64]context.CancelCauseFunc{}, free: make(chan struct{})}
}

// Acquire takes a slot. It returns the context the work must use (for
// speculative work, one the limiter cancels with ErrPreempted) and an
// idempotent release.
func (l *PriorityLimiter) Acquire(ctx context.Context, p Priority) (context.Context, func(), error) {
	if l == nil || l.capacity <= 0 {
		return ctx, func() {}, nil
	}
	l.mu.Lock()
	if p == Speculative {
		if l.inUse >= l.capacity || l.waiting > 0 {
			l.mu.Unlock()
			return nil, nil, ErrSaturated
		}
		l.inUse++
		id := l.next
		l.next++
		sctx, cancel := context.WithCancelCause(ctx)
		l.spec[id] = cancel
		l.mu.Unlock()
		return sctx, l.release(func() bool {
			_, live := l.spec[id]
			delete(l.spec, id)
			cancel(nil)
			return !live // already preempted
		}), nil
	}
	l.waiting++
	for l.inUse >= l.capacity {
		if l.preempting < l.waiting {
			l.preemptOldest()
		}
		ch := l.free
		l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			l.mu.Lock()
			l.waiting--
			l.mu.Unlock()
			return nil, nil, ctx.Err()
		}
		l.mu.Lock()
	}
	l.waiting--
	l.inUse++
	l.mu.Unlock()
	return ctx, l.release(func() bool { return false }), nil
}

// preemptOldest cancels the oldest live speculative holder (l.mu held).
func (l *PriorityLimiter) preemptOldest() {
	var oldest uint64
	found := false
	for id := range l.spec {
		if !found || id < oldest {
			oldest, found = id, true
		}
	}
	if !found {
		return
	}
	l.spec[oldest](ErrPreempted)
	delete(l.spec, oldest)
	l.preempting++
}

func (l *PriorityLimiter) release(done func() (wasPreempted bool)) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if done() {
				l.preempting--
			}
			l.inUse--
			close(l.free)
			l.free = make(chan struct{})
			l.mu.Unlock()
		})
	}
}

// InUse returns the number of held slots.
func (l *PriorityLimiter) InUse() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inUse
}
