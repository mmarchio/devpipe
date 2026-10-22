package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// Repeated actions. When the worker is about to be sent the same task
// command (IMPLEMENT, CONTINUE or TEST) after maxRepeatedCommands sends of
// it in a row on one task, the overseer stops the loop instead: it pauses
// the project, has the ARCHITECT analyze the task, fixes it when the
// analysis found optimizations, requeues the task and resumes. A task gets
// one automatic intervention; repeating again needs operator recovery.

// maxRepeatedCommands is how many sends in a row of one task command
// trigger the intervention.
const maxRepeatedCommands = 3

// reasonRepeatedCommand prefixes the recovery reason of a task that
// repeated again after its automatic intervention.
const reasonRepeatedCommand = "the worker repeated the same action"

// repeatedCommand stops the step before the command is sent again when the
// task's last sends were all this command; nil when it may be sent.
func (e *Engine) repeatedCommand(ctx context.Context, st *engineState, step *persistence.StepRecord) (*execution.StepResult, error) {
	n, err := e.consecutiveTaskCommands(ctx, st, step)
	if err != nil || n < maxRepeatedCommands {
		return nil, err
	}
	key, uid := wbs.UnitKey(st.unit), st.unit.ID.String()
	evidence := map[string]any{"work_unit_id": uid, "key": key, "state": st.cur.Key, "repeats": n}
	// The intervention's own requeue does not grant another one; only an
	// operator's recovery resolution does.
	resolved, err := e.lastEventAt(ctx, st, execution.EventRecoveryResolved, nil)
	if err != nil {
		return nil, err
	}
	prev, err := e.lastEventAt(ctx, st, execution.EventUnitRepeatDetected, &st.unit.ID)
	if err != nil {
		return nil, err
	}
	if prev.After(resolved) {
		r, err := e.requireRecovery(ctx, st, step, fmt.Sprintf("%s (%s) %d times in a row on task %s again after the automatic analysis and requeue",
			reasonRepeatedCommand, st.cur.Key, n, key), evidence)
		return &r, err
	}
	if e.d.Lifecycle == nil {
		r, err := e.requireRecovery(ctx, st, step, fmt.Sprintf("%s (%s) %d times in a row on task %s", reasonRepeatedCommand, st.cur.Key, n, key), evidence)
		return &r, err
	}
	if err := e.event(ctx, nil, st, execution.EventUnitRepeatDetected, evidence); err != nil {
		return nil, translate(err)
	}
	// The step stays INTENT: the requeue abandons it.
	if _, err := e.d.Lifecycle.Pause(ctx, st.rec.ProjectID.String()); err != nil {
		return nil, err
	}
	e.d.Logger.WarnContext(ctx, "worker repeated a task command; pausing to analyze and requeue the task", "execution_id", st.rec.ID.String(),
		"task", key, "state", st.cur.Key, "repeats", n)
	go e.breakRepeat(context.WithoutCancel(ctx), st.rec.ProjectID, st.rec.ID, key, st.cur.Key, n)
	return &execution.StepResult{Outcome: execution.StepPaused, State: st.cur.Key, Action: step.Action, StepSeq: step.Seq,
		Reason: fmt.Sprintf("task %s repeated %s %d times in a row", key, st.cur.Key, n)}, nil
}

// consecutiveTaskCommands counts the task's most recent command sends (since
// its last requeue and the last recovery resolution) that were the current
// state's command, back to the first different one.
func (e *Engine) consecutiveTaskCommands(ctx context.Context, st *engineState, step *persistence.StepRecord) (int, error) {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return 0, err
	}
	since, err := e.repeatResetAt(ctx, st)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		if s.ID == step.ID || s.Action != execution.ActionCommand || s.Status != execution.StepApplied {
			continue
		}
		if s.WorkUnitID == nil || *s.WorkUnitID != st.unit.ID || !s.IntentAt.After(since) || s.StateKey != st.cur.Key {
			break
		}
		n++
	}
	return n, nil
}

// repeatResetAt is when the task's repeat count restarts: its last requeue
// or the last recovery resolution, whichever is later.
func (e *Engine) repeatResetAt(ctx context.Context, st *engineState) (time.Time, error) {
	requeued, err := e.lastEventAt(ctx, st, execution.EventUnitRequeued, &st.unit.ID)
	if err != nil {
		return time.Time{}, err
	}
	resolved, err := e.lastEventAt(ctx, st, execution.EventRecoveryResolved, nil)
	if err != nil {
		return time.Time{}, err
	}
	if resolved.After(requeued) {
		return resolved, nil
	}
	return requeued, nil
}

// lastEventAt is when the execution's last event of typ (for the unit, when
// given) was recorded; zero when there is none.
func (e *Engine) lastEventAt(ctx context.Context, st *engineState, typ execution.EventType, unit *uuid.UUID) (time.Time, error) {
	ev, err := e.d.Repo.LastEvent(ctx, nil, st.rec.ID, typ, unit)
	if err != nil || ev == nil {
		return time.Time{}, err
	}
	return ev.At, nil
}

// breakRepeat runs the intervention of a paused execution whose worker
// repeated a task command: analyze, fix when optimizations were found,
// requeue and resume. A failed analysis or fix still requeues and resumes;
// an operator who resumed or cancelled meanwhile ends it.
func (e *Engine) breakRepeat(ctx context.Context, projectID, executionID uuid.UUID, key, state string, repeats int) {
	defer containPanic(e.d.Logger, "repeated action intervention", "execution_id", executionID.String())
	warn := func(what string, err error) {
		e.d.Logger.WarnContext(ctx, "repeated action intervention: "+what, "execution_id", executionID.String(), "task", key, "error", err.Error())
	}
	// The step that detected the repeat holds the execution's lock until
	// it returns.
	m := e.lock(executionID)
	m.Lock()
	m.Unlock()
	note := fmt.Sprintf("It paused because the worker was sent the %s command %d times in a row on this task without moving on. "+
		"Find why the task loops and what would let it progress.", state, repeats)
	if done, err := e.startTaskOp(ctx, projectID, executionID, TaskOpAnalyze, "overseer", note); err != nil {
		warn("analysis not started", err)
	} else {
		<-done
		if v, err := e.TaskOps(ctx, projectID, executionID); err != nil {
			warn("analysis not read", err)
		} else if v.FixEnabled {
			if done, err := e.startTaskOp(ctx, projectID, executionID, TaskOpFix, "overseer", ""); err != nil {
				warn("fix not started", err)
			} else {
				<-done
			}
		}
	}
	if rec, err := e.d.Execs.GetByID(ctx, nil, executionID); err != nil || rec.Status != execution.ExecutionPaused {
		// Resumed or cancelled meanwhile: the operator took over.
		return
	}
	reason := fmt.Sprintf("the worker repeated %s %d times in a row", state, repeats)
	if _, err := e.Requeue(ctx, projectID, executionID, "overseer", reason); err != nil {
		warn("requeue failed", err)
	}
	if _, err := e.d.Lifecycle.Resume(ctx, projectID.String()); err != nil {
		warn("resume failed", err)
		return
	}
	e.Kick()
}
