package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/worker"
)

// providerLabel is the context-source label of provider telemetry.
func providerLabel(p worker.WorkerProvider) string {
	if p == worker.ProviderHermesGateway || p == "" {
		return "Hermes"
	}
	return string(p)
}

func (s *WorkerService) currentSession(ctx context.Context, projectID uuid.UUID) (*persistence.WorkerSessionRecord, error) {
	if rec, err := s.d.Sessions.GetActiveForProject(ctx, projectID); err == nil {
		return rec, nil
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	all, err := s.d.Sessions.ListForProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	if len(all) == 0 {
		return nil, nil
	}
	return &all[len(all)-1], nil
}

// Health is the worker health read model (P12-68, WBS 12.49). It never
// connects or creates anything.
func (s *WorkerService) Health(ctx context.Context, projectID uuid.UUID) (worker.WorkerHealth, error) {
	if _, err := s.d.ProjectSettings.Get(ctx, projectID.String()); err != nil {
		return worker.WorkerHealth{}, translate(err)
	}
	conn, provider := worker.ConnDisconnected, worker.WorkerProvider("")
	var lastOK *time.Time
	var lastErr *worker.WorkerErrorSummary
	if pw := s.existing(projectID); pw != nil {
		conn, provider = pw.state(), pw.provider
		pw.mu.Lock()
		lastOK, lastErr = pw.lastOK, pw.lastErr
		pw.mu.Unlock()
	} else if cfg, err := s.workerConfig(ctx, projectID); err == nil {
		provider = cfg.Provider
	}
	sess, err := s.currentSession(ctx, projectID)
	if err != nil {
		return worker.WorkerHealth{}, err
	}
	var status *worker.SessionStatus
	if sess != nil {
		st := sess.Status
		status = &st
	}
	// After a restart the in-memory history is empty: fall back to the
	// persisted dispatch history.
	if lastOK == nil || lastErr == nil {
		ds, _, err := s.d.Dispatches.List(ctx, persistence.DispatchFilter{ProjectID: projectID, Limit: 200})
		if err == nil {
			for _, d := range ds {
				if lastOK == nil && d.Status == worker.DispatchCompleted && d.CompletedAt != nil {
					t := *d.CompletedAt
					lastOK = &t
				}
				if lastErr == nil && d.ErrorCode != "" && d.CompletedAt != nil {
					sum := worker.WorkerErrorSummary{Code: worker.WorkerErrorCode(d.ErrorCode), Message: d.ErrorMessage, ProviderCode: d.ProviderCode, At: *d.CompletedAt}
					if d.Delivery != nil {
						sum.Delivery = *d.Delivery
					}
					lastErr = &sum
				}
			}
		}
	}
	inflight, _ := s.d.Dispatches.ListInFlight(ctx, &projectID)
	return worker.BuildHealth(provider, conn, status, lastOK, lastErr, len(inflight) > 0), nil
}

// ContextProgress is the context read model of the current session (WBS
// 12.51): the latest observation, labelled by source; unknown when none.
func (s *WorkerService) ContextProgress(ctx context.Context, projectID uuid.UUID) (worker.ContextProgress, error) {
	sess, err := s.currentSession(ctx, projectID)
	if err != nil || sess == nil {
		return worker.BuildContextProgress(worker.WorkerUsage{}, worker.SourceUnknown, ""), err
	}
	o, err := s.d.Responses.LatestObservation(ctx, projectID, sess.ID)
	if errors.Is(err, persistence.ErrNotFound) {
		return worker.BuildContextProgress(worker.WorkerUsage{}, worker.SourceUnknown, ""), nil
	} else if err != nil {
		return worker.ContextProgress{}, translate(err)
	}
	return worker.BuildContextProgress(o.Usage, o.Source, providerLabel(sess.Provider)), nil
}

// View is the worker panel read model (WBS 12.86).
func (s *WorkerService) View(ctx context.Context, projectID uuid.UUID) (worker.WorkerView, error) {
	h, err := s.Health(ctx, projectID)
	if err != nil {
		return worker.WorkerView{}, err
	}
	v := worker.WorkerView{Provider: h.Provider, Connection: h.ConnectionState, SessionStatus: h.SessionState}
	sess, err := s.currentSession(ctx, projectID)
	if err != nil {
		return worker.WorkerView{}, err
	}
	if sess != nil {
		id := sess.ID
		v.SessionID, v.WorkingDirectory = &id, sess.WorkingDirectory
	}
	if v.Context, err = s.ContextProgress(ctx, projectID); err != nil {
		return worker.WorkerView{}, err
	}
	inflight, err := s.d.Dispatches.ListInFlight(ctx, &projectID)
	if err != nil {
		return worker.WorkerView{}, translate(err)
	}
	if len(inflight) > 0 {
		d := inflight[len(inflight)-1]
		start := d.CreatedAt
		if d.StartedAt != nil {
			start = *d.StartedAt
		}
		v.Current = &worker.CurrentRequest{DispatchID: d.ID, WorkUnitID: d.WorkUnitID, RoleKey: d.RoleKey, StateKey: d.StateKey,
			Status: d.Status, StartedAt: start, Duration: s.d.Now().Sub(start)}
	}
	return v, nil
}

// Metrics aggregates the project's worker metrics (WBS 12.54).
func (s *WorkerService) Metrics(ctx context.Context, projectID uuid.UUID) (worker.WorkerMetrics, error) {
	var in worker.MetricsInput
	for off := 0; ; off += 500 {
		ds, total, err := s.d.Dispatches.List(ctx, persistence.DispatchFilter{ProjectID: projectID, Limit: 500, Offset: off})
		if err != nil {
			return worker.WorkerMetrics{}, translate(err)
		}
		in.Dispatches = append(in.Dispatches, ds...)
		if off+500 >= total {
			break
		}
	}
	evs, err := s.d.Events.List(ctx, persistence.WorkerEventFilter{ProjectID: projectID})
	if err != nil {
		return worker.WorkerMetrics{}, translate(err)
	}
	for _, e := range evs {
		in.Events = append(in.Events, e.Type)
	}
	obs, err := s.d.Responses.ListObservations(ctx, projectID, nil)
	if err != nil {
		return worker.WorkerMetrics{}, translate(err)
	}
	for _, o := range obs {
		if o.Usage.InputTokens != nil {
			in.InputTokens += *o.Usage.InputTokens
		}
		if o.Usage.OutputTokens != nil {
			in.OutputTokens += *o.Usage.OutputTokens
		}
		if u, err := o.Usage.Utilization(); err == nil && u.Known {
			in.Utilization = append(in.Utilization, u.Percent)
		}
	}
	return worker.AggregateMetrics(in), nil
}

// Events returns the project's worker audit events.
func (s *WorkerService) Events(ctx context.Context, f persistence.WorkerEventFilter) ([]persistence.WorkerEventRecord, error) {
	out, err := s.d.Events.List(ctx, f)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// UsageObservations returns the project's usage observations in order.
func (s *WorkerService) UsageObservations(ctx context.Context, projectID uuid.UUID, sessionID *uuid.UUID) ([]persistence.WorkerUsageObservation, error) {
	out, err := s.d.Responses.ListObservations(ctx, projectID, sessionID)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// ConnectionState returns the project's transport state without creating
// a worker (DISCONNECTED when none exists).
func (s *WorkerService) ConnectionState(projectID uuid.UUID) worker.ConnectionState {
	if pw := s.existing(projectID); pw != nil {
		return pw.state()
	}
	return worker.ConnDisconnected
}

// Capabilities connects the project's worker (when needed) and returns
// what it advertises (WBS 12.7/12.97: orchestration asks, never assumes).
func (s *WorkerService) Capabilities(ctx context.Context, projectID uuid.UUID) (worker.WorkerCapabilities, error) {
	pw, err := s.worker(ctx, projectID)
	if err != nil {
		return worker.WorkerCapabilities{}, err
	}
	return pw.w.Capabilities(ctx)
}

// ValidateConfiguration is the pre-execution worker configuration check
// (WBS 12.96): every detected problem is returned together.
func (s *WorkerService) ValidateConfiguration(ctx context.Context, projectID uuid.UUID, req worker.WorkflowRequirements) error {
	cfg, err := s.workerConfig(ctx, projectID)
	if err != nil {
		return err
	}
	var caps *worker.WorkerCapabilities
	var providerErr error
	credentials := true
	if pw, err := s.worker(ctx, projectID); pw != nil && err == nil {
		if c, err := pw.w.Capabilities(ctx); err == nil {
			caps = &c
		} else {
			providerErr = err
		}
	} else if err != nil {
		var ce *worker.ConfigError
		if errors.As(err, &ce) {
			for _, p := range ce.Problems {
				if strings.Contains(p, "credentials") {
					credentials = false
				}
			}
		}
		providerErr = err
	}
	return worker.ValidateConfiguration(cfg, caps, req, providerErr, credentials)
}
