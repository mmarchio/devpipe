package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// Continuation service errors.
var (
	// ErrContinuationNotFound: no such continuation for the project.
	ErrContinuationNotFound = errors.New("service: continuation not found")
)

// ContinuationDeps wires the Phase 14 continuation controller.
type ContinuationDeps struct {
	Repo            persistence.ContinuationRepository
	Settings        persistence.ContinuationSettingsRepository
	ProjectSettings persistence.SettingsRepository
	Projects        persistence.ProjectRepository
	Runtime         persistence.RuntimeStateRepository
	Snapshots       persistence.ContextSnapshotRepository
	SnapshotSvc     *ExecutionSnapshotService
	Canonical       persistence.CanonicalPromptRepository
	Prompts         persistence.CompressionRepository
	Templates       persistence.PromptTemplateRepository
	TemplateVers    persistence.PromptTemplateVersionRepository
	Units           WorkUnitReader
	Dispatches      persistence.WorkerDispatchRepository
	Sessions        persistence.WorkerSessionRepository
	Responses       persistence.WorkerResponseRepository
	Attempts        persistence.ParseAttemptRepository
	Events          persistence.WorkerEventRepository

	Cache   *PromptCacheService
	Worker  *WorkerService
	Results *ResultService

	Policy continuation.Policy
	Logger *slog.Logger
	Now    func() time.Time
	Owner  string

	// ContinueTemplateKey is the continuation prompt template key
	// (default CONTINUE_WORK_UNIT, WBS 14.28).
	ContinueTemplateKey string
	// StepHook, when set, runs before every step; returning false stops
	// processing of that continuation (test seam for pause / crash
	// windows).
	StepHook func(c *continuation.Continuation) bool
}

// ContinuationService is the Phase 14 continuation controller: it decides
// how an unfinished work unit continues (same or fresh session), performs
// the session / context / CWD / dispatch sequence with persisted intent and
// confirmed states, consumes worker results exactly once and stays
// restart-safe. It never marks work units or phases DONE, advances the
// workflow, runs tests or lets worker text choose a strategy.
type ContinuationService struct {
	d ContinuationDeps

	runner *continuationRunner
	hookMu sync.RWMutex
}

// NewContinuationService builds the service.
func NewContinuationService(d ContinuationDeps) *ContinuationService {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Policy == nil {
		d.Policy = continuation.DefaultPolicy{}
	}
	if d.Owner == "" {
		d.Owner = "overseer-" + uuid.NewString()[:8]
	}
	if d.ContinueTemplateKey == "" {
		d.ContinueTemplateKey = "CONTINUE_WORK_UNIT"
	}
	return &ContinuationService{d: d}
}

// Owner is this instance's lease owner.
func (s *ContinuationService) Owner() string { return s.d.Owner }

// SetStepHook replaces the step hook (test seam).
func (s *ContinuationService) SetStepHook(h func(c *continuation.Continuation) bool) {
	s.hookMu.Lock()
	s.d.StepHook = h
	s.hookMu.Unlock()
}

func (s *ContinuationService) stepHook() func(c *continuation.Continuation) bool {
	s.hookMu.RLock()
	defer s.hookMu.RUnlock()
	return s.d.StepHook
}

// --- settings (P14-39) --------------------------------------------------------

// ContinuationSettings returns the project's continuation settings
// (defaults when never stored).
func (s *ContinuationService) ContinuationSettings(ctx context.Context, projectID uuid.UUID) (continuation.Settings, bool, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return continuation.Settings{}, false, translate(err)
	}
	st, ok, err := s.d.Settings.Get(ctx, projectID)
	if err != nil {
		return continuation.Settings{}, false, translate(err)
	}
	return st, ok, nil
}

// UpdateContinuationSettings validates and stores settings.
func (s *ContinuationService) UpdateContinuationSettings(ctx context.Context, projectID uuid.UUID, st continuation.Settings) (continuation.Settings, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return continuation.Settings{}, translate(err)
	}
	if err := st.Validate(); err != nil {
		return continuation.Settings{}, errors.Join(ErrInvalidInput, err)
	}
	if err := s.d.Settings.Upsert(ctx, projectID, st); err != nil {
		return continuation.Settings{}, translate(err)
	}
	return st, nil
}

// Threshold returns the project's context threshold (project setting
// context_threshold, WBS 14.5).
func (s *ContinuationService) Threshold(ctx context.Context, projectID uuid.UUID) (float64, error) {
	ps, err := s.d.ProjectSettings.Get(ctx, projectID.String())
	if err != nil {
		return 0, translate(err)
	}
	return ps.ContextThreshold, nil
}

