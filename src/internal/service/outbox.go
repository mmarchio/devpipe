package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"overseer/internal/persistence"
	"overseer/internal/resilience"
)

// ---------------------------------------------------------------------------
// Workflow outbox (Phase 19.10). A follow-up action is committed in the
// same transaction as the workflow change that requires it, so a crash
// can never keep the change and lose the action. The dispatcher claims due
// actions with a lease (another instance takes over an expired one),
// performs them, and retries failures with backoff until the policy's
// attempts are spent (DEAD). Handlers must be safe to repeat: a crash
// after the action and before DONE performs it again.

// OutboxPolicy bounds an action's attempts and backoff.
var OutboxPolicy = resilience.Policy{Name: "workflow_outbox", MaxAttempts: 8, Base: time.Second, Max: 5 * time.Minute, Jitter: 0.2}

// OutboxHandler performs one action; an error is retried.
type OutboxHandler func(ctx context.Context, o *persistence.OutboxRecord) error

// OutboxDispatcher drains the workflow outbox.
type OutboxDispatcher struct {
	repo     persistence.MCPRepository
	handlers map[string]OutboxHandler
	owner    string
	lease    time.Duration
	policy   resilience.Policy
	now      func() time.Time
	logger   *slog.Logger

	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

// NewOutboxDispatcher builds a dispatcher with the engine's handlers:
// ENGINE_WAKE and CONTEXT_RESET schedule the execution now (the engine
// applies a requested reset before its next command), BLOCKER_NOTIFY
// raises the blocker in the log for the operator.
func NewOutboxDispatcher(repo persistence.MCPRepository, e *Engine, owner string, logger *slog.Logger) *OutboxDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	d := &OutboxDispatcher{repo: repo, owner: owner, lease: time.Minute, policy: OutboxPolicy, now: time.Now, logger: logger}
	wake := func(context.Context, *persistence.OutboxRecord) error {
		if e != nil {
			e.Kick()
		}
		return nil
	}
	d.handlers = map[string]OutboxHandler{
		persistence.OutboxEngineWake:   wake,
		persistence.OutboxContextReset: wake,
		persistence.OutboxBlockerNotify: func(ctx context.Context, o *persistence.OutboxRecord) error {
			logger.WarnContext(ctx, "team reported a blocker", "project_id", o.ProjectID.String(), "reason", o.Payload["reason"],
				"needs", o.Payload["needs"], "dispatch_id", o.Payload["dispatch_id"])
			return nil
		},
	}
	return d
}

// Handle replaces the handler of a kind (tests).
func (d *OutboxDispatcher) Handle(kind string, h OutboxHandler) { d.handlers[kind] = h }

// SetClock replaces the clock (tests).
func (d *OutboxDispatcher) SetClock(now func() time.Time) { d.now = now }

// RunOnce claims and performs the due actions; it returns how many were
// performed (DONE).
func (d *OutboxDispatcher) RunOnce(ctx context.Context) (int, error) {
	now := d.now()
	claimed, err := d.repo.ClaimOutbox(ctx, d.owner, now, d.lease, 50)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range claimed {
		herr := errors.New("no handler for " + o.Kind)
		if h := d.handlers[o.Kind]; h != nil {
			herr = h(ctx, o)
		}
		if herr == nil {
			if err := d.repo.FinishOutbox(ctx, o.ID, d.owner, d.now()); err != nil && !errors.Is(err, persistence.ErrConflict) {
				return n, err
			}
			n++
			continue
		}
		attempt := o.Attempts + 1
		dead := attempt >= d.policy.MaxAttempts
		next := d.now().Add(d.policy.Backoff(attempt, nil))
		if err := d.repo.FailOutbox(ctx, o.ID, d.owner, herr.Error(), next, dead, d.now()); err != nil && !errors.Is(err, persistence.ErrConflict) {
			return n, err
		}
		level := slog.LevelWarn
		if dead {
			level = slog.LevelError
		}
		d.logger.Log(ctx, level, "outbox action failed", "outbox_id", o.ID.String(), "kind", o.Kind, "attempt", attempt, "dead", dead,
			"error", herr.Error())
	}
	return n, nil
}

// Start runs the dispatcher every interval (and once at start: actions
// left by a crashed instance are recovered on startup).
func (d *OutboxDispatcher) Start(interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("outbox: interval must be positive")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stop != nil {
		return nil
	}
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	stop, done := d.stop, d.done
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if _, err := d.RunOnce(context.Background()); err != nil {
				d.logger.Warn("outbox run failed", "error", err.Error())
			}
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// Stop ends the loop and waits for the current run.
func (d *OutboxDispatcher) Stop() {
	d.mu.Lock()
	stop, done := d.stop, d.done
	d.stop, d.done = nil, nil
	d.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}
