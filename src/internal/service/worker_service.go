package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/persistence"
	"overseer/internal/precompute"
	"overseer/internal/worker"
)

// Phase 12 worker service errors.
var (
	// ErrWorkerNotConfigured: the project has no (resolvable) worker
	// provider (WBS 12.96).
	ErrWorkerNotConfigured = errors.New("service: worker provider not configured")
	// ErrNoActiveSession: the project has no ACTIVE worker session. Phase
	// 12 never creates one implicitly.
	ErrNoActiveSession = errors.New("service: no active worker session")
	// ErrSessionBusy: the session already has a dispatch in flight (WBS
	// 12.56/12.57).
	ErrSessionBusy = errors.New("service: worker session busy")
	// ErrDuplicateDispatch: the same logical dispatch attempt already
	// exists (WBS 12.33).
	ErrDuplicateDispatch = errors.New("service: duplicate worker dispatch")
	// ErrPromptIntegrity: the worker prompt no longer matches its hash
	// (WBS 12.29).
	ErrPromptIntegrity = errors.New("service: worker prompt integrity check failed")
	// ErrWorkingDirectoryNotReady: the session's confirmed directory is
	// unknown or differs from the project's (WBS 12.26/12.62); orchestration
	// must issue ChangeDirectory first.
	ErrWorkingDirectoryNotReady = errors.New("service: worker working directory not confirmed")
)

// WorkerProviderResolver maps a project's worker_provider setting (a
// configured provider name) to the generic provider kind, its opaque
// provider configuration and the session timeout.
type WorkerProviderResolver func(name string) (provider worker.WorkerProvider, providerConfig json.RawMessage, sessionTimeout time.Duration, ok bool)

// WorkerActivity receives worker dispatch notifications (Phase 11 WBS
// 11.4/11.58 triggers).
type WorkerActivity interface {
	NotifyWorkerDispatched(ctx context.Context, projectID uuid.UUID) *precompute.Job
	NotifyWorkerFinished(projectID uuid.UUID)
}

// WorkerPromptReader loads immutable worker prompts.
type WorkerPromptReader interface {
	GetWorkerPrompt(ctx context.Context, id uuid.UUID) (*compression.WorkerPrompt, error)
}

// WorkerDeps wires the worker service.
type WorkerDeps struct {
	Factory   worker.WorkerFactory
	Providers WorkerProviderResolver

	ProjectSettings persistence.SettingsRepository
	Settings        persistence.WorkerSettingsRepository
	Sessions        persistence.WorkerSessionRepository
	Dispatches      persistence.WorkerDispatchRepository
	Responses       persistence.WorkerResponseRepository
	Leases          persistence.WorkerLeaseRepository
	Events          persistence.WorkerEventRepository
	Prompts         WorkerPromptReader

	Activity WorkerActivity
	Logger   *slog.Logger
	// Owner identifies this overseer instance in session leases.
	Owner string
	Now   func() time.Time
	// LocalDirCheck, when set, verifies a directory on this host before
	// it is sent to the worker (only meaningful for a local worker).
	LocalDirCheck func(string) bool
}

// projectWorker is the in-memory worker of one project. It is
// acceleration only; PostgreSQL holds the authoritative session and
// dispatch history.
type projectWorker struct {
	w        worker.Worker
	provider worker.WorkerProvider
	cfg      worker.WorkerConfig

	mu      sync.Mutex
	lastOK  *time.Time
	lastErr *worker.WorkerErrorSummary
}

func (p *projectWorker) recordOK(at time.Time) {
	p.mu.Lock()
	p.lastOK = &at
	p.mu.Unlock()
}

func (p *projectWorker) recordErr(we *worker.WorkerError, at time.Time) {
	s := we.Summary(at)
	p.mu.Lock()
	p.lastErr = &s
	p.mu.Unlock()
}

func (p *projectWorker) state() worker.ConnectionState {
	if c, ok := p.w.(worker.Connector); ok {
		return c.ConnectionState()
	}
	return worker.ConnConnected
}

