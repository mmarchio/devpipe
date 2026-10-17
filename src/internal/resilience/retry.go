package resilience

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// Policy is one named retry rule (WBS 18.189). Every retry loop in the
// overseer uses a registered policy instead of inventing its own.
type Policy struct {
	Name        string
	MaxAttempts int           // total attempts including the first (≥ 1)
	Base        time.Duration // first backoff
	Max         time.Duration // backoff cap (WBS 18.191)
	Jitter      float64       // 0..1 fraction of the delay randomized
	// OverallBudget bounds the whole operation including waits
	// (WBS 18.193); 0 = only the context deadline.
	OverallBudget time.Duration
}

// Validate checks the policy is bounded.
func (p Policy) Validate() error {
	switch {
	case p.Name == "":
		return errors.New("resilience: policy name required")
	case p.MaxAttempts < 1:
		return fmt.Errorf("resilience: %s: max attempts must be ≥ 1", p.Name)
	case p.Base < 0 || p.Max < p.Base:
		return fmt.Errorf("resilience: %s: backoff must satisfy 0 ≤ base ≤ max", p.Name)
	case p.Jitter < 0 || p.Jitter > 1:
		return fmt.Errorf("resilience: %s: jitter must be within [0,1]", p.Name)
	case p.OverallBudget < 0:
		return fmt.Errorf("resilience: %s: overall budget must not be negative", p.Name)
	}
	return nil
}

// Backoff returns the wait before attempt (1-based retry number): an
// exponential delay Base·2^(attempt-1), capped at Max, with ±Jitter/2
// randomization, never above Max (WBS 18.191).
func (p Policy) Backoff(attempt int, rng *rand.Rand) time.Duration {
	if attempt < 1 || p.Base <= 0 {
		return 0
	}
	d := p.Base
	for i := 1; i < attempt && d < p.Max; i++ {
		d *= 2
	}
	if d > p.Max {
		d = p.Max
	}
	if p.Jitter > 0 && rng != nil {
		span := float64(d) * p.Jitter
		d = time.Duration(float64(d) - span/2 + rng.Float64()*span)
	}
	if d > p.Max {
		d = p.Max
	}
	if d < 0 {
		d = 0
	}
	return d
}

// Registered policy names.
const (
	PolicyDBTx            = "db_tx"
	PolicyWorkerConnect   = "worker_connect"
	PolicyCompressionCall = "compression_call"
	PolicyGitLock         = "git_lock"
	PolicyValidationInfra = "validation_infra"
	PolicyProviderProbe   = "provider_probe"
)

// DefaultPolicies are the built-in policies.
func DefaultPolicies() []Policy {
	return []Policy{
		{Name: PolicyDBTx, MaxAttempts: 4, Base: 10 * time.Millisecond, Max: 200 * time.Millisecond, Jitter: 0.5, OverallBudget: 2 * time.Second},
		{Name: PolicyWorkerConnect, MaxAttempts: 3, Base: 200 * time.Millisecond, Max: 2 * time.Second, Jitter: 0.5, OverallBudget: 10 * time.Second},
		{Name: PolicyCompressionCall, MaxAttempts: 2, Base: 200 * time.Millisecond, Max: 1 * time.Second, Jitter: 0.5, OverallBudget: 30 * time.Second},
		{Name: PolicyGitLock, MaxAttempts: 5, Base: 50 * time.Millisecond, Max: 1 * time.Second, Jitter: 0.5, OverallBudget: 5 * time.Second},
		{Name: PolicyValidationInfra, MaxAttempts: 2, Base: 500 * time.Millisecond, Max: 2 * time.Second, Jitter: 0.2, OverallBudget: 0},
		{Name: PolicyProviderProbe, MaxAttempts: 1, Base: 0, Max: 0, OverallBudget: 5 * time.Second},
	}
}

// Registry holds the named policies and the shared retry budget.
type Registry struct {
	mu       sync.Mutex
	policies map[string]Policy
	budget   *Budget
	rng      *rand.Rand
	sleep    func(ctx context.Context, d time.Duration) error
	now      func() time.Time
}