// --- context reading (P14-40) -------------------------------------------------

// ReadingForDispatch returns the authoritative Phase 12 context telemetry
// of a dispatch (WBS 14.6, 14.11). Worker-authored result text is never
// consulted.
func (s *ContinuationService) ReadingForDispatch(ctx context.Context, d *worker.WorkerDispatch) (continuation.ContextReading, error) {
	sid := d.SessionID
	obs, err := s.d.Responses.ListObservations(ctx, d.ProjectID, &sid)
	if err != nil {
		return continuation.ContextReading{}, translate(err)
	}
	var mine []continuation.Observation
	for _, o := range obs {
		if o.DispatchID != nil && *o.DispatchID == d.ID {
			mine = append(mine, continuation.Observation{Usage: o.Usage, Source: o.Source, Seq: o.Seq, ObservedAt: o.ObservedAt})
		}
	}
	return continuation.SelectReading(mine), nil
}

// --- source snapshot (P14-42) -------------------------------------------------

// SourceSnapshotForDispatch resolves the execution-context snapshot a
// dispatch's prompt was composed from (worker prompt → canonical prompt →
// context snapshot).
func (s *ContinuationService) SourceSnapshotForDispatch(ctx context.Context, d *worker.WorkerDispatch) (*uuid.UUID, error) {
	wp, err := s.d.Prompts.GetWorkerPrompt(ctx, d.WorkerPromptID)
	if err != nil {
		return nil, translate(err)
	}
	cp, err := s.d.Canonical.Get(ctx, wp.CanonicalPromptID)
	if err != nil {
		return nil, translate(err)
	}
	id := cp.ContextSnapshotID
	return &id, nil
}

// --- events + logging (P14-43) ------------------------------------------------

func (s *ContinuationService) event(ctx context.Context, c *continuation.Continuation, typ worker.EventType, extra map[string]any) {
	if s.d.Events == nil {
		return
	}
	p := map[string]any{"continuation_id": c.ID.String(), "attempt": c.Attempt, "status": string(c.Status),
		"work_unit_id": c.WorkUnitID.String()}
	if c.Strategy != "" {
		p["strategy"] = string(c.Strategy)
	}
	if c.Decision != nil {
		p["decision_reason"] = string(c.Decision.Reason)
	}
	if c.RecoveryReason != "" {
		p["recovery_reason"] = string(c.RecoveryReason)
	}
	for k, v := range extra {
		p[k] = v
	}
	e := &persistence.WorkerEventRecord{ID: uuid.New(), ProjectID: c.ProjectID, SessionID: c.ConfirmedSession(), DispatchID: c.DispatchID,
		Type: typ, Payload: p, CreatedAt: s.d.Now()}
	if err := s.d.Events.Append(context.WithoutCancel(ctx), e); err != nil {
		s.d.Logger.Warn("continuation event not recorded", "type", string(typ), "continuation_id", c.ID.String(), "error", err.Error())
	}
}

// log writes the WBS 14.89 structured fields; prompts, worker status text
// and secrets are never logged.
func (s *ContinuationService) log(c *continuation.Continuation, msg string, extra ...any) {
	args := []any{"project_id", c.ProjectID.String(), "phase_id", c.PhaseID.String(), "work_unit_id", c.WorkUnitID.String(),
		"continuation_id", c.ID.String(), "attempt", c.Attempt, "status", string(c.Status)}
	if c.Strategy != "" {
		args = append(args, "strategy", string(c.Strategy))
	}
	if d := c.Decision; d != nil {
		args = append(args, "decision_reason", string(d.Reason), "threshold", d.Threshold)
		if d.Utilization != nil {
			args = append(args, "context_usage", *d.Utilization)
		} else {
			args = append(args, "context_usage", "unknown")
		}
	}
	if c.OldSessionID != nil {
		args = append(args, "old_session_id", c.OldSessionID.String())
	}
	if c.NewSessionID != nil {
		args = append(args, "new_session_id", c.NewSessionID.String())
	}
	if c.DispatchID != nil {
		args = append(args, "dispatch_id", c.DispatchID.String())
	}
	s.d.Logger.Info(msg, append(args, extra...)...)
}

// advance commits one transition and emits its event after commit.
func (s *ContinuationService) advance(ctx context.Context, c *continuation.Continuation, to continuation.Status, reason string,
	m persistence.ContinuationMutation) (*continuation.Continuation, error) {
	from := c.Status
	out, err := s.d.Repo.Advance(ctx, c.ID, c.Version, to, reason, m, s.d.Now())
	if err != nil {
		return nil, err
	}
	s.afterCommit(ctx, from, out, reason)
	return out, nil
}

