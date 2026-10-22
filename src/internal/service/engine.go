// Phase 16 (P16-81 … P16-86): the autonomous execution engine. Workers
// report what happened; the engine decides what happens next. Every Step
// reads persisted state, resolves exactly one action for the runtime's
// current workflow state, commits an INTENT step row before any external
// action, performs the action through the generic Phase 12–14 services,
// records the result, and applies the workflow transition (runtime +
// history + execution position + event) in one transaction.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/compression"
	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/gitcheckpoint"
	"overseer/internal/hardening"
	"overseer/internal/logging"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/project"
	"overseer/internal/recovery"
	"overseer/internal/resilience"
	"overseer/internal/state"
	"overseer/internal/wbs"
	"overseer/internal/worker"
)

// ValidationRunner executes deterministic validation commands
// (internal/valrun in production).
type ValidationRunner interface {
	RunAll(ctx context.Context, cmds []wbs.ValidationSpec, cwd string, timeout time.Duration, outputCap int) []execution.ValidationRecord
}

// Checkpointer creates and reconciles git checkpoints
// (internal/gitcheckpoint in production).
type Checkpointer interface {
	Create(ctx context.Context, dir, message string) (*gitcheckpoint.Commit, error)
	FindByKey(ctx context.Context, dir, key string) (*gitcheckpoint.Commit, error)
	FindAllByKey(ctx context.Context, dir, key string) ([]*gitcheckpoint.Commit, error)
}

// EngineDeps wires the engine.
type EngineDeps struct {
	Pool   *pgxpool.Pool
	Execs  persistence.ExecutionRepository
	Repo   persistence.EngineRepository
	Plans  *dbrepo.PlanRepository
	Units  *dbrepo.WorkUnitRepository
	Phases persistence.PhaseRepository

	Projects        persistence.ProjectRepository
	ProjectSettings persistence.SettingsRepository
	WBSVersions     persistence.WBSVersionRepository
	Machines        persistence.StateMachineRepository
	States          persistence.WorkflowStateRepository
	Runtime         persistence.RuntimeStateRepository
	Dispatches      persistence.WorkerDispatchRepository
	Prompts         WorkerPromptReader
	// CommandPrompts stores the statefile workflow's command prompts;
	// TemplateVers resolves their captured command templates.
	CommandPrompts CommandPromptWriter
	TemplateVers   persistence.PromptTemplateVersionRepository

	ContextSnaps *ExecutionSnapshotService
	Cache        *PromptCacheService
	Worker       *WorkerService
	// Recovery stores operator delivery resolutions (Phase 18; nil =
	// none are applied).
	Recovery      persistence.RecoveryStore
	Results       *ResultService
	Continuations *ContinuationService
	ActiveTask    *ActiveTaskService
	StateFiles    *StateFileService
	Lifecycle     *LifecycleService

	Runner       ValidationRunner
	Checkpointer Checkpointer
	Inspector    execution.RepositoryInspector
	// MaxValidations bounds simultaneous validation command runs across
	// executions (WBS 18.93; 0 = unlimited).
	MaxValidations int
	// Invariants checks durable, execution-scoped invariants before and
	// after every side effect (WBS 18.178 / 18.179); nil disables it.
	Invariants InvariantScanner
	// Identifier / Identities back the repository guard (WBS 18.48 /
	// 18.49 / 18.53); nil disables it (unit scope).
	Identifier execution.RepositoryIdentifier
	Identities persistence.RepositoryIdentityStore
	// GoBinary is the go command used by hardening analysis (default "go").
	GoBinary string

	Owner  string
	Logger *slog.Logger
	// Gate blocks new worker requests process-wide (nil = never).
	Gate *WorkGate
	// GC runs the bounded garbage collection immediately before each
	// phase's planning step (decision U1); nil = none. A failure is logged
	// and never blocks planning.
	GC  func(ctx context.Context) error
	Now func() time.Time
}

// CommandPromptWriter persists a command worker prompt.
type CommandPromptWriter interface {
	CreateWorkerPrompt(ctx context.Context, projectID uuid.UUID, wp *compression.WorkerPrompt) error
}

// Engine is the WBS 16.1 execution engine.
type Engine struct {
	d     EngineDeps
	files derivedFileWriter
	locks sync.Map
	// stepHook is a test seam run after the intent is committed and before
	// the action's external effect (crash / concurrency injection).
	stepHook func(ctx context.Context, action execution.StepAction, stage string) error
	runner   *engineRunner
	// draining: shutdown began — no new step starts (WBS 18.125).
	draining atomic.Bool
	// held are the driver leases this process holds (released at
	// shutdown, WBS 18.125).
	heldMu sync.Mutex
	held   map[uuid.UUID]*driver
	// validations bounds simultaneous validation command runs (WBS 18.93;
	// nil = unlimited).
	validations chan struct{}
	// taskOps holds the recovery-screen task analysis / fix running per
	// execution (TaskOpAnalyze / TaskOpFix).
	taskOps sync.Map
}

// Engine errors.
var (
	// ErrExecutionBusy: another scheduler holds the execution's driver
	// lease (WBS 16.4).
	ErrExecutionBusy = errors.New("service: execution is driven by another scheduler")
	// ErrNotSchedulable: the execution is not RUNNING / WAITING.
	ErrNotSchedulable = errors.New("service: execution is not schedulable")
)

