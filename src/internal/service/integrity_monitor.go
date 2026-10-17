package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"overseer/internal/health"
)

// IntegrityMonitorConfig tunes the periodic integrity scan.
type IntegrityMonitorConfig struct {
	Interval time.Duration // default 15m
	Batch    int           // executions per tick, default 50
}

// IntegrityMonitor is the periodic background integrity scan (WBS 18.180):
// each tick checks a bounded round-robin batch of executions, one at a
// time (a single connection at a time; it never holds a lease longer than
// one check), so it cannot starve execution. A violation raises
// INTEGRITY_VIOLATION and puts the affected execution — only that one —
// into INVARIANT_VIOLATION recovery.
type IntegrityMonitor struct {
	list   func(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error)
	check  func(ctx context.Context, executionID uuid.UUID) (int, error)
	cfg    IntegrityMonitorConfig
	log    *slog.Logger
	mu     sync.Mutex
	cursor uuid.UUID
	found  map[uuid.UUID]int
	stop   chan struct{}
	done   chan struct{}
}

// NewIntegrityMonitor builds the monitor.
func NewIntegrityMonitor(list func(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error),
	check func(ctx context.Context, executionID uuid.UUID) (int, error), cfg IntegrityMonitorConfig, log *slog.Logger) *IntegrityMonitor {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Minute
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	if log == nil {
		log = slog.Default()
	}
	return &IntegrityMonitor{list: list, check: check, cfg: cfg, log: log, found: map[uuid.UUID]int{}}
}

// Tick scans the next batch and returns how many executions it checked.
func (m *IntegrityMonitor) Tick(ctx context.Context) (int, error) {
	m.mu.Lock()
	cursor := m.cursor
	m.mu.Unlock()
	ids, err := m.list(ctx, cursor, m.cfg.Batch)
	if err != nil {
		return 0, err
	}
	if len(ids) < m.cfg.Batch {
		// The end of the round: the next tick starts over.
		m.mu.Lock()
		m.cursor = uuid.Nil
		m.mu.Unlock()
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		n, err := m.check(ctx, id)
		if err != nil && !errors.Is(err, ErrExecutionBusy) {
			m.log.WarnContext(ctx, "integrity check failed", "execution_id", id.String(), "error", err.Error())
		}
		m.mu.Lock()
		if n > 0 {
			if m.found[id] == 0 {
				m.log.ErrorContext(ctx, "integrity violation detected", "execution_id", id.String(), "violations", n)
			}
			m.found[id] = n
		} else if err == nil {
			delete(m.found, id)
		}
		if len(ids) == m.cfg.Batch {
			m.cursor = id
		}
		m.mu.Unlock()
	}
	return len(ids), nil
}

// Violations is the number of critical violations found in the latest
// check of every execution.
func (m *IntegrityMonitor) Violations() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.found {
		n += v
	}
	return n
}

// Contribute feeds the alert evaluator.
func (m *IntegrityMonitor) Contribute(_ context.Context, f *health.AlertFacts) {
	f.IntegrityViolations += m.Violations()
}

// Start ticks every interval until Stop.
func (m *IntegrityMonitor) Start() {
	m.mu.Lock()
	if m.stop != nil {
		m.mu.Unlock()
		return
	}
	m.stop, m.done = make(chan struct{}), make(chan struct{})
	stop, done := m.stop, m.done
	m.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(m.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Interval)
				func() {
					defer containPanic(m.log, "integrity scan")
					_, _ = m.Tick(ctx)
				}()
				cancel()
			}
		}
	}()
}

// Stop ends the periodic scan and waits for it.
func (m *IntegrityMonitor) Stop() {
	m.mu.Lock()
	stop, done := m.stop, m.done
	m.stop, m.done = nil, nil
	m.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}
