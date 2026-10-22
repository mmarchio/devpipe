package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// Lifecycle mirroring (P16-115, resolves Phase 15 Decision 20).

// OnProjectStatus mirrors committed project pause / resume / cancel onto
// the active execution (WBS 16.72–16.74): PAUSED stops new orchestration
// with the position untouched, RUNNING resumes the exact durable state,
// CANCELLED stops all future scheduling (no dispatch afterwards).
func (e *Engine) OnProjectStatus(ctx context.Context, projectID uuid.UUID, status project.ProjectStatus) {
	var to execution.ExecutionStatus
	var typ execution.EventType
	switch status {
	case project.StatusPAUSED:
		to, typ = execution.ExecutionPaused, execution.EventExecutionPaused
	case project.StatusRUNNING:
		to, typ = execution.ExecutionRunning, execution.EventExecutionResumed
	case project.StatusCANCELLED:
		to, typ = execution.ExecutionCancelled, execution.EventExecutionCancelled
	default:
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		rec, err := e.d.Execs.GetActive(ctx, nil, projectID)
		if err != nil {
			return
		}
		if rec.Status == to || !execution.CanTransition(rec.Status, to) {
			// e.g. RECOVERY_REQUIRED stays until resolved; READY is
			// activated by bootstrap.
			if !(to == execution.ExecutionPaused && rec.Status == execution.ExecutionRecovery) {
				return
			}
			return
		}
		id := rec.ID
		err = e.inTx(ctx, nil, func(tx persistence.Transaction) error {
			if _, err := e.d.Repo.UpdatePosition(ctx, tx, rec.ID, rec.Version, persistence.PositionUpdate{Status: to, Reason: "project " + string(status),
				SetWait: true}); err != nil {
				return err
			}
			return e.d.Execs.AppendEvent(ctx, tx, &execution.Event{ProjectID: projectID, ExecutionID: &id, Type: typ,
				Payload: map[string]any{"from": string(rec.Status)}, CreatedAt: e.d.Now().UTC()})
		})
		if errors.Is(err, persistence.ErrConflict) {
			continue
		}
		if err != nil {
			e.d.Logger.WarnContext(ctx, "execution lifecycle mirror failed", "project_id", projectID.String(), "error", err.Error())
		}
		break
	}
	if status == project.StatusCANCELLED && e.d.Continuations != nil {
		_, _ = e.d.Continuations.CancelProject(ctx, projectID)
	}
}

// ---------------------------------------------------------------------------
// Recovery resolution (P16-116).

// Recovery resolution actions.
const (
	RecoveryResume          = "RESUME"
	RecoveryRetryStep       = "RETRY_STEP"
	RecoveryApproveCriteria = "APPROVE_CRITERIA"
)

// Attempt limits: a recovery caused by one is classified by its reason,
// and resolving it resets that limit's counter (the counter counts from
// the resolution on).
const (
	LimitPlanAttempts     = "PLAN_ATTEMPTS"
	LimitFixAttempts      = "FIX_ATTEMPTS"
	LimitHardeningFixes   = "HARDENING_FIX_ATTEMPTS"
	LimitCommandResends   = "COMMAND_RESENDS"
	LimitStatefileResends = "STATEFILE_RESENDS"
	LimitProtocolRepairs  = "PROTOCOL_REPAIRS"
	LimitContinuations    = "CONTINUATIONS"
	// LimitIdenticalFailures counts consecutive identical failed
	// validations of a task (a replan resets it too).
	LimitIdenticalFailures = "IDENTICAL_FAILURES"
)

// Recovery reasons (or the parts that identify them) of the attempt limits.
const (
	reasonFixExhausted          = "fix attempts exhausted"
	reasonHardeningFixExhausted = "hardening fix attempts exhausted"
	reasonCommandResendsMarker  = " did not complete after "
	reasonStatefileMarker       = "(still wrong after "
	reasonProtocolPrefix        = "repeated worker protocol violations"
	reasonContinuationPrefix    = "continuation limit reached"
)

