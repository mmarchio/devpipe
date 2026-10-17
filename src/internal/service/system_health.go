package service

import (
	"context"
	"sync"
	"time"

	"overseer/internal/health"
)

// SubsystemCheck reports one subsystem's health (WBS 18.117). It must be
// bounded by ctx.
type SubsystemCheck func(ctx context.Context) health.Subsystem

// SystemHealthService reports per-subsystem health: database, each worker
// and compression provider, repository roots, disk, scheduler, audit. A
// provider outage degrades the service without making it unavailable
// (WBS 18.117); only a startup-critical subsystem does.
type SystemHealthService struct {
	mu     sync.Mutex
	checks []SubsystemCheck
}

// NewSystemHealthService builds the service.
func NewSystemHealthService(checks ...SubsystemCheck) *SystemHealthService {
	return &SystemHealthService{checks: checks}
}

// Add registers further checks (e.g. the disk monitor).
func (s *SystemHealthService) Add(c ...SubsystemCheck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, c...)
}

// Report runs every check concurrently (each bounded to 3s) and folds
// the overall status.
func (s *SystemHealthService) Report(ctx context.Context) health.Report {
	s.mu.Lock()
	checks := append([]SubsystemCheck(nil), s.checks...)
	s.mu.Unlock()
	subs := make([]health.Subsystem, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c SubsystemCheck) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			subs[i] = c(cctx)
		}(i, c)
	}
	wg.Wait()
	return health.NewReport(subs)
}
