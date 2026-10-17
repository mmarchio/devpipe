package precompute

import (
	"net"
	"net/url"
	"sync"
	"time"
)

// Decision is the resource-policy verdict for one unit of work.
type Decision string

const (
	DecisionRun  Decision = "RUN"
	DecisionSkip Decision = "SKIP"
)

// Evaluate applies the resource policy (WBS 11.40). compression reports
// whether the work calls the compression provider; providerLocal whether
// that provider shares local resources; workerActive whether a development
// worker is executing for the project.
//   - ALWAYS: run.
//   - IDLE_ONLY: compression work is skipped while the worker is active
//     (static fragment work still runs).
//   - REMOTE_ONLY_WHILE_WORKER_ACTIVE: while the worker is active, only a
//     remote compression provider may be used.
//   - DISABLED_DURING_LOCAL_WORKER: nothing runs while a local worker is
//     active.
func Evaluate(p ResourcePolicy, compression, providerLocal, workerActive, workerLocal bool) (Decision, string) {
	switch p {
	case PolicyIdleOnly:
		if workerActive && compression {
			return DecisionSkip, "IDLE_ONLY: compression deferred while the worker is active"
		}
	case PolicyRemoteOnlyWhileActive:
		if workerActive && compression && providerLocal {
			return DecisionSkip, "REMOTE_ONLY_WHILE_WORKER_ACTIVE: local compression provider busy with the worker"
		}
	case PolicyDisabledDuringLocal:
		if workerActive && workerLocal {
			return DecisionSkip, "DISABLED_DURING_LOCAL_WORKER: local worker active"
		}
	}
	return DecisionRun, ""
}

// IsLocalURL reports whether a provider base URL points at local or private
// infrastructure (loopback, private ranges, localhost).
func IsLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// Budget tracks the per-job resource budget (WBS 11.39). Zero limits are
// unlimited. Safe for concurrent use.
type Budget struct {
	mu         sync.Mutex
	maxCands   int
	maxTokens  int
	deadline   time.Time
	now        func() time.Time
	candidates int
	tokens     int
}

// NewBudget builds a budget starting now.
func NewBudget(maxCandidates, maxTokens int, maxDuration time.Duration, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	b := &Budget{maxCands: maxCandidates, maxTokens: maxTokens, now: now}
	if maxDuration > 0 {
		b.deadline = now().Add(maxDuration)
	}
	return b
}

// Exhaustion reasons.
const (
	BudgetCandidates = "candidate budget exhausted"
	BudgetTokens     = "token budget exhausted"
	BudgetDuration   = "duration budget exhausted"
)

// Take reserves one candidate with an estimated compression-token cost;
// it returns "" or the exhaustion reason (nothing reserved then).
func (b *Budget) Take(tokens int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.deadline.IsZero() && !b.now().Before(b.deadline) {
		return BudgetDuration
	}
	if b.maxCands > 0 && b.candidates >= b.maxCands {
		return BudgetCandidates
	}
	if b.maxTokens > 0 && tokens > 0 && b.tokens+tokens > b.maxTokens {
		return BudgetTokens
	}
	b.candidates++
	b.tokens += tokens
	return ""
}

// Tokens returns the reserved compression tokens.
func (b *Budget) Tokens() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens
}

// ActivityTracker tracks in-process worker activity per project (fed by
// the dispatch / finish hooks). Acceleration-only state.
type ActivityTracker struct {
	mu     sync.Mutex
	active map[string]bool
}

// SetActive records the worker activity of a project.
func (t *ActivityTracker) SetActive(project string, active bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active == nil {
		t.active = map[string]bool{}
	}
	t.active[project] = active
}

// Active reports whether a worker is active for the project.
func (t *ActivityTracker) Active(project string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active[project]
}