// WorkerService is the Phase 12 application service: a per-project worker
// registry (WBS 12.2/12.55), session control (12.19-12.27), dispatch
// (12.28-12.37), restart recovery (12.42-12.44) and read models (12.49-
// 12.54). It transports prompts and records what happened; it never
// interprets responses, transitions workflow state, resets sessions on its
// own or resends ambiguous work.
type WorkerService struct {
	d WorkerDeps

	mu      sync.Mutex
	workers map[uuid.UUID]*projectWorker
	closing bool
	// shuttingDown: a worker call ending in an error now was cut short by
	// this process's own shutdown — its row stays reconcilable (WBS
	// 18.126).
	shuttingDown atomic.Bool
	// inflight tracks dispatches between admission and final persistence
	// (shutdown waits for them, WBS 12.67.2).
	inflight sync.WaitGroup
	// active holds the dispatches this process is still executing (Phase
	// 18): a non-terminal row absent here belongs to no live call in this
	// instance.
	active map[uuid.UUID]struct{}

	completion DispatchCompletionObserver
	failure    DispatchFailureObserver

	// availability is the Phase 18 provider circuit + concurrency state.
	availability *availability
}

// DispatchFailureObserver is told when a dispatch ends without a response
// (FAILED / CANCELLED / UNKNOWN) and its outcome is persisted (Phase 14
// context-limit / lost-session hook, P14-56).
type DispatchFailureObserver interface {
	OnDispatchFailed(ctx context.Context, d worker.WorkerDispatch)
}

// WithFailureObserver registers the failure observer.
func (s *WorkerService) WithFailureObserver(o DispatchFailureObserver) *WorkerService {
	s.failure = o
	return s
}

// DispatchCompletionObserver is told when a dispatch is COMPLETED and its
// raw response persisted (Phase 13 parsing hook, P13-36).
type DispatchCompletionObserver interface {
	OnDispatchCompleted(ctx context.Context, projectID, dispatchID uuid.UUID)
}

// WithCompletionObserver registers the completion observer.
func (s *WorkerService) WithCompletionObserver(o DispatchCompletionObserver) *WorkerService {
	s.completion = o
	return s
}

// NewWorkerService builds the service.
func NewWorkerService(d WorkerDeps) *WorkerService {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Owner == "" {
		d.Owner = "overseer-" + uuid.NewString()[:8]
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &WorkerService{d: d, workers: map[uuid.UUID]*projectWorker{}}
}

// Owner returns this instance's lease owner identity.
func (s *WorkerService) Owner() string { return s.d.Owner }

// --- registry (P12-57) ------------------------------------------------------

// workerConfig builds the generic configuration of a project.
func (s *WorkerService) workerConfig(ctx context.Context, projectID uuid.UUID) (worker.WorkerConfig, error) {
	ps, err := s.d.ProjectSettings.Get(ctx, projectID.String())
	if err != nil {
		return worker.WorkerConfig{}, translate(err)
	}
	if ps.WorkerProvider == "" || s.d.Providers == nil {
		return worker.WorkerConfig{}, ErrWorkerNotConfigured
	}
	prov, pc, sessTO, ok := s.d.Providers(ps.WorkerProvider)
	if !ok {
		return worker.WorkerConfig{}, fmt.Errorf("%w: %q", ErrWorkerNotConfigured, ps.WorkerProvider)
	}
	return worker.WorkerConfig{Provider: prov, Model: ps.WorkerModel, RequestTimeout: ps.WorkerTimeout, SessionTimeout: sessTO,
		ContextWindow: ps.ContextWindow, ProviderConfig: pc}, nil
}

// worker returns the project's worker, creating and connecting it on first
// use. A worker whose connection failed is kept (its state reports FAILED)
// so the failure is visible and Reconnect can restore it.
func (s *WorkerService) worker(ctx context.Context, projectID uuid.UUID) (*projectWorker, error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "worker service shutting down", nil)
	}
	pw := s.workers[projectID]
	s.mu.Unlock()
	if pw == nil {
		cfg, err := s.workerConfig(ctx, projectID)
		if err != nil {
			return nil, err
		}
		w, err := s.d.Factory.Create(ctx, cfg)
		if err != nil {
			return nil, errors.Join(ErrWorkerNotConfigured, err)
		}
		pw = &projectWorker{w: w, provider: cfg.Provider, cfg: cfg}
		if o, ok := w.(worker.Observable); ok {
			o.OnConnectionChange(s.connectionObserver(projectID))
		}
		s.mu.Lock()
		if existing := s.workers[projectID]; existing != nil {
			s.mu.Unlock()
			_ = w.Close(ctx)
			pw = existing
		} else if s.closing {
			s.mu.Unlock()
			_ = w.Close(ctx)
			return nil, worker.NewError(worker.CodeClosed, worker.DeliveryNotSent, "worker service shutting down", nil)
		} else {
			s.workers[projectID] = pw
			s.mu.Unlock()
		}
	}
	if c, ok := pw.w.(worker.Connector); ok {
		switch c.ConnectionState() {
		case worker.ConnDisconnected, worker.ConnFailed:
			if err := c.Connect(ctx); err != nil {
				we := worker.AsWorkerError(err)
				pw.recordErr(we, s.d.Now())
				return pw, we
			}
		}
	}
	return pw, nil
}

