package service

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"overseer/internal/dbrepo"
	"overseer/internal/health"
)

// AlertFactsSource reads the durable alert facts.
type AlertFactsSource interface {
	Facts(ctx context.Context, stall, infraWindow time.Duration) (*dbrepo.AlertFacts, error)
}

// AlertContributor adds facts owned by another component (disk monitor,
// periodic integrity scan).
type AlertContributor func(ctx context.Context, f *health.AlertFacts)

// AlertConfig tunes the evaluator.
type AlertConfig struct {
	StallThreshold time.Duration // default 10m
	InfraWindow    time.Duration // default 1h
	InfraThreshold int           // default 3
	OnActive       func(id health.AlertID, active bool)
}

// AlertService evaluates the alert conditions (WBS 18.118–18.120)
// periodically and on demand. Alerts surface through the API, the console
// and the log (decision U10): every activation is logged at WARN, every
// clearing at INFO, and each alert has an active gauge.
type AlertService struct {
	facts  AlertFactsSource
	health *SystemHealthService
	audit  *AuditVerificationService
	cfg    AlertConfig
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	contrib []AlertContributor
	tracker *health.Tracker
	last    []health.Alert
	stop    chan struct{}
	done    chan struct{}
}

// NewAlertService builds the evaluator.
func NewAlertService(facts AlertFactsSource, h *SystemHealthService, audit *AuditVerificationService, cfg AlertConfig, log *slog.Logger) *AlertService {
	if cfg.StallThreshold <= 0 {
		cfg.StallThreshold = 10 * time.Minute
	}
	if cfg.InfraWindow <= 0 {
		cfg.InfraWindow = time.Hour
	}
	if cfg.InfraThreshold <= 0 {
		cfg.InfraThreshold = 3
	}
	if log == nil {
		log = slog.Default()
	}
	return &AlertService{facts: facts, health: h, audit: audit, cfg: cfg, log: log, now: time.Now, tracker: health.NewTracker()}
}

// Contribute registers a facts contributor.
func (s *AlertService) Contribute(c AlertContributor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contrib = append(s.contrib, c)
}

// Evaluate gathers facts, evaluates every alert, records transitions
// (logged) and returns the current alerts.
func (s *AlertService) Evaluate(ctx context.Context) []health.Alert {
	f := health.AlertFacts{DatabaseUp: true, ValidationInfraThreshold: s.cfg.InfraThreshold}
	if s.health != nil {
		for _, sub := range s.health.Report(ctx).Subsystems {
			switch {
			case sub.Name == "database" && sub.Status == health.Unavailable:
				f.DatabaseUp = false
			case strings.HasPrefix(sub.Name, "worker:") && sub.Status == health.Unavailable:
				f.WorkersUnavailable = append(f.WorkersUnavailable, strings.TrimPrefix(sub.Name, "worker:"))
			}
		}
	}
	if f.DatabaseUp && s.facts != nil {
		if d, err := s.facts.Facts(ctx, s.cfg.StallThreshold, s.cfg.InfraWindow); err == nil {
			f.RecoveryRequired, f.Stalled, f.Deadlocked, f.ValidationInfraFailures = d.RecoveryRequired, d.Stalled, d.Deadlocked, d.ValidationInfraFailures
		} else {
			s.log.WarnContext(ctx, "alert facts unavailable", "error", err.Error())
		}
	}
	if s.audit != nil {
		if v := s.audit.Last(); v != nil && v.BlocksNewWork {
			f.AuditStatus = string(v.Status)
		}
	}
	s.mu.Lock()
	contrib := append([]AlertContributor(nil), s.contrib...)
	s.mu.Unlock()
	for _, c := range contrib {
		c(ctx, &f)
	}
	alerts := health.Evaluate(f)
	s.mu.Lock()
	cur, trans := s.tracker.Update(s.now().UTC(), alerts)
	s.last = cur
	s.mu.Unlock()
	for _, t := range trans {
		detail := ""
		for _, a := range cur {
			if a.ID == t.ID {
				detail = a.Detail
			}
		}
		if t.Activated {
			s.log.WarnContext(ctx, "alert activated", "alert", string(t.ID), "detail", detail)
		} else {
			s.log.InfoContext(ctx, "alert cleared", "alert", string(t.ID))
		}
	}
	if s.cfg.OnActive != nil {
		for _, a := range cur {
			s.cfg.OnActive(a.ID, a.Active)
		}
	}
	return cur
}

// Last returns the latest evaluation (nil before the first).
func (s *AlertService) Last() []health.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]health.Alert(nil), s.last...)
}

// Active returns the active alerts of the latest evaluation.
func (s *AlertService) Active() []health.Alert {
	var out []health.Alert
	for _, a := range s.Last() {
		if a.Active {
			out = append(out, a)
		}
	}
	return out
}

// Start evaluates every interval until Stop.
func (s *AlertService) Start(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s.mu.Lock()
	if s.stop != nil {
		s.mu.Unlock()
		return
	}
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	stop, done := s.stop, s.done
	s.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			func() {
				defer containPanic(s.log, "alert evaluation")
				s.Evaluate(ctx)
			}()
			cancel()
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
}

// Stop ends the periodic evaluation and waits for it.
func (s *AlertService) Stop() {
	s.mu.Lock()
	stop, done := s.stop, s.done
	s.stop, s.done = nil, nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}
