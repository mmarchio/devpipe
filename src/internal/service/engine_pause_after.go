package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// Pause after the current task (statefile workflow). The operator asks
// for it while the execution runs; the request applies when the next task
// is loaded into ACTIVE_TASK.md and its first IMPLEMENT is about to be
// sent: the project pauses instead, and resuming starts work on that task.

// ErrPauseAfterTaskUnavailable: the execution cannot pause after its task
// (wrapped in ErrConflict).
var ErrPauseAfterTaskUnavailable = fmt.Errorf("%w: pause after task is not available", ErrConflict)

// RequestPauseAfterTask records the request (a no-op when one is pending).
func (e *Engine) RequestPauseAfterTask(ctx context.Context, projectID, executionID uuid.UUID, by string) error {
	return e.setPauseAfterTask(ctx, projectID, executionID, by, true)
}

// CancelPauseAfterTask withdraws a pending request (a no-op without one).
func (e *Engine) CancelPauseAfterTask(ctx context.Context, projectID, executionID uuid.UUID, by string) error {
	return e.setPauseAfterTask(ctx, projectID, executionID, by, false)
}

func (e *Engine) setPauseAfterTask(ctx context.Context, projectID, executionID uuid.UUID, by string, on bool) error {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return err
	}
	st, err := e.load(ctx, executionID)
	if err != nil {
		return translate(err)
	}
	if !st.statefileWorkflow() {
		return fmt.Errorf("%w: it applies to the statefile workflow", ErrPauseAfterTaskUnavailable)
	}
	switch st.rec.Status {
	case execution.ExecutionRunning, execution.ExecutionWaiting, execution.ExecutionPaused:
	default:
		return fmt.Errorf("%w: the execution is %s", ErrPauseAfterTaskUnavailable, st.rec.Status)
	}
	pending, err := e.PauseAfterTaskPending(ctx, executionID)
	if err != nil || pending == on {
		return err
	}
	typ := execution.EventPauseAfterTaskCancelled
	if on {
		typ = execution.EventPauseAfterTaskRequested
	}
	return translateNil(e.event(ctx, nil, st, typ, map[string]any{"by": by}))
}

// PauseAfterTaskPending reports whether a pause after the current task is
// requested and not yet applied or cancelled.
func (e *Engine) PauseAfterTaskPending(ctx context.Context, executionID uuid.UUID) (bool, error) {
	if e == nil {
		return false, nil
	}
	var seq [3]int64
	for i, typ := range []execution.EventType{execution.EventPauseAfterTaskRequested, execution.EventPauseAfterTaskCancelled,
		execution.EventPauseAfterTaskApplied} {
		ev, err := e.d.Repo.LastEvent(ctx, nil, executionID, typ, nil)
		if err != nil {
			return false, err
		}
		if ev != nil {
			seq[i] = ev.Seq
		}
	}
	return seq[0] > seq[1] && seq[0] > seq[2], nil
}

// pauseBeforeTask applies a pending request before the first IMPLEMENT of a
// newly loaded task: the project pauses with the step still INTENT, so
// resuming sends the command. nil when work may begin.
func (e *Engine) pauseBeforeTask(ctx context.Context, st *engineState, step *persistence.StepRecord) (*execution.StepResult, error) {
	if e.d.Lifecycle == nil {
		return nil, nil
	}
	pending, err := e.PauseAfterTaskPending(ctx, st.rec.ID)
	if err != nil || !pending {
		return nil, err
	}
	key := wbs.UnitKey(st.unit)
	if err := e.event(ctx, nil, st, execution.EventPauseAfterTaskApplied, map[string]any{"work_unit_id": st.unit.ID.String(), "key": key}); err != nil {
		return nil, translate(err)
	}
	if _, err := e.d.Lifecycle.Pause(ctx, st.rec.ProjectID.String()); err != nil {
		return nil, err
	}
	return &execution.StepResult{Outcome: execution.StepPaused, State: st.cur.Key, Action: step.Action, StepSeq: step.Seq,
		Reason: "paused before task " + key + " (pause after task)"}, nil
}