// existing returns the project's worker without creating one.
func (s *WorkerService) existing(projectID uuid.UUID) *projectWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers[projectID]
}

// connectionObserver records transport changes as audit events (WBS
// 12.53).
func (s *WorkerService) connectionObserver(projectID uuid.UUID) worker.ConnectionObserver {
	return func(from, to worker.ConnectionState, cause error) {
		var typ worker.EventType
		switch {
		case to == worker.ConnConnected && from == worker.ConnReconnecting:
			typ = worker.EventReconnected
		case to == worker.ConnConnected:
			typ = worker.EventConnected
		case to == worker.ConnDisconnected, to == worker.ConnClosed:
			typ = worker.EventDisconnected
		case to == worker.ConnFailed, cause != nil && to == worker.ConnReconnecting:
			typ = worker.EventConnectionFailed
		default:
			return
		}
		p := map[string]any{"from": string(from), "to": string(to)}
		if cause != nil {
			we := worker.AsWorkerError(cause)
			p["code"], p["delivery"] = string(we.Code), string(we.Delivery)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.event(ctx, projectID, nil, nil, typ, p)
	}
}

// sanitizedKeys are never allowed in an audit payload (WBS 12.53.4).
var sanitizedKeys = []string{"content", "prompt", "response", "token", "secret", "authorization", "password", "api_key"}

func sanitizePayload(p map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range p {
		lk := strings.ToLower(k)
		if strings.HasSuffix(lk, "_hash") || strings.HasSuffix(lk, "_bytes") {
			out[k] = v // digests and sizes identify content without carrying it
			continue
		}
		drop := false
		for _, bad := range sanitizedKeys {
			if strings.Contains(lk, bad) {
				drop = true
				break
			}
		}
		if !drop {
			out[k] = v
		}
	}
	return out
}

func (s *WorkerService) event(ctx context.Context, projectID uuid.UUID, sessionID, dispatchID *uuid.UUID, typ worker.EventType, payload map[string]any) {
	if s.d.Events == nil {
		return
	}
	e := &persistence.WorkerEventRecord{ID: uuid.New(), ProjectID: projectID, SessionID: sessionID, DispatchID: dispatchID, Type: typ,
		Payload: sanitizePayload(payload), CreatedAt: s.d.Now()}
	if err := s.d.Events.Append(context.WithoutCancel(ctx), e); err != nil {
		s.d.Logger.Warn("worker event not recorded", "type", string(typ), "project_id", projectID.String(), "error", err.Error())
	}
}

// Reconnect restores a project's worker transport and reconciles its
// active session (WBS 12.40-12.42, 12.100). It never resends a request
// and never creates a session.
func (s *WorkerService) Reconnect(ctx context.Context, projectID uuid.UUID) (worker.Reconciliation, error) {
	pw := s.existing(projectID)
	if pw == nil {
		var err error
		if pw, err = s.worker(ctx, projectID); err != nil && pw == nil {
			return worker.ReconcileUnknown, err
		}
	} else if r, ok := pw.w.(worker.Reconciler); ok {
		if err := r.Reconnect(ctx); err != nil {
			pw.recordErr(worker.AsWorkerError(err), s.d.Now())
			return worker.ReconcileUnknown, err
		}
	}
	rec, err := s.d.Sessions.GetActiveForProject(ctx, projectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return worker.ReconcileUnknown, ErrNoActiveSession
	} else if err != nil {
		return worker.ReconcileUnknown, translate(err)
	}
	return s.reconcile(ctx, pw, rec)
}

// reconcile checks a persisted session with the worker and records the
// outcome (LOST → session LOST + event; CONFIRMED → last seen + any
// provider-reported directory).
func (s *WorkerService) reconcile(ctx context.Context, pw *projectWorker, rec *persistence.WorkerSessionRecord) (worker.Reconciliation, error) {
	r, ok := pw.w.(worker.Reconciler)
	if !ok {
		return worker.ReconcileUnknown, nil
	}
	res, err := r.ReconcileSession(ctx, worker.SessionInfo{ID: rec.ProviderSessionID, Provider: rec.Provider, CreatedAt: rec.CreatedAt, WorkingDirectory: rec.WorkingDirectory})
	if err != nil {
		return worker.ReconcileUnknown, err
	}
	now := s.d.Now()
	switch res.Result {
	case worker.ReconcileConfirmed:
		_ = s.d.Sessions.UpdateLastSeen(ctx, rec.ProjectID, rec.ID, now)
		if d := res.Session.WorkingDirectory; d != nil && (rec.WorkingDirectory == nil || *rec.WorkingDirectory != *d) {
			_ = s.d.Sessions.UpdateWorkingDirectory(ctx, rec.ProjectID, rec.ID, d, now)
		}
	case worker.ReconcileLost:
		s.markLost(ctx, rec, "provider reports the session no longer exists")
	}
	return res.Result, nil
}

func (s *WorkerService) markLost(ctx context.Context, rec *persistence.WorkerSessionRecord, reason string) {
	if ok, err := s.d.Sessions.MarkLost(context.WithoutCancel(ctx), rec.ProjectID, rec.ID, reason, s.d.Now()); err == nil && ok {
		id := rec.ID
		s.event(ctx, rec.ProjectID, &id, nil, worker.EventSessionLost, map[string]any{"provider_session_id": rec.ProviderSessionID, "reason": reason})
	}
}

// ensureSession makes the worker's current session the persisted ACTIVE
// one (after a restart or a failed persistence, the adapter may hold no or
// another session). It never creates a session.
func (s *WorkerService) ensureSession(ctx context.Context, pw *projectWorker, rec *persistence.WorkerSessionRecord) error {
	if cur, err := pw.w.Session(ctx); err == nil && cur.ID == rec.ProviderSessionID {
		return nil
	}
	res, err := s.reconcile(ctx, pw, rec)
	if err != nil {
		return err
	}
	switch res {
	case worker.ReconcileConfirmed:
		return nil
	case worker.ReconcileLost:
		return worker.NewError(worker.CodeSessionLost, worker.DeliveryNotSent, "worker session no longer exists", nil)
	}
	return worker.NewError(worker.CodeUnavailable, worker.DeliveryNotSent, "worker session could not be confirmed", nil)
}

// Close shuts every project worker down (WBS 12.66/12.67): in-flight
// requests are cancelled by the adapters and their final status is
// persisted by the dispatch path before this returns (bounded by ctx).
// BeginShutdown marks the start of process shutdown (before the engine
// drain): calls cut short from now on are left for reconciliation.
func (s *WorkerService) BeginShutdown() { s.shuttingDown.Store(true) }

func (s *WorkerService) Close(ctx context.Context) error {
	s.shuttingDown.Store(true)
	s.mu.Lock()
	s.closing = true
	ws := make([]*projectWorker, 0, len(s.workers))
	for _, pw := range s.workers {
		ws = append(ws, pw)
	}
	s.mu.Unlock()
	var first error
	for _, pw := range ws {
		if err := pw.w.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	drained := make(chan struct{})
	go func() { s.inflight.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-ctx.Done():
		if first == nil {
			first = ctx.Err()
		}
	}
	return first
}