// NewEngine wires the engine.
func NewEngine(d EngineDeps) *Engine {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Owner == "" {
		d.Owner = "overseer-" + uuid.NewString()[:8]
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	e := &Engine{d: d}
	if d.MaxValidations > 0 {
		e.validations = make(chan struct{}, d.MaxValidations)
	}
	if d.StateFiles != nil {
		e.files = derivedFileWriter{crud: d.StateFiles}
	}
	return e
}

// SetStepHook installs the test seam: stage is "intent" (after the intent
// commit) or an action-specific point ("dispatch", "validation",
// "checkpoint", "hardening:<op>").
func (e *Engine) SetStepHook(h func(ctx context.Context, action execution.StepAction, stage string) error) {
	e.stepHook = h
}

func (e *Engine) hook(ctx context.Context, a execution.StepAction, stage string) error {
	if e.stepHook == nil {
		return nil
	}
	return e.stepHook(ctx, a, stage)
}

// Owner is this engine's driver identity.
func (e *Engine) Owner() string { return e.d.Owner }

func (e *Engine) lock(id uuid.UUID) *sync.Mutex {
	m, _ := e.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// inTx runs fn in a transaction that is retried on transient failures
// (WBS 18.35). Closures update st.rec / st.runtime with the rows they
// wrote; before a retry those are restored to their pre-transaction
// values, since the failed attempt's writes were rolled back. st may be
// nil when the closure touches no engine state.
func (e *Engine) inTx(ctx context.Context, st *engineState, fn func(tx persistence.Transaction) error) error {
	var reset func()
	if st != nil {
		rec, rt := st.rec, st.runtime
		reset = func() { st.rec, st.runtime = rec, rt }
	}
	return dbrepo.RunWithReset(ctx, e.d.Pool, reset, fn)
}

// ---------------------------------------------------------------------------
// Driver lease (P16-81).

// driver is a held lease.
type driver struct {
	id    uuid.UUID
	epoch int64
}

func (e *Engine) leaseTTL(ctx context.Context, st *engineState) time.Duration {
	if st != nil && st.engine.DriverLease > 0 {
		return st.engine.DriverLease
	}
	return execution.DefaultEngineSettings().DriverLease
}

// acquire takes the driver lease (WBS 16.4).
func (e *Engine) acquire(ctx context.Context, id uuid.UUID, ttl time.Duration) (*driver, error) {
	epoch, ok, err := e.d.Repo.AcquireDriver(ctx, id, e.d.Owner, e.d.Now(), ttl)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrExecutionBusy
	}
	d := &driver{id: id, epoch: epoch}
	e.heldMu.Lock()
	if e.held == nil {
		e.held = map[uuid.UUID]*driver{}
	}
	e.held[id] = d
	e.heldMu.Unlock()
	return d, nil
}

func (e *Engine) release(ctx context.Context, d *driver) {
	e.heldMu.Lock()
	if e.held[d.id] == d {
		delete(e.held, d.id)
	}
	e.heldMu.Unlock()
	_ = e.d.Repo.ReleaseDriver(context.WithoutCancel(ctx), d.id, e.d.Owner, d.epoch)
}

// ErrFenced: this instance no longer holds the execution's driver lease
// at its epoch (a newer holder took over, WBS 18.14). Nothing is written
// and no external side effect starts.
var ErrFenced = fmt.Errorf("%w: driver lease lost (fenced)", persistence.ErrConflict)

// fence verifies the lease inside a write transaction; a lost lease
// writes nothing (ErrFenced).
func (e *Engine) fence(ctx context.Context, tx persistence.Transaction, st *engineState) error {
	if st.drv == nil {
		return nil
	}
	if err := e.d.Repo.CheckDriver(ctx, tx, st.rec.ID, e.d.Owner, st.drv.epoch, e.d.Now()); err != nil {
		return ErrFenced
	}
	return nil
}

// fenceNow re-verifies the lease immediately before an external side
// effect (worker send, validation process, git commit, hardening edit):
// an instance that paused after its last fenced transaction and lost the
// lease meanwhile never performs the effect (WBS 18.14).
func (e *Engine) fenceNow(ctx context.Context, st *engineState) error {
	return e.fence(ctx, nil, st)
}

// ---------------------------------------------------------------------------
// Engine state (P16-82).

// engineState is everything one step reads — reconstructed from the
// database on every step, never kept in memory (WBS 16.77).
type engineState struct {
	drv      *driver
	rec      *persistence.ExecutionRecord
	project  *project.Project
	content  execution.ConfigurationContent
	engine   execution.EngineSettings
	hard     hardening.Settings
	machine  *state.StateMachineSnapshot
	byKey    map[string]*persistence.WorkflowStateRecord
	byID     map[uuid.UUID]*persistence.WorkflowStateRecord
	runtime  *persistence.RuntimeStateRecord
	cur      state.StateSnapshot
	curRec   *persistence.WorkflowStateRecord
	phases   []*phase.Phase
	posPhase *phase.Phase
	current  *phase.Phase
	wbsVer   int
	wbsHash  string
	plans    []*wbs.ExecutionPlan
	plan     *wbs.ExecutionPlan
	units    []*wbs.WorkUnit
	unit     *wbs.WorkUnit
	last     *persistence.StepRecord
}

func (st *engineState) cwd() string { return st.content.WorkingDirectory }

func (st *engineState) phaseByID(id *uuid.UUID) *phase.Phase {
	if id == nil {
		return nil
	}
	for _, p := range st.phases {
		if p.ID == *id {
			return p
		}
	}
	return nil
}

// load reconstructs the engine state of an execution (P16-82).
func (e *Engine) load(ctx context.Context, id uuid.UUID) (*engineState, error) {
	st := &engineState{byKey: map[string]*persistence.WorkflowStateRecord{}, byID: map[uuid.UUID]*persistence.WorkflowStateRecord{}}
	var err error
	if st.rec, err = e.d.Execs.GetByID(ctx, nil, id); err != nil {
		return nil, err
	}
	if st.project, err = e.d.Projects.Get(ctx, st.rec.ProjectID.String()); err != nil {
		return nil, err
	}
	snap, err := e.d.Execs.GetSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.content, err = execution.DecodeContent(snap.Canonical); err != nil {
		return nil, err
	}
	st.engine, st.hard = st.content.EngineSettingsOrDefault(), st.content.HardeningOrDefault()
	st.machine = &st.content.StateMachine
	mrec, err := e.d.Machines.GetVersion(ctx, st.rec.StateMachineID, st.rec.StateMachineVersion)
	if err != nil {
		return nil, err
	}
	recs, err := e.d.States.ListByMachine(ctx, mrec.ID, st.rec.StateMachineVersion)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		st.byKey[r.Key], st.byID[r.ID] = r, r
	}
	if st.runtime, err = e.d.Execs.GetRuntimeByExecution(ctx, nil, id); err != nil {
		return nil, err
	}
	st.curRec = st.byID[st.runtime.CurrentStateID]
	if st.curRec == nil {
		return nil, fmt.Errorf("%w: runtime state is not part of the bound machine", ErrInvalidInput)
	}
	for _, s := range st.machine.States {
		if s.Key == st.curRec.Key {
			st.cur = s
		}
	}
	if st.cur.Key == "" {
		st.cur = state.StateSnapshot{Key: st.curRec.Key, Kind: st.curRec.Kind, Terminal: st.curRec.Terminal, Enabled: st.curRec.Enabled}
	}
	if st.phases, err = e.d.Phases.ListByProject(ctx, st.rec.ProjectID); err != nil {
		return nil, err
	}
	pos := st.runtime.PhaseID
	if pos == nil {
		pos = st.rec.CurrentPhaseID
	}
	st.posPhase = st.phaseByID(pos)
	st.current = wbs.CurrentPhase(st.phases)
	// The working phase is the recorded position while it is not DONE
	// (WBS 16.9 / 16.71: an appended phase never preempts it).
	work := st.posPhase
	if work == nil || work.Status == phase.StatusDONE {
		work = st.current
	}
	if work != nil {
		if st.wbsVer, err = e.d.WBSVersions.GetCurrentVersion(ctx, work.ID.String()); err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return nil, err
		}
		if st.wbsVer > 0 {
			if v, err := e.d.WBSVersions.GetVersion(ctx, work.ID.String(), st.wbsVer); err == nil {
				st.wbsHash = v.ContentHash
			}
		}
		if st.plans, err = e.d.Repo.ListPhasePlans(ctx, nil, work.ID); err != nil {
			return nil, err
		}
		st.plan = wbs.ActivePlan(work.ID, st.plans)
		if st.plan != nil {
			if st.units, err = e.d.Repo.ListPlanUnits(ctx, nil, st.plan.ID); err != nil {
				return nil, err
			}
		}
	}
	if uid := st.runtime.WorkUnitID; uid != nil {
		for _, u := range st.units {
			if u.ID == *uid {
				st.unit = u
			}
		}
		if st.unit == nil {
			if u, err := e.d.Units.Get(ctx, *uid); err == nil {
				st.unit = u
			}
		}
	}
	if st.last, err = e.d.Repo.LatestStep(ctx, nil, id); err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	return st, nil
}