// attemptLimitOf returns the attempt limit behind a recovery reason, or ""
// when the recovery was not caused by one.
func attemptLimitOf(reason string) string {
	switch {
	case reason == recoveryPlanExhausted:
		return LimitPlanAttempts
	case reason == reasonFixExhausted:
		return LimitFixAttempts
	case reason == reasonHardeningFixExhausted:
		return LimitHardeningFixes
	case strings.Contains(reason, reasonStatefileMarker):
		return LimitStatefileResends
	case strings.Contains(reason, reasonCommandResendsMarker):
		return LimitCommandResends
	case strings.HasPrefix(reason, reasonProtocolPrefix):
		return LimitProtocolRepairs
	case strings.HasPrefix(reason, reasonContinuationPrefix):
		return LimitContinuations
	case strings.HasPrefix(reason, reasonIdenticalFailuresPrefix):
		return LimitIdenticalFailures
	}
	return ""
}

// limitResetAt is when the limit's counter was last reset in the
// execution (zero time when never).
func (e *Engine) limitResetAt(ctx context.Context, st *engineState, limit string) (time.Time, error) {
	at, err := e.d.Repo.LastLimitReset(ctx, nil, st.rec.ID, limit)
	if err != nil || at == nil {
		return time.Time{}, err
	}
	return *at, nil
}

// RecoveryResolution is an operator's explicit decision (WBS 16.76).
type RecoveryResolution struct {
	Action     string `json:"action"`
	WorkUnitID string `json:"work_unit_id,omitempty"`
	Indexes    []int  `json:"check_indexes,omitempty"`
	By         string `json:"by,omitempty"`
	Note       string `json:"note,omitempty"`
}

