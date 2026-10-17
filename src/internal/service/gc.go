package service

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// GCStore is the bounded GC statements (dbrepo.GCRepository).
type GCStore interface {
	ExpireCacheEntries(ctx context.Context, cutoff time.Time, batch int) (int, error)
	ExpirePrecomputedTemplates(ctx context.Context, cutoff time.Time, batch int) (int, error)
	ExpireProjections(ctx context.Context, cutoff time.Time, batch int) (int, error)
	ExpirePrecomputeJobs(ctx context.Context, cutoff time.Time, batch int) (int, error)
	ExpireValidationLogs(ctx context.Context, cutoff time.Time, batch int) (int, error)
}

// GCConfig is the retention policy (decision U2) and the bounds of one
// run.
type GCConfig struct {
	CacheRetention         time.Duration // default 7d
	PrecomputeRetention    time.Duration // default 24h
	ValidationLogRetention time.Duration // default 30d
	Batch                  int           // rows per statement, default 500
	MaxBatches             int           // statements per kind per run, default 20
}

// GCResult reports one run.
type GCResult struct {
	CacheEntries         int `json:"cache_entries"`
	PrecomputedTemplates int `json:"precomputed_templates"`
	Projections          int `json:"projections"`
	PrecomputeJobs       int `json:"precompute_jobs"`
	ValidationLogs       int `json:"validation_logs"`
	// Complete: every kind finished within the bounds (otherwise the
	// next run continues).
	Complete   bool      `json:"complete"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error,omitempty"`
}

// GCService collects expired artifacts (WBS 18.99–18.104, decisions
// U1 / U2): unreferenced prompt cache entries, expired precompute
// artifacts and raw validation logs. Runs are bounded, idempotent and
// restart-safe; one at a time per process; audit is never collected.
type GCService struct {
	store GCStore
	cfg   GCConfig
	log   *slog.Logger
	now   func() time.Time

	run  sync.Mutex
	mu   sync.Mutex
	last *GCResult
}

// NewGCService builds the service.
func NewGCService(store GCStore, cfg GCConfig, log *slog.Logger) *GCService {
	if cfg.CacheRetention <= 0 {
		cfg.CacheRetention = 7 * 24 * time.Hour
	}
	if cfg.PrecomputeRetention <= 0 {
		cfg.PrecomputeRetention = 24 * time.Hour
	}
	if cfg.ValidationLogRetention <= 0 {
		cfg.ValidationLogRetention = 30 * 24 * time.Hour
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 500
	}
	if cfg.MaxBatches <= 0 {
		cfg.MaxBatches = 20
	}
	if log == nil {
		log = slog.Default()
	}
	return &GCService{store: store, cfg: cfg, log: log, now: time.Now}
}

// Run performs one bounded collection.
func (s *GCService) Run(ctx context.Context) (*GCResult, error) {
	s.run.Lock()
	defer s.run.Unlock()
	now := s.now()
	res := &GCResult{StartedAt: now.UTC(), Complete: true}
	kinds := []struct {
		name   string
		cutoff time.Time
		fn     func(context.Context, time.Time, int) (int, error)
		count  *int
	}{
		{"cache entries", now.Add(-s.cfg.CacheRetention), s.store.ExpireCacheEntries, &res.CacheEntries},
		{"precomputed templates", now.Add(-s.cfg.PrecomputeRetention), s.store.ExpirePrecomputedTemplates, &res.PrecomputedTemplates},
		{"precompute projections", now.Add(-s.cfg.PrecomputeRetention), s.store.ExpireProjections, &res.Projections},
		{"precompute jobs", now.Add(-s.cfg.PrecomputeRetention), s.store.ExpirePrecomputeJobs, &res.PrecomputeJobs},
		{"validation logs", now.Add(-s.cfg.ValidationLogRetention), s.store.ExpireValidationLogs, &res.ValidationLogs},
	}
	var firstErr error
	for _, k := range kinds {
		done := false
		for i := 0; i < s.cfg.MaxBatches; i++ {
			if ctx.Err() != nil {
				firstErr = ctx.Err()
				break
			}
			n, err := k.fn(ctx, k.cutoff, s.cfg.Batch)
			*k.count += n
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				s.log.ErrorContext(ctx, "garbage collection failed", "kind", k.name, "error", err.Error())
				break
			}
			if n < s.cfg.Batch {
				done = true
				break
			}
		}
		res.Complete = res.Complete && done
	}
	res.FinishedAt = s.now().UTC()
	if firstErr != nil {
		res.Error = firstErr.Error()
	}
	s.mu.Lock()
	s.last = res
	s.mu.Unlock()
	s.log.InfoContext(ctx, "garbage collection finished", "cache_entries", res.CacheEntries, "precomputed_templates", res.PrecomputedTemplates,
		"projections", res.Projections, "precompute_jobs", res.PrecomputeJobs, "validation_logs", res.ValidationLogs, "complete", res.Complete)
	if firstErr != nil {
		return res, translate(firstErr)
	}
	return res, nil
}

// Last returns the latest run (nil before the first).
func (s *GCService) Last() *GCResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return nil
	}
	cp := *s.last
	return &cp
}

// Failed reports whether the latest run failed (alerting).
func (s *GCService) Failed() bool {
	l := s.Last()
	return l != nil && l.Error != ""
}