// workPhase is the phase the engine is working on.
func (st *engineState) workPhase() *phase.Phase {
	if st.posPhase != nil && st.posPhase.Status != phase.StatusDONE {
		return st.posPhase
	}
	return st.current
}

// ---------------------------------------------------------------------------
// Step / Run (P16-83 / P16-85).

// stepHandler performs one action. It must finish the step (applied,
// failed, recovery) or leave it INTENT and report WAITING.
type stepHandler func(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error)

func (e *Engine) handler(a execution.StepAction) stepHandler {
	switch a {
	case execution.ActionControl:
		return e.handleControl
	case execution.ActionCommand:
		return e.handleCommand
	case execution.ActionImplementCheck:
		return e.handleImplementCheck
	case execution.ActionTestCheck:
		return e.handleTestCheck
	case execution.ActionAcceptanceCheck:
		return e.handleAcceptanceCheck
	case execution.ActionPlanDispatch:
		return e.handlePlanDispatch
	case execution.ActionPlanValidate:
		return e.handlePlanValidate
	case execution.ActionSelectUnit:
		return e.handleSelectUnit
	case execution.ActionDispatch:
		return e.handleDispatch
	case execution.ActionEvaluateResult:
		return e.handleEvaluate
	case execution.ActionContinuation:
		return e.handleContinuation
	case execution.ActionRunUnitValidation:
		return e.handleUnitValidation
	case execution.ActionUnitGates:
		return e.handleUnitGates
	case execution.ActionMoreWork:
		return e.handleMoreWork
	case execution.ActionPhaseValidation:
		return e.handlePhaseValidation
	case execution.ActionMorePhases:
		return e.handleMorePhases
	case execution.ActionFinalHardening:
		return e.handleFinalHardening
	case execution.ActionHardeningFix:
		return e.handleHardeningFix
	case execution.ActionFinalValidation:
		return e.handleFinalValidation
	case execution.ActionFinalCheckpoint:
		return e.handleFinalCheckpoint
	case execution.ActionComplete:
		return e.handleComplete
	}
	return nil
}

// statusOutcome maps a non-schedulable status to its step outcome.
func statusOutcome(s execution.ExecutionStatus) execution.StepOutcome {
	switch s {
	case execution.ExecutionPaused:
		return execution.StepPaused
	case execution.ExecutionRecovery:
		return execution.StepRecoveryRequired
	case execution.ExecutionFailed:
		return execution.StepExecutionFailed
	case execution.ExecutionCancelled:
		return execution.StepCancelled
	case execution.ExecutionCompleted:
		return execution.StepCompleted
	}
	return execution.StepNoop
}