// ResolveRecovery returns a RECOVERY_REQUIRED execution to RUNNING after an
// explicit operator decision; the next step re-evaluates the current state
// from persisted evidence. Never automatic.
func (e *Engine) ResolveRecovery(ctx context.Context, projectID, executionID uuid.UUID, r RecoveryResolution) (*persistence.ExecutionRecord, error) {
	rec, err := e.d.Execs.Get(ctx, nil, projectID, executionID)
	if err != nil {
		return nil, translate(err)
	}
	if rec.Status != execution.ExecutionRecovery {
		return nil, ErrConflict
	}
	if e.taskOpRunning(executionID) {
		// The ARCHITECT is analyzing or fixing the task in the worker
		// session; the engine must not start sending meanwhile.
		return nil, ErrTaskOpRunning
	}
	switch r.Action {
	case RecoveryResume, RecoveryRetryStep:
	case RecoveryApproveCriteria:
		uid, err := uuid.Parse(r.WorkUnitID)
		if err != nil || len(r.Indexes) == 0 {
			return nil, ErrInvalidInput
		}
		by := r.By
		if by == "" {
			by = "operator"
		}
		for _, i := range r.Indexes {
			if i < 0 {
				return nil, ErrInvalidInput
			}
			if err := e.d.Repo.ApproveCriterion(ctx, executionID, uid, i, by, r.Note); err != nil {
				return nil, translate(err)
			}
		}
	default:
		return nil, ErrInvalidInput
	}
	// A hardening pass stopped by an external modification resumes at its
	// interrupted status with the repository as the operator left it.
	if pass, err := e.d.Repo.GetPass(ctx, nil, executionID, rec.FinalizationGeneration); err == nil && pass.Status == hardening.PassRecoveryRequired {
		fp := ""
		if e.d.Inspector != nil {
			snap, _ := e.d.Execs.GetSnapshot(ctx, executionID)
			if snap != nil {
				if c, err := execution.DecodeContent(snap.Canonical); err == nil {
					if s, err := e.d.Inspector.Inspect(ctx, c.WorkingDirectory); err == nil {
						fp = s.Fingerprint
					}
				}
			}
		}
		next := *pass
		next.Status, next.ResumeStatus, next.RecoveryReason, next.RepoFingerprint = hardening.PassExecuting, nil, "", fp
		if pass.ResumeStatus != nil {
			next.Status = *pass.ResumeStatus
		}
		if _, err := e.d.Repo.UpdatePass(ctx, nil, &next); err != nil {
			return nil, translate(err)
		}
	}
	var out *persistence.ExecutionRecord
	err = e.inTx(ctx, nil, func(tx persistence.Transaction) error {
		var err error
		out, err = e.d.Repo.UpdatePosition(ctx, tx, rec.ID, rec.Version, persistence.PositionUpdate{Status: execution.ExecutionRunning,
			Reason: "recovery resolved: " + r.Action, SetRecovery: true, SetWait: true})
		if err != nil {
			return err
		}
		id := rec.ID
		payload := map[string]any{"action": r.Action, "reason": rec.RecoveryReason, "by": r.By, "note": r.Note}
		if limit := attemptLimitOf(rec.RecoveryReason); limit != "" {
			// Resolving an attempt-limit recovery resets that counter.
			payload["limit"] = limit
		}
		return e.d.Execs.AppendEvent(ctx, tx, &execution.Event{ProjectID: projectID, ExecutionID: &id, Type: execution.EventRecoveryResolved,
			Payload: payload, CreatedAt: e.d.Now().UTC()})
	})
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Restart recovery (P16-117).

// RecoverExecutions reconciles engine state at startup (WBS 16.77–16.80):
// in-flight validation runs are abandoned (PASS is never inferred; the
// step reruns them); pending step intents stay and are resumed by the next
// step — dispatches through Phase 12 reconciliation (an unknown delivery
// is never resent), checkpoints through the git trailer lookup, hardening
// operations through source observation. No conversation memory is used.
func (e *Engine) RecoverExecutions(ctx context.Context) (int, error) {
	list, err := e.d.Repo.ListEngineActive(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range list {
		runs, err := e.d.Repo.ListRunningValidations(ctx, rec.ID)
		if err != nil {
			return n, err
		}
		for _, r := range runs {
			if err := e.d.Repo.FinishValidationRun(ctx, nil, r.ID, persistence.RunAbandoned, nil, nil); err != nil && !errors.Is(err, persistence.ErrConflict) {
				return n, err
			}
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Scheduler (P16-118).

type engineRunner struct {
	// cancel stops the scheduling loop; cancelWork aborts running drives
	// (only after the shutdown drain expired).
	cancel     context.CancelFunc
	cancelWork context.CancelFunc
	work       context.Context
	done       chan struct{}
	kick       chan struct{}
	wg         sync.WaitGroup
}

var engineRunnerMu sync.Mutex

// Start runs the scheduler: every interval (or kick) it lists schedulable
// executions (WBS 16.6), picks a fair batch (16.7) and drives each through
// Run with the project's step budget under the driver lease (16.4).
// Completed / paused / recovery executions are never listed (16.159).
func (e *Engine) Start(interval time.Duration, workers int) {
	engineRunnerMu.Lock()
	defer engineRunnerMu.Unlock()
	if e.runner != nil {
		return
	}
	if interval <= 0 {
		interval = time.Second
	}
	if workers <= 0 {
		workers = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	work, cancelWork := context.WithCancel(context.Background())
	r := &engineRunner{cancel: cancel, cancelWork: cancelWork, work: work, done: make(chan struct{}), kick: make(chan struct{}, 1)}
	e.draining.Store(false)
	e.runner = r
	go func() {
		defer close(r.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			e.ScheduleOnce(ctx, workers)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-r.kick:
			}
		}
	}()
}

// currentRunner reads the scheduler runner under its mutex.
func (e *Engine) currentRunner() *engineRunner {
	engineRunnerMu.Lock()
	defer engineRunnerMu.Unlock()
	return e.runner
}

// Kick triggers a scheduling round now.
func (e *Engine) Kick() {
	if r := e.currentRunner(); r != nil {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// Shutdown stops starting steps and drains (WBS 18.125, decision U6):
// the scheduler stops, no new step starts, running steps (worker calls
// included) may finish until ctx expires; then they are cancelled — a
// worker call cut short stays reconcilable, never FAILED (WBS 18.126) —
// and every driver lease this process still holds is released so another
// instance can take over immediately.
func (e *Engine) Shutdown(ctx context.Context) {
	e.draining.Store(true)
	engineRunnerMu.Lock()
	r := e.runner
	e.runner = nil
	engineRunnerMu.Unlock()
	if r != nil {
		r.cancel()
		waited := make(chan struct{})
		go func() { <-r.done; r.wg.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-ctx.Done():
			r.cancelWork()
			select {
			case <-waited:
			case <-time.After(5 * time.Second):
			}
		}
		r.cancelWork()
	}
	e.heldMu.Lock()
	held := make([]*driver, 0, len(e.held))
	for _, d := range e.held {
		held = append(held, d)
	}
	e.held = map[uuid.UUID]*driver{}
	e.heldMu.Unlock()
	for _, d := range held {
		_ = e.d.Repo.ReleaseDriver(context.WithoutCancel(ctx), d.id, e.d.Owner, d.epoch)
	}
}

// Draining reports whether shutdown began.
func (e *Engine) Draining() bool { return e.draining.Load() }

// ScheduleOnce performs one scheduling round synchronously and returns the
// executions driven.
func (e *Engine) ScheduleOnce(ctx context.Context, workers int) []uuid.UUID {
	rows, err := e.d.Repo.ListSchedulable(ctx, e.d.Now(), 200)
	if err != nil {
		e.d.Logger.WarnContext(ctx, "list schedulable executions", "error", err.Error())
		return nil
	}
	var cands []execution.SchedulingCandidate
	for _, r := range rows {
		cands = append(cands, r.SchedulingCandidate)
	}
	batch := execution.SelectSchedulingBatch(cands, workers)
	var wg sync.WaitGroup
	var out []uuid.UUID
	var mu sync.Mutex
	runner := e.currentRunner()
	for _, c := range batch {
		c := c
		if e.draining.Load() {
			break
		}
		wg.Add(1)
		if runner != nil {
			runner.wg.Add(1)
		}
		go func() {
			defer wg.Done()
			if runner != nil {
				defer runner.wg.Done()
			}
			defer containPanic(e.d.Logger, "execution drive", "execution_id", c.ExecutionID.String())
			_ = e.d.Repo.TouchScheduled(ctx, c.ExecutionID, e.d.Now())
			budget := execution.DefaultEngineSettings().StepBudget
			if snap, err := e.d.Execs.GetSnapshot(ctx, c.ExecutionID); err == nil {
				if content, err := execution.DecodeContent(snap.Canonical); err == nil {
					budget = content.EngineSettingsOrDefault().StepBudget
				}
			}
			runCtx := ctx
			if runner != nil {
				// Drives outlive the scheduling loop until the drain ends.
				runCtx = runner.work
			}
			if _, err := e.Run(runCtx, c.ExecutionID, budget); err != nil && !errors.Is(err, ErrExecutionBusy) && runCtx.Err() == nil {
				e.d.Logger.WarnContext(ctx, "execution run", "execution_id", c.ExecutionID.String(), "error", err.Error())
			}
			mu.Lock()
			out = append(out, c.ExecutionID)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// ---------------------------------------------------------------------------
// Read models (P16-137).

// ExecutionEngineView is the engine's view of one execution.
type ExecutionEngineView struct {
	Execution    *persistence.ExecutionRecord  `json:"-"`
	State        string                        `json:"state"`
	Summary      execution.ExecutionSummary    `json:"summary"`
	WBSProgress  wbs.Progress                  `json:"wbs_progress"`
	CurrentPhase *uuid.UUID                    `json:"current_phase_id,omitempty"`
	CurrentUnit  *uuid.UUID                    `json:"current_work_unit_id,omitempty"`
	Completion   *persistence.CompletionRecord `json:"completion,omitempty"`
}

// execFor loads a project-scoped execution.
func (e *Engine) execFor(ctx context.Context, projectID, executionID uuid.UUID) (*persistence.ExecutionRecord, error) {
	rec, err := e.d.Execs.Get(ctx, nil, projectID, executionID)
	if err != nil {
		return nil, translate(err)
	}
	return rec, nil
}

// Timeline returns the execution's events in sequence (WBS 16.81/16.82).
func (e *Engine) Timeline(ctx context.Context, projectID, executionID uuid.UUID) ([]execution.Event, error) {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return nil, err
	}
	return e.d.Repo.ListTimeline(ctx, executionID)
}

// Steps pages the execution's steps.
func (e *Engine) Steps(ctx context.Context, projectID, executionID uuid.UUID, limit, offset int) ([]*persistence.StepRecord, int, error) {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return nil, 0, err
	}
	return e.d.Repo.ListSteps(ctx, executionID, limit, offset)
}

// ValidationRuns lists the execution's deterministic validation runs.
func (e *Engine) ValidationRuns(ctx context.Context, projectID, executionID uuid.UUID) ([]*persistence.ValidationRunRecord, error) {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return nil, err
	}
	return e.d.Repo.ListValidationRuns(ctx, executionID)
}

// Checkpoints lists the execution's repository checkpoints.
func (e *Engine) Checkpoints(ctx context.Context, projectID, executionID uuid.UUID) ([]*persistence.RepoCheckpointRecord, error) {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return nil, err
	}
	return e.d.Repo.ListRepoCheckpoints(ctx, executionID)
}

// FixAttempts lists the execution's repair attempts.
func (e *Engine) FixAttempts(ctx context.Context, projectID, executionID uuid.UUID) ([]*persistence.FixAttemptRecord, error) {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return nil, err
	}
	return e.d.Repo.ListFixAttempts(ctx, executionID, nil)
}

// View derives the execution summary from persisted records (WBS 16.161).
func (e *Engine) View(ctx context.Context, projectID, executionID uuid.UUID) (*ExecutionEngineView, error) {
	rec, err := e.execFor(ctx, projectID, executionID)
	if err != nil {
		return nil, err
	}
	v := &ExecutionEngineView{Execution: rec, CurrentPhase: rec.CurrentPhaseID, CurrentUnit: rec.CurrentWorkUnitID}
	if rt, err := e.d.Execs.GetRuntimeByExecution(ctx, nil, executionID); err == nil {
		if s, err := e.d.States.Get(ctx, rt.CurrentStateID); err == nil {
			v.State = s.Key
		}
	}
	phases, err := e.d.Phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	var units []*wbs.WorkUnit
	for _, p := range phases {
		plans, err := e.d.Repo.ListPhasePlans(ctx, nil, p.ID)
		if err != nil {
			return nil, translate(err)
		}
		if ap := wbs.ActivePlan(p.ID, plans); ap != nil {
			us, err := e.d.Repo.ListPlanUnits(ctx, nil, ap.ID)
			if err != nil {
				return nil, translate(err)
			}
			units = append(units, us...)
		}
	}
	in := execution.SummaryInput{Phases: phases, CurrentPlanUnits: units, Now: e.d.Now(), StartedAt: rec.RunningAt, CompletedAt: rec.CompletedAt}
	in.Dispatches, in.Continuations, in.SessionResets, _, _ = e.d.Execs.ActivityCounts(ctx, projectID)
	if runs, err := e.d.Repo.ListValidationRuns(ctx, executionID); err == nil {
		in.ValidationRuns = len(runs)
	}
	if fs, err := e.d.Repo.ListFixAttempts(ctx, executionID, nil); err == nil {
		in.FixAttempts = len(fs)
	}
	if cps, err := e.d.Repo.ListRepoCheckpoints(ctx, executionID); err == nil {
		for _, c := range cps {
			if c.Status == persistence.CheckpointCreated {
				in.Checkpoints++
			}
		}
	}
	if passes, err := e.d.Repo.ListPasses(ctx, executionID); err == nil {
		for _, p := range passes {
			gs, _ := e.d.Repo.ListGroups(ctx, p.ID, 0)
			in.HardeningGroups += len(gs)
			for _, g := range gs {
				in.DuplicatesRemoved += g.FunctionsRemoved
				if g.TestsBefore > g.TestsAfter && g.Status == hardening.GroupConsolidated {
					in.TestsConsolidated += g.TestsBefore - g.TestsAfter
				}
			}
		}
	}
	v.Summary = execution.Summarize(in)
	v.WBSProgress = wbs.PlanProgress(units)
	if c, err := e.d.Repo.GetCompletion(ctx, executionID); err == nil {
		v.Completion = c
	}
	return v, nil
}

// HardeningView is the hardening read model (WBS 16.145–16.148).
type HardeningView struct {
	Pass     *persistence.HardeningPassRecord    `json:"pass"`
	Progress hardening.Progress                  `json:"progress"`
	Report   *hardening.Report                   `json:"report,omitempty"`
	Groups   []*persistence.HardeningGroupRecord `json:"groups"`
}

// Hardening returns the execution's current hardening pass, its progress
// (separate from WBS progress), groups with lineage and the report.
func (e *Engine) Hardening(ctx context.Context, projectID, executionID uuid.UUID) (*HardeningView, error) {
	rec, err := e.execFor(ctx, projectID, executionID)
	if err != nil {
		return nil, err
	}
	pass, err := e.d.Repo.GetPass(ctx, nil, executionID, rec.FinalizationGeneration)
	if err != nil {
		return nil, translate(err)
	}
	groups, err := e.d.Repo.ListGroups(ctx, pass.ID, 0)
	if err != nil {
		return nil, translate(err)
	}
	v := &HardeningView{Pass: pass, Groups: groups}
	var recs []hardening.GroupRecord
	cur, op := "", ""
	for _, g := range groups {
		if g.Round != pass.Round {
			continue
		}
		recs = append(recs, hardening.GroupRecord{ID: g.Key, Round: g.Round, Classification: g.Classification, Reason: g.Reason, Status: g.Status})
		if g.Status == hardening.GroupInProgress || g.Status == hardening.GroupFailed {
			cur = g.Key
			if ops, err := e.d.Repo.ListOperations(ctx, g.ID); err == nil {
				for _, o := range ops {
					if o.Status != hardening.OpDone {
						op = string(o.Kind)
						break
					}
				}
			}
		}
	}
	v.Progress = hardening.ComputeProgress(pass.Round, recs, cur, op)
	if r, err := e.d.Repo.GetReport(ctx, pass.ID); err == nil {
		v.Report = r
	}
	return v, nil
}

// EngineSettings returns the project's engine + hardening settings.
func (e *Engine) EngineSettings(ctx context.Context, projectID uuid.UUID) (execution.EngineSettings, hardening.Settings, bool, error) {
	if _, err := e.d.Projects.Get(ctx, projectID.String()); err != nil {
		return execution.EngineSettings{}, hardening.Settings{}, false, translate(err)
	}
	return e.d.Repo.GetEngineSettings(ctx, projectID)
}

// UpdateEngineSettings validates and stores the settings (P16-76).
func (e *Engine) UpdateEngineSettings(ctx context.Context, projectID uuid.UUID, s execution.EngineSettings, h hardening.Settings) error {
	if _, err := e.d.Projects.Get(ctx, projectID.String()); err != nil {
		return translate(err)
	}
	if err := errors.Join(s.Validate(), h.Validate()); err != nil {
		return errors.Join(ErrInvalidInput, err)
	}
	if err := e.d.Repo.UpsertEngineSettings(ctx, projectID, s, h); err != nil {
		e.d.Logger.ErrorContext(ctx, "engine settings not stored", "error", err.Error())
		return translate(err)
	}
	return nil
}

// Execution returns a project-scoped execution.
func (e *Engine) Execution(ctx context.Context, projectID, executionID uuid.UUID) (*persistence.ExecutionRecord, error) {
	return e.execFor(ctx, projectID, executionID)
}

// NewEngineForTest returns a second engine sharing e's dependencies under
// another driver identity (multi-instance tests).
func NewEngineForTest(e *Engine, owner string) *Engine {
	d := e.d
	d.Owner = owner
	return NewEngine(d)
}