func (s *ContinuationService) afterCommit(ctx context.Context, from continuation.Status, c *continuation.Continuation, reason string) {
	if typ, ok := continuation.EventFor(from, c.Status); ok {
		s.event(ctx, c, typ, map[string]any{"from": string(from), "reason": reason})
	}
	if c.Status == continuation.StatusRecoveryRequired && c.RecoveryReason == continuation.RecoveryNoProgress {
		s.event(ctx, c, continuation.EventNoProgress, map[string]any{"streak": c.NoProgressStreak})
	}
	s.log(c, "continuation transition", "from", string(from), "reason", reason)
}

func (s *ContinuationService) recovery(ctx context.Context, c *continuation.Continuation, r continuation.RecoveryReason, msg string,
	m persistence.ContinuationMutation) (*continuation.Continuation, error) {
	m.RecoveryReason = &r
	if msg != "" {
		m.FailureMessage = &msg
	}
	return s.advance(ctx, c, continuation.StatusRecoveryRequired, string(r), m)
}

// --- creation (P14-44, P14-46) ------------------------------------------------

// OnParseOutcome is the Phase 13 hook (P14-56): an accepted INCOMPLETE
// result of an ordinary dispatch creates a continuation; any outcome of a
// continuation dispatch is consumed by that continuation. Failures are
// logged; the parse outcome is never changed.
func (s *ContinuationService) OnParseOutcome(ctx context.Context, projectID, dispatchID uuid.UUID, res *persistence.StoredWorkerResult) {
	if err := s.handleParseOutcome(ctx, projectID, dispatchID, res); err != nil {
		s.d.Logger.Warn("continuation consumption deferred to recovery", "dispatch_id", dispatchID.String(), "error", err.Error())
	}
}

func (s *ContinuationService) handleParseOutcome(ctx context.Context, projectID, dispatchID uuid.UUID, res *persistence.StoredWorkerResult) error {
	d, err := s.d.Dispatches.Get(ctx, projectID, dispatchID)
	if err != nil {
		return err
	}
	if d.ContinuationID != nil {
		_, err := s.reconcileDispatch(ctx, *d.ContinuationID, projectID)
		return err
	}
	if res == nil || res.Result.Status != workerresult.StatusIncomplete {
		return nil
	}
	_, err = s.createFromResult(ctx, d, res)
	return err
}

func (s *ContinuationService) runtimeFor(ctx context.Context, d *worker.WorkerDispatch) (*persistence.RuntimeStateRecord, error) {
	rt, err := s.d.Runtime.Get(ctx, d.ProjectID)
	if err != nil {
		return nil, err
	}
	if rt.ID != d.RuntimeStateID {
		return nil, ErrContinuationBinding
	}
	return rt, nil
}

// prevStatus returns the previous captured status of the unit (for
// progress comparison) and its no-progress streak.
func prevStatus(counts persistence.WorkUnitCounts) (*continuation.CapturedStatus, int) {
	if counts.Latest == nil || counts.Latest.StatusUnknown {
		return nil, 0
	}
	cs := counts.Latest.Captured.Clone()
	return &cs, counts.Latest.NoProgressStreak
}

func (s *ContinuationService) newFromSource(ctx context.Context, d *worker.WorkerDispatch, trigger continuation.Trigger, res *persistence.StoredWorkerResult,
	captured continuation.CapturedStatus, unknown bool, retryOf *uuid.UUID) (*continuation.Continuation, error) {
	if d.PhaseID == nil || d.WorkUnitID == nil {
		return nil, ErrInvalidInput
	}
	rt, err := s.runtimeFor(ctx, d)
	if err != nil {
		return nil, err
	}
	counts, err := s.d.Repo.CountsForWorkUnit(ctx, d.ProjectID, *d.WorkUnitID)
	if err != nil {
		return nil, err
	}
	snap, err := s.SourceSnapshotForDispatch(ctx, d)
	if err != nil {
		return nil, err
	}
	streak, expanded := 1, false
	if !unknown {
		prev, prevStreak := prevStatus(counts)
		cmp := continuation.CompareProgress(prev, captured)
		streak, expanded = continuation.NextNoProgressStreak(prevStreak, cmp), cmp.ScopeExpanded
	}
	in := continuation.NewInput{ProjectID: d.ProjectID, PhaseID: *d.PhaseID, WorkUnitID: *d.WorkUnitID, RuntimeStateID: rt.ID,
		RuntimeStateVersion: rt.Version, Trigger: trigger, Attempt: continuation.NextAttempt(counts.MaxAttempt),
		SourceDispatch: continuation.Source{ID: d.ID, ProjectID: d.ProjectID, PhaseID: d.PhaseID, WorkUnitID: d.WorkUnitID, RuntimeStateID: d.RuntimeStateID},
		Captured:       captured, StatusUnknown: unknown, NoProgressStreak: streak, ScopeExpanded: expanded,
		SourceSnapshotID: snap, RoleKey: d.RoleKey, StateKey: d.StateKey, RetryOfID: retryOf, Now: s.d.Now()}
	if res != nil {
		m := res.Result.Metadata
		in.SourceResult = &continuation.Source{ID: res.ID, ProjectID: m.ProjectID, PhaseID: m.PhaseID, WorkUnitID: m.WorkUnitID}
	}
	return continuation.NewContinuation(in)
}