// Step performs at most one deterministic orchestration step (WBS 16.1).
// It takes the driver lease for the duration of the step.
func (e *Engine) Step(ctx context.Context, executionID uuid.UUID) (execution.StepResult, error) {
	if e.draining.Load() {
		return execution.StepResult{}, ErrShuttingDown
	}
	m := e.lock(executionID)
	m.Lock()
	defer m.Unlock()
	rec, err := e.d.Execs.GetByID(ctx, nil, executionID)
	if err != nil {
		return execution.StepResult{}, translate(err)
	}
	if !rec.Status.IsSchedulable() {
		return execution.StepResult{Outcome: statusOutcome(rec.Status), Reason: string(rec.Status)}, nil
	}
	d, err := e.acquire(ctx, executionID, e.recordLeaseTTL(ctx, rec))
	if err != nil {
		return execution.StepResult{}, err
	}
	defer e.release(ctx, d)
	return e.safeStep(ctx, d)
}

// Run invokes Step until the execution waits, pauses, requires recovery,
// fails, is cancelled or completes, or the budget is used (WBS 16.1).
// Run never acts by itself: every iteration is one Step.
func (e *Engine) Run(ctx context.Context, executionID uuid.UUID, budget int) ([]execution.StepResult, error) {
	m := e.lock(executionID)
	m.Lock()
	defer m.Unlock()
	rec, err := e.d.Execs.GetByID(ctx, nil, executionID)
	if err != nil {
		return nil, translate(err)
	}
	if !rec.Status.IsSchedulable() {
		return []execution.StepResult{{Outcome: statusOutcome(rec.Status), Reason: string(rec.Status)}}, nil
	}
	ttl := e.recordLeaseTTL(ctx, rec)
	d, err := e.acquire(ctx, executionID, ttl)
	if err != nil {
		return nil, err
	}
	defer e.release(ctx, d)
	var out []execution.StepResult
	for i := 0; budget <= 0 || i < budget; i++ {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if e.draining.Load() {
			// Shutting down: the current step finished; none starts.
			return out, nil
		}
		res, err := e.safeStep(ctx, d)
		out = append(out, res)
		if err != nil {
			return out, err
		}
		if res.Outcome.StopsRun() {
			return out, nil
		}
		if _, err := e.d.Repo.RenewDriver(ctx, executionID, e.d.Owner, d.epoch, e.d.Now(), ttl); err != nil {
			return out, err
		}
	}
	return out, nil
}

// recordLeaseTTL is the execution's captured driver lease (engine
// settings in the configuration snapshot; default when absent).
func (e *Engine) recordLeaseTTL(ctx context.Context, rec *persistence.ExecutionRecord) time.Duration {
	if snap, err := e.d.Execs.GetSnapshot(ctx, rec.ID); err == nil && snap != nil {
		if c, err := execution.DecodeContent(snap.Canonical); err == nil {
			if d := c.EngineSettingsOrDefault().DriverLease; d > 0 {
				return d
			}
		}
	}
	return execution.DefaultEngineSettings().DriverLease
}

// safeStep is step with panic containment (WBS 18.196 / 18.197): a panic
// is logged with its stack and the execution — only this one — enters
// PANIC recovery. The step stays INTENT and is never retried blindly: it
// may have performed an external mutation, so an operator reconciles it.
func (e *Engine) safeStep(ctx context.Context, d *driver) (res execution.StepResult, err error) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		e.d.Logger.ErrorContext(ctx, "engine step panicked", "execution_id", d.id.String(), "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
		st, lerr := e.load(context.WithoutCancel(ctx), d.id)
		if lerr != nil {
			res, err = execution.StepResult{}, fmt.Errorf("engine step panicked: %v (state not loaded: %w)", v, lerr)
			return
		}
		st.drv = d
		var step *persistence.StepRecord
		if st.last != nil && st.last.Status == execution.StepIntent {
			step = st.last
		}
		res, err = e.requireRecovery(context.WithoutCancel(ctx), st, step, fmt.Sprintf("internal error (panic) during the step: %v", v),
			map[string]any{"condition": string(recovery.Panic), "panic": fmt.Sprint(v)})
	}()
	return e.step(ctx, d)
}

