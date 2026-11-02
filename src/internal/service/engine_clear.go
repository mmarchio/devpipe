package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/wbs"
)

// ClearProcesses is the operator's remedy for a stuck execution: unlike
// Requeue, which waits for a worker command already sent, it interrupts
// whatever this process is running for the execution and restarts the
// current task at once.
//
//  1. A requeue of the current task is requested, marked as interrupting:
//     it applies even over the interrupted (still pending) step.
//  2. Pending actions are withdrawn: a requested pause after the task.
//  3. The running step is interrupted: its in-flight worker call is
//     abandoned (the dispatch becomes CANCELLED, never resent) and a
//     running validation command is killed. A running task analysis or
//     fix is cancelled the same way.
//  4. Once the step has returned, the requeue is applied: a new worker
//     session (the previous one is deleted at the provider, which ends a
//     run still going there), the task reset to READY and the workflow
//     back at ACCEPTANCE.
//
// Code changes already made in the repository are kept.

// ErrProcessesCleared is the cancellation cause of work interrupted by
// ClearProcesses.
var ErrProcessesCleared = errors.New("service: processes cleared by the operator")

// clearProcessesWait bounds how long ClearProcesses waits for the
// interrupted step to return before leaving the requeue to the next step.
const clearProcessesWait = 30 * time.Second

// ClearProcessesResult reports what ClearProcesses did.
type ClearProcessesResult struct {
	// Status is RequeueApplied (done) or RequeueRequested (the step did
	// not return in time, or another instance drives the execution: the
	// requeue applies at its next step).
	Status string `json:"status"`
	// Task is the requeued task's key.
	Task string `json:"task"`
	// InterruptedStep: a running step was interrupted.
	InterruptedStep bool `json:"interrupted_step"`
	// CancelledTaskOp: a running task analysis or fix was cancelled.
	CancelledTaskOp bool `json:"cancelled_task_op"`
	// CancelledPauseAfterTask: a requested pause after the task was
	// withdrawn.
	CancelledPauseAfterTask bool `json:"cancelled_pause_after_task"`
}

// runningStep is a step in progress (Engine.steps).
type runningStep struct {
	cancel context.CancelCauseFunc
}

// trackStep registers the step about to run for executionID so
// ClearProcesses can interrupt it; unregister when it returns.
func (e *Engine) trackStep(ctx context.Context, executionID uuid.UUID) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	rs := &runningStep{cancel: cancel}
	e.steps.Store(executionID, rs)
	return ctx, func() {
		e.steps.CompareAndDelete(executionID, rs)
		cancel(nil)
	}
}

// ClearProcesses interrupts the execution's running work and restarts its
// current task; see the comment above. by and reason are recorded.
func (e *Engine) ClearProcesses(ctx context.Context, projectID, executionID uuid.UUID, by, reason string) (*ClearProcessesResult, error) {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return nil, err
	}
	st, err := e.load(ctx, executionID)
	if err != nil {
		return nil, translate(err)
	}
	if err := requeueable(st); err != nil {
		return nil, err
	}
	key := wbs.UnitKey(st.unit)
	res := &ClearProcessesResult{Status: RequeueRequested, Task: key}
	if err := e.event(ctx, nil, st, execution.EventUnitRequeueRequested, map[string]any{"work_unit_id": st.unit.ID.String(),
		"key": key, "by": by, "reason": reason, "interrupt": true}); err != nil {
		return nil, translate(err)
	}
	if pending, err := e.PauseAfterTaskPending(ctx, executionID); err != nil {
		return nil, translate(err)
	} else if pending {
		if err := e.event(ctx, nil, st, execution.EventPauseAfterTaskCancelled, map[string]any{"by": by}); err != nil {
			return nil, translate(err)
		}
		res.CancelledPauseAfterTask = true
	}
	if v, ok := e.steps.Load(executionID); ok {
		v.(*runningStep).cancel(ErrProcessesCleared)
		res.InterruptedStep = true
	}
	if v, ok := e.taskOps.Load(executionID); ok {
		v.(*taskOpRun).cancel(ErrProcessesCleared)
		res.CancelledTaskOp = true
	}
	e.d.Logger.InfoContext(ctx, "execution processes cleared", "execution_id", executionID.String(), "task", key,
		"interrupted_step", res.InterruptedStep, "cancelled_task_op", res.CancelledTaskOp)

	// Apply the requeue once the interrupted step has returned, taking the
	// execution's lock and driver lease like a step would.
	m := e.lock(executionID)
	if !e.waitLock(ctx, m, clearProcessesWait) {
		e.Kick()
		return res, nil
	}
	defer m.Unlock()
	d, err := e.acquire(ctx, executionID, e.recordLeaseTTL(ctx, st.rec))
	if errors.Is(err, ErrExecutionBusy) {
		e.Kick()
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	defer e.release(ctx, d)
	if st, err = e.load(ctx, executionID); err != nil {
		return nil, translate(err)
	}
	st.drv = d
	req, err := e.pendingRequeue(ctx, st)
	if err != nil {
		return nil, translate(err)
	}
	if req != nil {
		if err := requeueable(st); err != nil {
			return nil, err
		}
		if err := e.applyRequeue(ctx, st); err != nil {
			return nil, err
		}
	}
	// Without a pending request the engine applied it already, in the
	// interrupted run's next step.
	res.Status = RequeueApplied
	e.Kick()
	return res, nil
}

// waitLock takes m, polling until it is free, ctx ends or wait elapses.
func (e *Engine) waitLock(ctx context.Context, m interface{ TryLock() bool }, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if m.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}