func captureResult(res *persistence.StoredWorkerResult) (continuation.CapturedStatus, error) {
	cs, err := res.Result.Continuation()
	if err != nil {
		return continuation.CapturedStatus{}, err
	}
	return continuation.Capture(cs), nil
}

// createFromResult consumes an accepted INCOMPLETE result of an ordinary
// dispatch and creates the PENDING continuation in one transaction (WBS
// 14.1, 14.16, 14.101): the same result never creates two continuations.
func (s *ContinuationService) createFromResult(ctx context.Context, d *worker.WorkerDispatch, res *persistence.StoredWorkerResult) (*continuation.Continuation, error) {
	if ok, err := s.d.Repo.IsConsumed(ctx, res.ID, persistence.ConsumerContinuation); err != nil || ok {
		return nil, err
	}
	captured, err := captureResult(res)
	if err != nil {
		return nil, err
	}
	c, err := s.newFromSource(ctx, d, continuation.TriggerResultIncomplete, res, captured, false, nil)
	if err != nil {
		return nil, err
	}
	id := c.ID
	err = s.d.Repo.Create(ctx, c, &persistence.ResultConsumption{ResultID: res.ID, Consumer: persistence.ConsumerContinuation,
		ProjectID: c.ProjectID, ContinuationID: &id, ConsumedAt: s.d.Now()})
	if errors.Is(err, persistence.ErrAlreadyConsumed) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.event(ctx, c, continuation.EventCreated, map[string]any{"trigger": string(c.Trigger), "source_dispatch_id": d.ID.String(),
		"source_result_id": res.ID.String(), "status_hash": c.StatusHash})
	s.log(c, "continuation created", "trigger", string(c.Trigger))
	s.Kick(c.ProjectID)
	return c, nil
}

// OnDispatchFailed is the Phase 12 failure hook (P14-46, WBS 14.44-14.46):
// an ordinary dispatch that failed with a context limit, or with a lost
// session before anything was sent, becomes a fresh-session continuation —
// without fabricating a WorkerResult. Delivery-unknown is never continued.
func (s *ContinuationService) OnDispatchFailed(ctx context.Context, d worker.WorkerDispatch) {
	if d.ContinuationID != nil || d.WorkUnitID == nil || d.PhaseID == nil {
		return
	}
	del := worker.DeliveryUnknown
	if d.Delivery != nil {
		del = *d.Delivery
	}
	fc := continuation.ClassifyDispatchFailure(worker.WorkerErrorCode(d.ErrorCode), del)
	if fc.Kind != continuation.FailureFresh {
		return
	}
	trigger := continuation.TriggerContextLimit
	if fc.Reason == continuation.ReasonSessionLost {
		trigger = continuation.TriggerSessionLost
	}
	if _, err := s.createFromFailure(ctx, &d, trigger); err != nil {
		s.d.Logger.Warn("continuation for failed dispatch not created", "dispatch_id", d.ID.String(), "error", err.Error())
	}
}