// step is one Step under a held lease.
func (e *Engine) step(ctx context.Context, d *driver) (execution.StepResult, error) {
	ctx = logging.WithFields(ctx, "execution_id", d.id.String())
	st, err := e.load(ctx, d.id)
	if err != nil {
		return execution.StepResult{}, translate(err)
	}
	st.drv = d
	ctx = logging.WithFields(ctx, "project_id", st.rec.ProjectID.String())
	if !st.rec.Status.IsSchedulable() {
		return execution.StepResult{Outcome: statusOutcome(st.rec.Status), Reason: string(st.rec.Status)}, nil
	}
	if st.project.Status != project.StatusRUNNING {
		return execution.StepResult{Outcome: execution.StepNoop, Reason: "project is " + string(st.project.Status)}, nil
	}
	// A requested requeue applies at a step boundary (never while a step
	// is pending): the running step finished before this one.
	if st.last == nil || st.last.Status != execution.StepIntent {
		if pending, err := e.pendingRequeue(ctx, st); err != nil {
			return execution.StepResult{}, translate(err)
		} else if pending {
			from, key := st.cur.Key, wbs.UnitKey(st.unit)
			if err := e.applyRequeue(ctx, st); err != nil {
				return execution.StepResult{}, err
			}
			return execution.StepResult{Outcome: execution.StepProgressed, State: from, Reason: "task " + key + " requeued"}, nil
		}
	}
	action := execution.ResolveAction(st.cur)
	if st.curRec != nil && execution.IsCommandTemplate(st.curRec.PromptTemplate) {
		action = execution.ActionCommand
	}
	var step *persistence.StepRecord
	if st.last != nil && st.last.Status == execution.StepIntent {
		// Resume the interrupted / waiting step (WBS 16.77, 16.83) under
		// its own correlation id unless an operator request drives it.
		step = st.last
		action = step.Action
		if c, _ := step.Detail["correlation_id"].(string); c != "" && logging.CorrelationID(ctx) == "" {
			ctx = logging.WithCorrelationID(ctx, c)
		}
		ctx, _ = logging.EnsureCorrelationID(ctx)
	} else {
		attempt := 1
		if st.last != nil && st.last.RuntimeVersion == st.runtime.Version && st.last.Action == action {
			attempt = st.last.Attempt + 1
		}
		unitAction := action == execution.ActionDispatch || action == execution.ActionRunUnitValidation || action == execution.ActionUnitGates
		if v := e.invariants(ctx, st, unitAction); len(v) > 0 {
			return e.requireRecoveryNoStep(ctx, st, "execution invariant violated", map[string]any{"violations": v})
		}
		// Correlation (WBS 18.112): the operator's request id, else one
		// generated for this step; stored with the step.
		var cid string
		ctx, cid = logging.EnsureCorrelationID(ctx)
		step = &persistence.StepRecord{ExecutionID: st.rec.ID, ProjectID: st.rec.ProjectID, Action: action, StateKey: st.cur.Key,
			RuntimeVersion: st.runtime.Version, Attempt: attempt, IdempotencyKey: execution.IdempotencyKey(st.rec.ID, int64(st.runtime.Version), action, attempt),
			PhaseID: st.runtime.PhaseID, WorkUnitID: st.runtime.WorkUnitID, Detail: map[string]any{"correlation_id": cid}}
		var created bool
		err := e.inTx(ctx, nil, func(tx persistence.Transaction) error {
			if err := e.fence(ctx, tx, st); err != nil {
				return err
			}
			var err error
			step, created, err = e.d.Repo.AppendStepIntent(ctx, tx, step)
			if err != nil || !created {
				return err
			}
			return e.event(ctx, tx, st, execution.EventStepIntent, map[string]any{"seq": step.Seq, "action": string(action), "state": st.cur.Key})
		})
		if err != nil {
			return execution.StepResult{}, translate(err)
		}
		if !created && step.Status != execution.StepIntent {
			// Already applied (WBS 16.83): nothing to repeat.
			return execution.StepResult{Outcome: execution.StepNoop, Action: action, State: st.cur.Key, StepSeq: step.Seq, Reason: "step already applied"}, nil
		}
		if err := e.hook(ctx, action, "intent"); err != nil {
			return execution.StepResult{}, err
		}
	}
	ctx = logging.WithFields(ctx, "step", step.Seq, "action", string(action))
	h := e.handler(action)
	if h == nil {
		return e.requireRecovery(ctx, st, step, "no engine handler for action "+string(action), nil)
	}
	res, err := h(ctx, st, step)
	if res.Action == "" {
		res.Action = action
	}
	if res.StepSeq == 0 {
		res.StepSeq = step.Seq
	}
	if res.State == "" {
		res.State = st.cur.Key
	}
	if err != nil && (errors.Is(err, ErrConflict) || errors.Is(err, persistence.ErrConflict)) {
		// A concurrent pause / cancel / recovery changed the execution
		// mid-step: nothing was applied; the step stays INTENT and resumes
		// after the execution is schedulable again.
		if rec, gerr := e.d.Execs.GetByID(ctx, nil, st.rec.ID); gerr == nil && !rec.Status.IsSchedulable() {
			return execution.StepResult{Outcome: statusOutcome(rec.Status), Action: action, State: st.cur.Key, StepSeq: step.Seq, Reason: string(rec.Status)}, nil
		}
	}
	var guard *repositoryGuardError
	if errors.As(err, &guard) {
		// The repository is missing, replaced, switched or unwritable: stop
		// before the side effect (WBS 18.48 / 18.49 / 18.53).
		return e.requireRecovery(ctx, st, step, guard.reason, guard.evidence)
	}
	var held *gateWaitError
	if errors.As(err, &held) {
		return e.wait(ctx, st, step, held.reason, persistence.StepRefs{})
	}
	if errors.Is(err, ErrValidationCapacity) {
		return e.wait(ctx, st, step, "validation capacity reached: queued until a validation slot frees", persistence.StepRefs{})
	}
	if errors.Is(err, gitcheckpoint.ErrLocked) || errors.Is(err, gitcheckpoint.ErrTimeout) {
		// The repository is busy (index.lock held past the bounded retry)
		// or git hung and was killed: wait — the step stays INTENT and is
		// retried by the scheduler; nothing is marked failed (WBS 18.57 /
		// 18.60).
		return e.wait(ctx, st, step, "repository busy: "+err.Error()+"; retried later (remove a stale .git/index.lock if no git process is running)",
			persistence.StepRefs{})
	}
	if resilience.IsDiskFull(err) {
		// No space left (WBS 18.54): an operator must free disk; nothing
		// is retried, and no step result is recorded as success.
		return e.requireRecovery(ctx, st, step, "disk full: "+string(action)+" could not complete (no space left on device)",
			map[string]any{"condition": string(recovery.DiskFull), "error": err.Error()})
	}
	if err != nil {
		e.d.Logger.WarnContext(ctx, "execution step failed", "execution_id", st.rec.ID.String(), "action", string(action), "error", err.Error())
	}
	return res, err
}

// invariants evaluates the WBS 16.84 invariants over persisted state.
func (e *Engine) invariants(ctx context.Context, st *engineState, unitAction bool) []string {
	open := map[uuid.UUID]int{}
	if st.unit != nil && e.d.Dispatches != nil {
		uid := st.unit.ID
		ds, _, err := e.d.Dispatches.List(ctx, persistence.DispatchFilter{ProjectID: st.rec.ProjectID, WorkUnitID: &uid, Limit: 50})
		if err == nil {
			for _, d := range ds {
				switch d.Status {
				case worker.DispatchPending, worker.DispatchSending, worker.DispatchWaiting:
					open[uid]++
				}
			}
		}
	}
	return execution.CheckInvariants(execution.InvariantFacts{PositionPhase: st.posPhase, CurrentPhase: st.current, Plan: st.plan,
		CurrentWBSVersion: st.wbsVer, PositionUnit: st.unit, PlanUnits: st.units, OpenDispatches: open, UnitAction: unitAction})
}

