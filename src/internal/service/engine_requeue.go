package service

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
	"overseer/internal/worker"
)

// Requeue restarts the current task of a statefile-workflow execution from
// the beginning (operator action): ACTIVE_TASK.md is cleared and the task
// is reset to TODO with its criteria unchecked in BACKLOG.md, the worker
// context is cleared (a new session), the task's attempt counters and
// manual approvals are reset, and the workflow returns to ACCEPTANCE so
// the project manager loads the task again.
//
// A worker command already sent cannot be interrupted, so a requeue is
// first recorded as a request: it is applied at once when the execution is
// idle, otherwise by the engine at the next step boundary (after the
// running step finishes).

// Requeue results.
const (
	RequeueApplied   = "REQUEUED"
	RequeueRequested = "REQUESTED"
)

// RequeueResult reports what a requeue request did.
type RequeueResult struct {
	// Status is RequeueApplied (done) or RequeueRequested (applies when
	// the running step finishes).
	Status string `json:"status"`
	// Task is the requeued task's key.
	Task string `json:"task"`
}

// ErrNotRequeueable is returned when the execution has no task that can
// be requeued (wrapped in ErrConflict).
var ErrNotRequeueable = fmt.Errorf("%w: no task can be requeued", ErrConflict)

// requeueResets are the attempt limits a requeue resets.
var requeueResets = []string{LimitFixAttempts, LimitContinuations, LimitStatefileResends, LimitCommandResends,
	LimitProtocolRepairs, LimitIdenticalFailures}