func (s *ContinuationService) createFromFailure(ctx context.Context, d *worker.WorkerDispatch, trigger continuation.Trigger) (*continuation.Continuation, error) {
	// WBS 14.45: the latest accepted INCOMPLETE status of the unit, if any;
	// otherwise progress is unknown and nothing is invented.
	captured, unknown := continuation.CapturedStatus{}, true
	if s.d.Results != nil {
		unit := *d.WorkUnitID
		rs, _, err := s.d.Results.Results(ctx, persistence.WorkerResultFilter{ProjectID: d.ProjectID, WorkUnitID: &unit,
			Status: workerresult.StatusIncomplete, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(rs) > 0 {
			full, err := s.d.Results.ResultByDispatch(ctx, d.ProjectID, rs[0].Result.Metadata.DispatchID)
			if err != nil {
				return nil, err
			}
			if cs, err := captureResult(full); err == nil {
				captured, unknown = cs, false
			}
		}
	}
	c, err := s.newFromSource(ctx, d, trigger, nil, captured, unknown, nil)
	if err != nil {
		return nil, err
	}
	if err := s.d.Repo.Create(ctx, c, nil); err != nil {
		return nil, err
	}
	s.event(ctx, c, continuation.EventCreated, map[string]any{"trigger": string(c.Trigger), "source_dispatch_id": d.ID.String(),
		"status_unknown": unknown})
	s.log(c, "continuation created", "trigger", string(c.Trigger))
	s.Kick(c.ProjectID)
	return c, nil
}

// --- consumption of a continuation dispatch's result (P14-45) -----------------

// consume closes a WAITING continuation from its dispatch's accepted
// result, and for INCOMPLETE creates the next continuation, atomically
// (WBS 14.60, 14.101). A continuation that is no longer waiting (e.g.
// cancelled) only records the consumption — no next continuation.
func (s *ContinuationService) consume(ctx context.Context, c *continuation.Continuation, d *worker.WorkerDispatch, res *persistence.StoredWorkerResult) (*continuation.Continuation, error) {
	cid := c.ID
	cons := persistence.ResultConsumption{ResultID: res.ID, Consumer: persistence.ConsumerContinuation, ProjectID: c.ProjectID,
		ContinuationID: &cid, ConsumedAt: s.d.Now()}
	if c.Status != continuation.StatusWaiting {
		if err := s.d.Repo.Consume(ctx, cons); err != nil && !errors.Is(err, persistence.ErrAlreadyConsumed) {
			return nil, err
		}
		return c, nil
	}
	out, err := continuation.OutcomeFor(res.Result.Status)
	if err != nil {
		return nil, err
	}
	to, reason := out.Target, "worker reported "+string(res.Result.Status)
	var m persistence.ContinuationMutation
	var next *continuation.Continuation
	if out.CreateNext {
		settings, _, err := s.d.Settings.Get(ctx, c.ProjectID)
		if err != nil {
			return nil, err
		}
		counts, err := s.d.Repo.CountsForWorkUnit(ctx, c.ProjectID, c.WorkUnitID)
		if err != nil {
			return nil, err
		}
		if continuation.ContinuationLimitReached(continuation.NextAttempt(counts.MaxAttempt), settings) {
			// WBS 14.37/14.112: no new continuation; recovery recorded.
			to, reason = continuation.StatusRecoveryRequired, string(continuation.RecoveryContinuationLimit)
			rr := continuation.RecoveryContinuationLimit
			m.RecoveryReason = &rr
		} else {
			captured, err := captureResult(res)
			if err != nil {
				return nil, err
			}
			n, err := s.newFromSource(ctx, d, continuation.TriggerResultIncomplete, res, captured, false, nil)
			if err != nil {
				return nil, err
			}
			next = n
		}
	}
	from := c.Status
	updated, err := s.d.Repo.ConsumeAndAdvance(ctx, cons, c.ID, c.Version, to, reason, m, s.d.Now(), next)
	if errors.Is(err, persistence.ErrAlreadyConsumed) {
		return s.d.Repo.Get(ctx, c.ProjectID, c.ID)
	}
	if err != nil {
		return nil, err
	}
	s.afterCommit(ctx, from, updated, reason)
	if next != nil {
		s.event(ctx, next, continuation.EventCreated, map[string]any{"trigger": string(next.Trigger), "source_dispatch_id": d.ID.String(),
			"source_result_id": res.ID.String(), "status_hash": next.StatusHash, "previous_continuation_id": c.ID.String()})
		s.log(next, "continuation created", "trigger", string(next.Trigger))
		s.Kick(c.ProjectID)
	}
	return updated, nil
}

// --- helpers ------------------------------------------------------------------

func (s *ContinuationService) projectStatus(ctx context.Context, projectID uuid.UUID) (project.ProjectStatus, error) {
	p, err := s.d.Projects.Get(ctx, projectID.String())
	if err != nil {
		return "", translate(err)
	}
	return p.Status, nil
}

func errMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return strings.TrimSpace(msg)
}

func wrapStep(c *continuation.Continuation, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("continuation %s (%s): %w", c.ID, c.Status, err)
}