// ---------------------------------------------------------------------------
// Transition application (P16-84).

// advanceOpts customizes one applied transition.
type advanceOpts struct {
	SetPhase bool
	PhaseID  *uuid.UUID
	SetUnit  bool
	UnitID   *uuid.UUID
	Refs     persistence.StepRefs
	Outcome  string
	// Status changes the execution status (e.g. WAITING → RUNNING is
	// implicit; COMPLETED at the end).
	Status execution.ExecutionStatus
	// Events are appended in the same transaction.
	Events []pendingEvent
	// Tx runs extra writes inside the transaction (unit status, plan
	// activation, …) before the transition.
	Tx func(tx persistence.Transaction) error
}

type pendingEvent struct {
	Type    execution.EventType
	Payload map[string]any
}

func (e *Engine) event(ctx context.Context, ex persistence.Executor, st *engineState, typ execution.EventType, payload map[string]any) error {
	id := st.rec.ID
	if cid := logging.CorrelationID(ctx); cid != "" {
		if _, set := payload["correlation_id"]; !set {
			cp := make(map[string]any, len(payload)+1)
			for k, v := range payload {
				cp[k] = v
			}
			cp["correlation_id"] = cid
			payload = cp
		}
	}
	return e.d.Execs.AppendEvent(ctx, ex, &execution.Event{ProjectID: st.rec.ProjectID, ExecutionID: &id, Type: typ, Payload: payload, CreatedAt: e.d.Now().UTC()})
}

// forwardPath resolves `token` from the current state, following at most
// two intermediate control states reached by their single EVALUATE /
// PROCEED edge (e.g. VALIDATE → VALIDATION_RESULT → PASS).
func forwardPath(m *state.StateMachineSnapshot, from string, kinds map[string]string, token string) ([]state.TransitionSnapshot, error) {
	var path []state.TransitionSnapshot
	cur := from
	for depth := 0; depth < 3; depth++ {
		d, err := execution.RouteToken(m, cur, execution.ResultToken(token))
		if err != nil {
			return nil, err
		}
		if d.Transition != nil {
			return append(path, *d.Transition), nil
		}
		var next *state.TransitionSnapshot
		for _, tok := range []string{"EVALUATE", "PROCEED"} {
			if t, _ := execution.RouteToken(m, cur, execution.ResultToken(tok)); t.Transition != nil && kinds[t.Transition.To] == string(state.KindControl) {
				tt := *t.Transition
				next = &tt
				break
			}
		}
		if next == nil {
			return nil, nil
		}
		path = append(path, *next)
		cur = next.To
	}
	return nil, nil
}

func (st *engineState) kinds() map[string]string {
	out := map[string]string{}
	for _, s := range st.machine.States {
		out[s.Key] = s.Kind
	}
	return out
}

// advance applies `token` from the current state (with forwardPath) and
// finishes the step APPLIED — one transaction (WBS 16.8): fence, extra
// writes, runtime CAS + history, execution position CAS, events. A
// missing edge is a recovery condition.
func (e *Engine) advance(ctx context.Context, st *engineState, step *persistence.StepRecord, token string, o advanceOpts) (execution.StepResult, error) {
	path, err := forwardPath(st.machine, st.cur.Key, st.kinds(), token)
	if err != nil {
		return e.requireRecovery(ctx, st, step, err.Error(), nil)
	}
	if len(path) == 0 {
		return e.requireRecovery(ctx, st, step, fmt.Sprintf("workflow has no %s transition from %s", token, st.cur.Key),
			map[string]any{"token": token, "state": st.cur.Key})
	}
	final := path[len(path)-1].To
	// Fault points (WBS 18.141): around the durable transition.
	if err := faults.Hit(ctx, faults.BeforeTransition); err != nil {
		return execution.StepResult{}, err
	}
	err = e.inTx(ctx, st, func(tx persistence.Transaction) error {
		if err := e.fence(ctx, tx, st); err != nil {
			return err
		}
		if o.Tx != nil {
			if err := o.Tx(tx); err != nil {
				return err
			}
		}
		rt := *st.runtime
		phaseID, unitID := rt.PhaseID, rt.WorkUnitID
		if o.SetPhase {
			phaseID = o.PhaseID
		}
		if o.SetUnit {
			unitID = o.UnitID
		}
		now := e.d.Now().UTC()
		repo := e.d.Runtime.WithTx(tx)
		for _, t := range path {
			to := st.byKey[t.To]
			if to == nil {
				return fmt.Errorf("%w: state %s missing from the bound machine", ErrInvalidInput, t.To)
			}
			next := rt
			next.CurrentStateID, next.EnteredAt, next.PhaseID, next.WorkUnitID = to.ID, now, phaseID, unitID
			if err := repo.UpdateWithVersion(ctx, &next, rt.Version); err != nil {
				return err
			}
			next.Version = rt.Version + 1
			execID := st.rec.ID
			if err := e.d.Execs.AppendHistory(ctx, tx, &persistence.TransitionHistoryRecord{ProjectID: rt.ProjectID, StateMachineID: rt.StateMachineID,
				StateMachineVersion: rt.StateMachineVersion, FromStateID: st.byKey[t.From].ID, ToStateID: to.ID, Outcome: t.Outcome,
				PhaseID: phaseID, WorkUnitID: unitID, RuntimeVersion: next.Version, ExecutionID: &execID, CreatedAt: now}); err != nil {
				return err
			}
			rt = next
		}
		upd := persistence.PositionUpdate{SetPhase: true, PhaseID: phaseID, SetUnit: true, UnitID: unitID, RuntimeStateID: &rt.ID,
			SetWait: true, Status: o.Status}
		if upd.Status == "" && st.rec.Status == execution.ExecutionWaiting {
			upd.Status = execution.ExecutionRunning
		}
		rec, err := e.d.Repo.UpdatePosition(ctx, tx, st.rec.ID, st.rec.Version, upd)
		if err != nil {
			return err
		}
		outcome := o.Outcome
		if outcome == "" {
			outcome = token
		}
		if err := e.d.Repo.FinishStep(ctx, tx, step.ID, execution.StepApplied, outcome, "", o.Refs); err != nil {
			return err
		}
		for _, ev := range o.Events {
			if err := e.event(ctx, tx, st, ev.Type, ev.Payload); err != nil {
				return err
			}
		}
		if err := e.event(ctx, tx, st, execution.EventStepApplied, map[string]any{"seq": step.Seq, "action": string(step.Action),
			"from": st.cur.Key, "to": final, "outcome": outcome}); err != nil {
			return err
		}
		st.rec, st.runtime = rec, &rt
		return nil
	})
	if err != nil {
		return execution.StepResult{}, translate(err)
	}
	if err := faults.Hit(ctx, faults.AfterTransition); err != nil {
		return execution.StepResult{}, err
	}
	out := execution.StepResult{Outcome: execution.StepProgressed, Action: step.Action, State: final, StepSeq: step.Seq, Reason: token}
	if o.Status == execution.ExecutionCompleted {
		out.Outcome = execution.StepCompleted
	}
	return out, nil
}