// Requeue requests that the execution's current task restart from the
// beginning; see the type comment above. by and reason are recorded.
func (e *Engine) Requeue(ctx context.Context, projectID, executionID uuid.UUID, by, reason string) (*RequeueResult, error) {
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
	if e.taskOpRunning(executionID) {
		return nil, ErrTaskOpRunning
	}
	key := wbs.UnitKey(st.unit)
	if err := e.event(ctx, nil, st, execution.EventUnitRequeueRequested, map[string]any{"work_unit_id": st.unit.ID.String(),
		"key": key, "by": by, "reason": reason}); err != nil {
		return nil, translate(err)
	}
	res := &RequeueResult{Status: RequeueRequested, Task: key}
	// Apply now when no step is running: take the execution's lock and
	// driver lease like a step would.
	m := e.lock(executionID)
	if !m.TryLock() {
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
	if err := requeueable(st); err != nil {
		return nil, err
	}
	if err := e.applyRequeue(ctx, st); err != nil {
		return nil, err
	}
	res.Status = RequeueApplied
	e.Kick()
	return res, nil
}

// requeueable checks that the execution is live, runs the statefile
// workflow and has a current task that is not DONE.
func requeueable(st *engineState) error {
	switch st.rec.Status {
	case execution.ExecutionRunning, execution.ExecutionWaiting, execution.ExecutionPaused, execution.ExecutionRecovery:
	default:
		return fmt.Errorf("%w: the execution is %s", ErrNotRequeueable, st.rec.Status)
	}
	if !st.statefileWorkflow() {
		return fmt.Errorf("%w: requeue applies to the statefile workflow", ErrNotRequeueable)
	}
	if st.unit == nil || st.unit.Status == wbs.WorkUnitStatusDONE {
		return fmt.Errorf("%w: the execution has no active task", ErrNotRequeueable)
	}
	return nil
}

// pendingRequeue reports whether a requeue of the current task was
// requested and not yet applied.
func (e *Engine) pendingRequeue(ctx context.Context, st *engineState) (bool, error) {
	if st.unit == nil || st.unit.Status == wbs.WorkUnitStatusDONE || !st.statefileWorkflow() {
		return false, nil
	}
	req, err := e.d.Repo.LastEvent(ctx, nil, st.rec.ID, execution.EventUnitRequeueRequested, &st.unit.ID)
	if err != nil || req == nil {
		return false, err
	}
	done, err := e.d.Repo.LastEvent(ctx, nil, st.rec.ID, execution.EventUnitRequeued, &st.unit.ID)
	if err != nil {
		return false, err
	}
	return done == nil || done.Seq < req.Seq, nil
}

// applyRequeue restarts the current task (the caller holds the driver
// lease): statefiles first (idempotent), then a new worker session, then
// one transaction that resets the unit and moves the workflow to
// ACCEPTANCE.
func (e *Engine) applyRequeue(ctx context.Context, st *engineState) error {
	u := st.unit
	key := wbs.UnitKey(u)
	acceptance, from := st.byKey["ACCEPTANCE"], st.byKey[st.cur.Key]
	if acceptance == nil || from == nil {
		return fmt.Errorf("%w: the workflow has no ACCEPTANCE state or the current state is unknown", ErrNotRequeueable)
	}
	format, err := e.statefileFormat(ctx, st.rec.ProjectID)
	if err != nil {
		return err
	}
	cwd := st.cwd()
	if abs, err := statefile.SafeRelPath(cwd, wbs.BacklogFile); err == nil {
		if b, err := os.ReadFile(abs); err == nil {
			if reset, found := wbs.ResetTask(string(b), key); found {
				if err := writeStatefile(cwd, wbs.BacklogFile, reset); err != nil {
					return fmt.Errorf("reset %s in %s: %w", key, wbs.BacklogFile, err)
				}
			}
		}
	}
	if err := writeStatefile(cwd, wbs.ActiveTaskFile, emptyActiveTask(format)); err != nil {
		return fmt.Errorf("clear %s: %w", wbs.ActiveTaskFile, err)
	}
	var session *uuid.UUID
	if e.d.Worker != nil {
		sess, err := e.d.Worker.NewSession(ctx, st.rec.ProjectID)
		if err != nil {
			return fmt.Errorf("open a new worker session: %w", err)
		}
		session = &sess.ID
	}
	return e.inTx(ctx, st, func(tx persistence.Transaction) error {
		if err := e.fence(ctx, tx, st); err != nil {
			return err
		}
		// An interrupted step (a dispatch that never completed) is
		// abandoned: the task starts over and never resumes it.
		if st.last != nil && st.last.Status == execution.StepIntent {
			if err := e.d.Repo.FinishStep(ctx, tx, st.last.ID, execution.StepAbandoned, "REQUEUED", "", persistence.StepRefs{}); err != nil {
				return err
			}
		}
		err := e.d.Repo.SetUnitStatus(ctx, tx, u.ID, []wbs.WorkUnitStatus{wbs.WorkUnitStatusEXECUTING, wbs.WorkUnitStatusINCOMPLETE,
			wbs.WorkUnitStatusVALIDATING, wbs.WorkUnitStatusFAILED, wbs.WorkUnitStatusBLOCKED}, wbs.WorkUnitStatusREADY)
		if err != nil && !errors.Is(err, persistence.ErrConflict) {
			return err
		}
		if err := e.d.Repo.ClearUnitApprovals(ctx, tx, u.ID); err != nil {
			return err
		}
		rt := *st.runtime
		now := e.d.Now().UTC()
		next := rt
		next.CurrentStateID, next.EnteredAt, next.WorkUnitID = acceptance.ID, now, nil
		if err := e.d.Runtime.WithTx(tx).UpdateWithVersion(ctx, &next, rt.Version); err != nil {
			return err
		}
		next.Version = rt.Version + 1
		execID := st.rec.ID
		if err := e.d.Execs.AppendHistory(ctx, tx, &persistence.TransitionHistoryRecord{ProjectID: rt.ProjectID, StateMachineID: rt.StateMachineID,
			StateMachineVersion: rt.StateMachineVersion, FromStateID: from.ID, ToStateID: acceptance.ID, Outcome: "REQUEUE",
			PhaseID: rt.PhaseID, WorkUnitID: &u.ID, RuntimeVersion: next.Version, ExecutionID: &execID, CreatedAt: now}); err != nil {
			return err
		}
		upd := persistence.PositionUpdate{SetUnit: true, UnitID: nil, RuntimeStateID: &next.ID, SetWait: true, SessionID: session}
		switch st.rec.Status {
		case execution.ExecutionRecovery:
			upd.Status, upd.Reason, upd.SetRecovery = execution.ExecutionRunning, "task "+key+" requeued", true
		case execution.ExecutionWaiting:
			upd.Status, upd.Reason = execution.ExecutionRunning, "task "+key+" requeued"
		}
		rec, err := e.d.Repo.UpdatePosition(ctx, tx, st.rec.ID, st.rec.Version, upd)
		if err != nil {
			return err
		}
		payload := map[string]any{"work_unit_id": u.ID.String(), "key": key, "from": st.cur.Key, "resets": requeueResets}
		if session != nil {
			payload["session_id"] = session.String()
		}
		if err := e.event(ctx, tx, st, execution.EventUnitRequeued, payload); err != nil {
			return err
		}
		st.rec, st.runtime = rec, &next
		return nil
	})
}

// emptyActiveTask is ACTIVE_TASK.md with no task, in the project's
// statefile format.
func emptyActiveTask(format worker.ResponseFormat) string {
	if syntaxFor(format).kv {
		return "title: Active task\n"
	}
	return "# Active task\n\nNo active task.\n"
}

// requeueHint is appended to the acceptance command after a requeue, until
// a task is claimed again: the project manager loads the requeued task.
func (e *Engine) requeueHint(ctx context.Context, st *engineState) (string, error) {
	rq, err := e.d.Repo.LastEvent(ctx, nil, st.rec.ID, execution.EventUnitRequeued, nil)
	if err != nil || rq == nil {
		return "", err
	}
	claimed, err := e.d.Repo.LastEvent(ctx, nil, st.rec.ID, execution.EventUnitClaimed, nil)
	if err != nil {
		return "", err
	}
	if claimed != nil && claimed.Seq > rq.Seq {
		return "", nil
	}
	key, _ := rq.Payload["key"].(string)
	if key == "" {
		return "", nil
	}
	return fmt.Sprintf("\n\nTask %s was requeued by the operator and restarts from the beginning: %s is empty and the task is TODO in %s. "+
		"Load task %s into %s next (status ACTIVE, also in the backlog), with its acceptance criteria unchecked.",
		key, wbs.ActiveTaskFile, wbs.BacklogFile, key, wbs.ActiveTaskFile), nil
}