// NewRegistry builds a registry from policies (invalid ones are an
// error) with an optional shared budget (nil = unlimited).
func NewRegistry(policies []Policy, budget *Budget) (*Registry, error) {
	r := &Registry{policies: map[string]Policy{}, budget: budget, rng: rand.New(rand.NewSource(time.Now().UnixNano())),
		sleep: sleepCtx, now: time.Now}
	for _, p := range policies {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		r.policies[p.Name] = p
	}
	return r, nil
}

// MustDefault returns a registry of DefaultPolicies with a generous shared
// budget.
func MustDefault() *Registry {
	r, err := NewRegistry(DefaultPolicies(), NewBudget(50, 10))
	if err != nil {
		panic(err)
	}
	return r
}

// Policy returns a registered policy.
func (r *Registry) Policy(name string) (Policy, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.policies[name]
	return p, ok
}

// SetClock replaces sleep / now (tests).
func (r *Registry) SetClock(sleep func(ctx context.Context, d time.Duration) error, now func() time.Time) {
	r.sleep, r.now = sleep, now
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrBudgetExhausted is returned (wrapping the last error) when the
// shared retry budget or the overall time budget stops a retry.
var ErrBudgetExhausted = errors.New("resilience: retry budget exhausted")

// Result reports what Do did.
type Result struct {
	Attempts int
	Waited   time.Duration
}

// Do runs op under the named policy: only TRANSIENT failures are
// retried, at most MaxAttempts in total, within OverallBudget and the
// shared budget. AMBIGUOUS / PERMANENT / RECOVERABLE / OPERATOR_REQUIRED /
// FATAL_PROCESS errors return immediately (WBS 18.190). The last error is
// returned unchanged unless a budget stopped the loop.
func (r *Registry) Do(ctx context.Context, name string, op func(ctx context.Context, attempt int) error) (Result, error) {
	p, ok := r.Policy(name)
	if !ok {
		return Result{}, fmt.Errorf("resilience: unknown policy %q", name)
	}
	start := r.now()
	var res Result
	for attempt := 1; ; attempt++ {
		res.Attempts = attempt
		err := op(ctx, attempt)
		if err == nil {
			return res, nil
		}
		if !Retryable(ClassOf(err)) || attempt >= p.MaxAttempts {
			return res, err
		}
		wait := p.Backoff(attempt, r.lockedRng())
		if p.OverallBudget > 0 && r.now().Sub(start)+wait >= p.OverallBudget {
			return res, fmt.Errorf("%w (%s overall budget %s): %w", ErrBudgetExhausted, p.Name, p.OverallBudget, err)
		}
		if r.budget != nil && !r.budget.Take() {
			return res, fmt.Errorf("%w (%s shared budget): %w", ErrBudgetExhausted, p.Name, err)
		}
		if serr := r.sleep(ctx, wait); serr != nil {
			return res, err
		}
		res.Waited += wait
	}
}

func (r *Registry) lockedRng() *rand.Rand {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A private copy seeded from the shared source keeps Backoff
	// goroutine-safe.
	return rand.New(rand.NewSource(r.rng.Int63()))
}

// Budget is a token bucket shared by all retries (WBS 18.192): every
// retry takes a token; tokens refill at RefillPerSecond up to Capacity.
type Budget struct {
	mu       sync.Mutex
	capacity float64
	refill   float64
	tokens   float64
	last     time.Time
	now      func() time.Time
}

// NewBudget builds a full budget.
func NewBudget(capacity, refillPerSecond float64) *Budget {
	return &Budget{capacity: capacity, refill: refillPerSecond, tokens: capacity, now: time.Now, last: time.Now()}
}

// SetNow replaces the clock (tests).
func (b *Budget) SetNow(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now, b.last = now, now()
}

// Take consumes one retry token; false when exhausted.
func (b *Budget) Take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.tokens += now.Sub(b.last).Seconds() * b.refill
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