// finishNoTransition records a step result that does not move the runtime
// (e.g. a hardening operation inside FINAL_HARDENING).
func (e *Engine) finishNoTransition(ctx context.Context, st *engineState, step *persistence.StepRecord, outcome string, refs persistence.StepRefs, events ...pendingEvent) (execution.StepResult, error) {
	err := e.inTx(ctx, st, func(tx persistence.Transaction) error {
		if err := e.fence(ctx, tx, st); err != nil {
			return err
		}
		if st.rec.Status == execution.ExecutionWaiting {
			rec, err := e.d.Repo.UpdatePosition(ctx, tx, st.rec.ID, st.rec.Version, persistence.PositionUpdate{Status: execution.ExecutionRunning, SetWait: true})
			if err != nil {
				return err
			}
			st.rec = rec
		}
		if err := e.d.Repo.FinishStep(ctx, tx, step.ID, execution.StepApplied, outcome, "", refs); err != nil {
			return err
		}
		for _, ev := range events {
			if err := e.event(ctx, tx, st, ev.Type, ev.Payload); err != nil {
				return err
			}
		}
		return e.event(ctx, tx, st, execution.EventStepApplied, map[string]any{"seq": step.Seq, "action": string(step.Action),
			"from": st.cur.Key, "to": st.cur.Key, "outcome": outcome})
	})
	if err != nil {
		return execution.StepResult{}, translate(err)
	}
	return execution.StepResult{Outcome: execution.StepProgressed, Action: step.Action, State: st.cur.Key, StepSeq: step.Seq, Reason: outcome}, nil
}

// lastDispatch is the execution's most recent worker dispatch: the step's
// own, else the latest step that recorded one (nil when none).
func (e *Engine) lastDispatch(ctx context.Context, st *engineState, step *persistence.StepRecord) *uuid.UUID {
	if step != nil && step.DispatchID != nil {
		return step.DispatchID
	}
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return nil
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].DispatchID != nil {
			return steps[i].DispatchID
		}
	}
	return nil
}

// requireRecovery stops autonomous progression with evidence (WBS 16.76,
// P16-93): the execution becomes RECOVERY_REQUIRED, the step FAILED.
func (e *Engine) requireRecovery(ctx context.Context, st *engineState, step *persistence.StepRecord, reason string, evidence map[string]any) (execution.StepResult, error) {
	if evidence == nil {
		evidence = map[string]any{}
	}
	if c, _ := evidence["condition"].(string); c == "" {
		// Every recovery records its classified condition (WBS 18.229):
		// plans, metrics and alerts read it.
		evidence["condition"] = string(recovery.ConditionForReason(reason, evidence))
	}
	evidence["state"] = st.cur.Key
	if step != nil {
		evidence["action"] = string(step.Action)
		evidence["step_seq"] = step.Seq
	}
	if _, ok := evidence["dispatch_id"]; !ok {
		// The latest worker dispatch (and so the model's reply) is evidence
		// of every recovery; dispatch_id stays reserved for the dispatch a
		// recovery is about (delivery resolution).
		if id := e.lastDispatch(ctx, st, step); id != nil {
			evidence["last_dispatch_id"] = id.String()
		}
	}
	err := e.inTx(ctx, st, func(tx persistence.Transaction) error {
		if err := e.fence(ctx, tx, st); err != nil {
			return err
		}
		rec, err := e.d.Repo.UpdatePosition(ctx, tx, st.rec.ID, st.rec.Version, persistence.PositionUpdate{Status: execution.ExecutionRecovery,
			Reason: reason, SetRecovery: true, RecoveryReason: reason, RecoveryEvidence: evidence, SetWait: true})
		if err != nil {
			return err
		}
		st.rec = rec
		if step != nil && step.Status == execution.StepIntent {
			if err := e.d.Repo.FinishStep(ctx, tx, step.ID, execution.StepFailed, "RECOVERY_REQUIRED", reason, persistence.StepRefs{}); err != nil {
				return err
			}
		}
		return e.event(ctx, tx, st, execution.EventRecoveryRequired, map[string]any{"reason": reason, "evidence": evidence})
	})
	if err != nil {
		return execution.StepResult{}, translate(err)
	}
	e.d.Logger.WarnContext(ctx, "execution requires recovery", "execution_id", st.rec.ID.String(), "state", st.cur.Key, "reason", reason)
	res := execution.StepResult{Outcome: execution.StepRecoveryRequired, State: st.cur.Key, Reason: reason}
	if step != nil {
		res.Action, res.StepSeq = step.Action, step.Seq
	}
	return res, nil
}

func (e *Engine) requireRecoveryNoStep(ctx context.Context, st *engineState, reason string, evidence map[string]any) (execution.StepResult, error) {
	return e.requireRecovery(ctx, st, nil, reason, evidence)
}

// wait parks the execution in WAITING with the step still INTENT
// (WBS 16.29): the scheduler re-checks the condition, never re-sends.
func (e *Engine) wait(ctx context.Context, st *engineState, step *persistence.StepRecord, reason string, refs persistence.StepRefs) (execution.StepResult, error) {
	err := e.inTx(ctx, st, func(tx persistence.Transaction) error {
		if err := e.fence(ctx, tx, st); err != nil {
			return err
		}
		if !refsEmpty(refs) {
			if err := e.d.Repo.SetStepRefs(ctx, tx, step.ID, refs); err != nil {
				return err
			}
		}
		if st.rec.Status == execution.ExecutionWaiting && st.rec.WaitReason == reason {
			return nil
		}
		rec, err := e.d.Repo.UpdatePosition(ctx, tx, st.rec.ID, st.rec.Version, persistence.PositionUpdate{Status: execution.ExecutionWaiting,
			SetWait: true, WaitReason: reason, Reason: reason})
		if err != nil {
			return err
		}
		st.rec = rec
		return e.event(ctx, tx, st, execution.EventExecutionWaiting, map[string]any{"reason": reason, "step_seq": step.Seq})
	})
	if err != nil {
		return execution.StepResult{}, translate(err)
	}
	return execution.StepResult{Outcome: execution.StepWaiting, Action: step.Action, State: st.cur.Key, StepSeq: step.Seq, Reason: reason}, nil
}

// ---------------------------------------------------------------------------
// CONTROL handler (P16-86).

// controlFacts derives the routing facts over persisted state (WBS
// 16.9–16.12): the working phase, its plan state against the current WBS
// version, the next unit.
func (e *Engine) controlFacts(ctx context.Context, st *engineState) execution.RoutingFacts {
	work := st.workPhase()
	in := execution.RoutingInput{CurrentWBSVersion: st.wbsVer, Plans: st.plans, Units: st.units}
	if work != nil {
		// Restrict routing to the working phase: only it and DONE phases
		// are presented, so an appended phase never becomes current early.
		for _, p := range st.phases {
			if p.ID == work.ID || p.Status == phase.StatusDONE {
				in.Phases = append(in.Phases, p)
			}
		}
	}
	if st.plan != nil {
		if el, err := e.d.Units.ListEligible(ctx, st.plan.ID); err == nil {
			in.EligibleUnits = el
		}
	}
	return execution.ResolveRouting(in)
}

// handleControl resolves a control state's outcome from authoritative
// facts: a validation verdict (PASS / FAIL) when the state routes on one,
// then the routing preference, then EVALUATE / PROCEED.
func (e *Engine) handleControl(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if st.cur.Key == "REPLAN_CHECK" && st.statefileWorkflow() {
		return e.handleReplanCheck(ctx, st, step)
	}
	if st.cur.Key == "ROUTE" && st.statefileWorkflow() {
		// The output-format skill command must have reached Hermes; a
		// failed send is resent (bounded) before any work is routed.
		if stop, err := e.checkSkillCommand(ctx, st, step); stop != nil || err != nil {
			if err != nil {
				return execution.StepResult{}, err
			}
			return *stop, nil
		}
	}
	var prefs []string
	if st.unit != nil {
		if run, err := e.d.Repo.LatestValidationRun(ctx, nil, st.rec.ID, execution.ScopeUnit, st.unit.ID); err == nil {
			switch run.Status {
			case string(execution.ValidationPass):
				prefs = append(prefs, "PASS")
			case string(execution.ValidationFail):
				prefs = append(prefs, "FAIL")
			}
		}
	}
	facts := e.controlFacts(ctx, st)
	for _, p := range facts.OutcomePreference() {
		if p == execution.OutcomeProceed {
			prefs = append(prefs, "EVALUATE")
		}
		prefs = append(prefs, p)
	}
	for _, tok := range prefs {
		d, err := execution.RouteToken(st.machine, st.cur.Key, execution.ResultToken(tok))
		if err != nil {
			return e.requireRecovery(ctx, st, step, err.Error(), nil)
		}
		if d.Transition != nil {
			return e.advance(ctx, st, step, tok, advanceOpts{})
		}
	}
	return e.requireRecovery(ctx, st, step, "control state "+st.cur.Key+" has no transition for "+strings.Join(prefs, ","), nil)
}

// handleComplete: the runtime reached a terminal state. Completion is only
// ever applied by the FINAL_CHECKPOINT handler; reaching a terminal state
// any other way is a recovery condition.
func (e *Engine) handleComplete(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if c, err := e.d.Repo.GetCompletion(ctx, st.rec.ID); err == nil && c != nil {
		return execution.StepResult{Outcome: execution.StepCompleted}, nil
	}
	return e.requireRecovery(ctx, st, step, "terminal state reached without completion gates", nil)
}

func refsEmpty(r persistence.StepRefs) bool {
	return r.DispatchID == nil && r.ContextSnapshotID == nil && r.WorkerPromptID == nil && r.ValidationRunID == nil &&
		r.CheckpointID == nil && r.PlanID == nil && r.ContinuationID == nil && len(r.Detail) == 0
}
