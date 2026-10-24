package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// Task analysis and fix from the recovery screen and the project overview.
// While an execution is in RECOVERY_REQUIRED or PAUSED with a current task
// (statefile workflow), the operator can have the ARCHITECT analyze the
// task — its definition, the phase WBS and plan, its tests and validation,
// and the recovery reason — for optimizations that remove the cause of the
// recovery. The analysis is submitted through the MCP server (an ANALYSIS
// result); when it lists optimizations, the operator can have the
// ARCHITECT implement them (the task definition, its tests or the code). A changed task definition is
// adopted through the replan checks (same key, kind and dependencies);
// either way the task's attempt counters reset. Both run in the background
// (a worker command takes minutes) and record their outcome as events,
// which the recovery screen and the overview show. The execution stays in
// recovery (or paused) and does not resume while one runs: the operator
// resumes it.

// Task operations.
const (
	TaskOpAnalyze = "ANALYZE"
	TaskOpFix     = "FIX"
)

// ErrTaskOpUnavailable: the execution is not in recovery or paused with a task that
// can be analyzed or fixed, or the requested step is not possible now
// (wrapped in ErrConflict).
var ErrTaskOpUnavailable = fmt.Errorf("%w: task analysis is not available", ErrConflict)

// ErrTaskOpRunning: a task analysis or fix is in progress; recovery
// resolution and requeue wait for it (wrapped in ErrConflict).
var ErrTaskOpRunning = fmt.Errorf("%w: a task analysis or fix is running", ErrConflict)

// TaskOpsView is the recovery screen's task analysis / fix panel.
type TaskOpsView struct {
	// Available: the execution is in recovery or paused with a current
	// task of the statefile workflow.
	Available bool   `json:"available"`
	Task      string `json:"task,omitempty"`
	// Running is the operation in progress ("ANALYZE" / "FIX"), if any.
	Running string `json:"running,omitempty"`
	// Interrupted names an operation that started but never finished
	// (the overseer stopped meanwhile); it can be run again.
	Interrupted string            `json:"interrupted,omitempty"`
	Analysis    *TaskAnalysisView `json:"analysis,omitempty"`
	// FixEnabled: the latest analysis found optimizations that were not
	// implemented yet, and nothing is running.
	FixEnabled bool `json:"fix_enabled"`
	// Fix is the outcome of the fix of the latest analysis (nil before
	// one ran): the recovery screen's notification.
	Fix *TaskFixView `json:"fix,omitempty"`
}

// TaskAnalysisView is the latest analysis of the task.
type TaskAnalysisView struct {
	At            time.Time `json:"at"`
	Fixable       bool      `json:"fixable"`
	Findings      []string  `json:"findings,omitempty"`
	Optimizations []string  `json:"optimizations,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// TaskFixView is the outcome of the latest fix of the task.
type TaskFixView struct {
	At                time.Time `json:"at"`
	Success           bool      `json:"success"`
	Message           string    `json:"message"`
	DefinitionChanged bool      `json:"definition_changed"`
}

// TaskOps returns the task analysis / fix panel of an execution.
func (e *Engine) TaskOps(ctx context.Context, projectID, executionID uuid.UUID) (*TaskOpsView, error) {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return nil, err
	}
	st, err := e.load(ctx, executionID)
	if err != nil {
		return nil, translate(err)
	}
	v := &TaskOpsView{}
	if taskOpsAvailable(st) != nil {
		return v, nil
	}
	v.Available, v.Task = true, wbs.UnitKey(st.unit)
	if op, ok := e.taskOps.Load(executionID); ok {
		v.Running = op.(string)
	}
	unit := &st.unit.ID
	last := func(t execution.EventType) (*persistence.EventRef, error) {
		return e.d.Repo.LastEvent(ctx, nil, executionID, t, unit)
	}
	analyzed, err := last(execution.EventUnitAnalyzed)
	if err != nil {
		return nil, err
	}
	fixed, err := last(execution.EventUnitFixed)
	if err != nil {
		return nil, err
	}
	if v.Running == "" {
		for _, p := range []struct {
			op      string
			started execution.EventType
			done    *persistence.EventRef
		}{{TaskOpAnalyze, execution.EventUnitAnalysisStarted, analyzed}, {TaskOpFix, execution.EventUnitFixStarted, fixed}} {
			s, err := last(p.started)
			if err != nil {
				return nil, err
			}
			if s != nil && (p.done == nil || p.done.Seq < s.Seq) {
				v.Interrupted = p.op
			}
		}
	}
	if analyzed != nil {
		a := &TaskAnalysisView{At: analyzed.At}
		a.Fixable, _ = analyzed.Payload["fixable"].(bool)
		a.Findings = stringList(analyzed.Payload["findings"])
		a.Optimizations = stringList(analyzed.Payload["optimizations"])
		a.Error, _ = analyzed.Payload["error"].(string)
		v.Analysis = a
		v.FixEnabled = v.Running == "" && a.Fixable && (fixed == nil || fixed.Seq < analyzed.Seq)
	}
	if fixed != nil && (analyzed == nil || fixed.Seq > analyzed.Seq) {
		f := &TaskFixView{At: fixed.At}
		f.Success, _ = fixed.Payload["success"].(bool)
		f.Message, _ = fixed.Payload["message"].(string)
		f.DefinitionChanged, _ = fixed.Payload["definition_changed"].(bool)
		v.Fix = f
	}
	return v, nil
}

// AnalyzeTask starts the ARCHITECT's analysis of the current task (in the
// background); see the TaskOps comment above.
func (e *Engine) AnalyzeTask(ctx context.Context, projectID, executionID uuid.UUID, by string) error {
	_, err := e.startTaskOp(ctx, projectID, executionID, TaskOpAnalyze, by, "")
	return err
}

// FixTask starts the ARCHITECT's implementation of the optimizations the
// latest analysis found (in the background).
func (e *Engine) FixTask(ctx context.Context, projectID, executionID uuid.UUID, by string) error {
	_, err := e.startTaskOp(ctx, projectID, executionID, TaskOpFix, by, "")
	return err
}

// taskOpRunning reports whether an analysis or fix of the execution is in
// progress in this process.
func (e *Engine) taskOpRunning(executionID uuid.UUID) bool {
	_, ok := e.taskOps.Load(executionID)
	return ok
}

// ResumeBlocked is the lifecycle resume guard: a paused execution does not
// resume while the ARCHITECT analyzes or fixes its task.
func (e *Engine) ResumeBlocked(ctx context.Context, projectID uuid.UUID) error {
	rec, err := e.d.Execs.GetActive(ctx, nil, projectID)
	if err != nil {
		return nil
	}
	if e.taskOpRunning(rec.ID) {
		return ErrTaskOpRunning
	}
	return nil
}

func taskOpsAvailable(st *engineState) error {
	if st.rec.Status != execution.ExecutionRecovery && st.rec.Status != execution.ExecutionPaused {
		return fmt.Errorf("%w: the execution is not paused or in recovery", ErrTaskOpUnavailable)
	}
	if !st.statefileWorkflow() {
		return fmt.Errorf("%w: it applies to the statefile workflow", ErrTaskOpUnavailable)
	}
	if st.unit == nil || st.unit.Status == wbs.WorkUnitStatusDONE {
		return fmt.Errorf("%w: the execution has no current task", ErrTaskOpUnavailable)
	}
	return nil
}

// startTaskOp starts op in the background; the returned channel closes
// when it finished. note (optional) tells the ARCHITECT why the analysis
// was started.
func (e *Engine) startTaskOp(ctx context.Context, projectID, executionID uuid.UUID, op, by, note string) (<-chan struct{}, error) {
	if _, err := e.execFor(ctx, projectID, executionID); err != nil {
		return nil, err
	}
	if e.d.Worker == nil || e.d.CommandPrompts == nil {
		return nil, fmt.Errorf("%w: no worker is configured", ErrTaskOpUnavailable)
	}
	if _, busy := e.taskOps.LoadOrStore(executionID, op); busy {
		return nil, fmt.Errorf("%w: an analysis or fix is already running", ErrTaskOpUnavailable)
	}
	started := false
	defer func() {
		if !started {
			e.taskOps.Delete(executionID)
		}
	}()
	st, err := e.load(ctx, executionID)
	if err != nil {
		return nil, translate(err)
	}
	if err := taskOpsAvailable(st); err != nil {
		return nil, err
	}
	if st.rec.Status == execution.ExecutionPaused {
		// A step dispatched before the pause may still be running.
		m := e.lock(executionID)
		if !m.TryLock() {
			return nil, fmt.Errorf("%w: the step in progress when the execution paused has not finished yet", ErrTaskOpUnavailable)
		}
		m.Unlock()
	}
	if op == TaskOpFix {
		v, err := e.TaskOps(ctx, projectID, executionID)
		if err != nil {
			return nil, err
		}
		// TaskOps sees this operation as running; the fix is possible
		// when the analysis is fixable and not yet implemented.
		if v.Analysis == nil || !v.Analysis.Fixable || v.Fix != nil {
			return nil, fmt.Errorf("%w: run an analysis that finds optimizations first", ErrTaskOpUnavailable)
		}
	}
	typ := execution.EventUnitAnalysisStarted
	if op == TaskOpFix {
		typ = execution.EventUnitFixStarted
	}
	if err := e.event(ctx, nil, st, typ, map[string]any{"work_unit_id": st.unit.ID.String(), "key": wbs.UnitKey(st.unit), "by": by}); err != nil {
		return nil, translate(err)
	}
	started = true
	bg := context.WithoutCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer e.taskOps.Delete(executionID)
		defer containPanic(e.d.Logger, "task "+strings.ToLower(op), "execution_id", executionID.String())
		var err error
		if op == TaskOpAnalyze {
			err = e.runAnalysis(bg, st, note)
		} else {
			err = e.runFix(bg, st)
		}
		if err != nil {
			e.d.Logger.WarnContext(bg, "task "+strings.ToLower(op)+" failed", "execution_id", executionID.String(), "error", err.Error())
		}
	}()
	return done, nil
}

// runAnalysis sends "analyze task <KEY>" in a fresh session and records
// the submitted analysis (or why there is none).
func (e *Engine) runAnalysis(ctx context.Context, st *engineState, note string) error {
	key := wbs.UnitKey(st.unit)
	record := func(payload map[string]any) error {
		payload["work_unit_id"], payload["key"] = st.unit.ID.String(), key
		return e.event(ctx, nil, st, execution.EventUnitAnalyzed, payload)
	}
	fail := func(msg string) error { return record(map[string]any{"fixable": false, "error": msg}) }
	cmd, err := e.analysisCommand(ctx, st, note)
	if err != nil {
		return fail(err.Error())
	}
	// The analysis starts from a clear context.
	if _, err := e.d.Worker.NewSession(ctx, st.rec.ProjectID); err != nil {
		return fail("a new worker session could not be opened: " + err.Error())
	}
	dispatch, msg := e.sendTaskCommand(ctx, st, cmd, "ANALYZE_TASK")
	if msg != "" {
		return fail(msg)
	}
	a, problem, err := e.analysisResult(ctx, dispatch)
	if err != nil {
		return fail(err.Error())
	}
	if problem != "" {
		return fail(problem)
	}
	return record(map[string]any{"fixable": a.Fixable, "findings": a.Findings, "optimizations": a.Optimizations})
}

// runFix sends "fix task <KEY>" and adopts the result: a changed task
// definition through the replan checks; tests / code as they are. On
// success the task's attempt counters reset.
func (e *Engine) runFix(ctx context.Context, st *engineState) error {
	key := wbs.UnitKey(st.unit)
	record := func(success bool, msg string, changed bool) error {
		payload := map[string]any{"work_unit_id": st.unit.ID.String(), "key": key, "success": success, "message": msg,
			"definition_changed": changed}
		if success {
			payload["resets"] = requeueResets
		}
		return e.event(ctx, nil, st, execution.EventUnitFixed, payload)
	}
	cmd := fixCommand(st, key)
	dispatch, msg := e.sendTaskCommand(ctx, st, cmd, "FIX_TASK")
	if msg != "" {
		return record(false, msg, false)
	}
	cur, err := e.load(ctx, st.rec.ID)
	if err != nil {
		return record(false, "the execution could not be reloaded: "+err.Error(), false)
	}
	if cur.unit == nil || cur.unit.ID != st.unit.ID {
		return record(false, "the execution no longer has task "+key+" as its current task", false)
	}
	spec, changed, problem, err := e.replannedTask(ctx, cur, &dispatch)
	if err != nil {
		return record(false, err.Error(), false)
	}
	if problem != "" {
		return record(false, "the fix submitted an unacceptable task definition (run the analysis again): "+problem, false)
	}
	if !changed {
		return record(true, "Fix applied to the tests or code; the task definition is unchanged. Resume the execution to continue.", false)
	}
	uid := cur.unit.ID
	err = e.inTx(ctx, nil, func(tx persistence.Transaction) error {
		if err := e.d.Repo.ReplanUnit(ctx, tx, uid, spec); err != nil {
			return err
		}
		err := e.d.Repo.SetUnitStatus(ctx, tx, uid, []wbs.WorkUnitStatus{wbs.WorkUnitStatusVALIDATING, wbs.WorkUnitStatusINCOMPLETE,
			wbs.WorkUnitStatusEXECUTING}, wbs.WorkUnitStatusEXECUTING)
		if err != nil && !errors.Is(err, persistence.ErrConflict) {
			return err
		}
		return nil
	})
	if err != nil {
		return record(false, "the new task definition could not be stored: "+err.Error(), false)
	}
	return record(true, "Fix applied: the task definition was rewritten and adopted (key, kind and dependencies kept). Resume the execution to continue.", true)
}

// sendTaskCommand dispatches one ARCHITECT command outside the workflow
// (the execution is in recovery or paused) and returns its dispatch and ""
// when the worker replied, otherwise why not.
func (e *Engine) sendTaskCommand(ctx context.Context, st *engineState, cmd, stateKey string) (uuid.UUID, string) {
	wp, err := e.commandPrompt(ctx, st, feedback(cmd), stateKey, "ARCHITECT")
	if err != nil {
		return uuid.Nil, "the command could not be prepared: " + err.Error()
	}
	if _, err := e.ensureSession(ctx, st, true); err != nil {
		return uuid.Nil, "the worker session is not ready: " + err.Error()
	}
	res, err := e.d.Worker.Dispatch(ctx, DispatchRequest{ProjectID: st.rec.ProjectID, PhaseID: st.runtime.PhaseID, WorkUnitID: &st.unit.ID,
		WorkerPromptID: wp.ID, RuntimeStateID: st.runtime.ID, StateKey: stateKey, RoleKey: "ARCHITECT", Attempt: 1})
	if err != nil {
		return uuid.Nil, "the command could not be sent: " + err.Error()
	}
	if res.Err != nil {
		return res.Dispatch.ID, "the worker did not complete the command: " + res.Err.Error()
	}
	return res.Dispatch.ID, ""
}

// analysisCommand is "analyze task <KEY>" with what the ARCHITECT needs:
// the recovery reason and evidence, where the task is described, how the
// overseer validates, and the analysis format.
func (e *Engine) analysisCommand(ctx context.Context, st *engineState, note string) (string, error) {
	key := wbs.UnitKey(st.unit)
	var b strings.Builder
	fmt.Fprintf(&b, "analyze task %s\n\n", key)
	paused := st.rec.Status == execution.ExecutionPaused
	if paused {
		who := "The operator"
		if note != "" {
			who = "The overseer"
		}
		fmt.Fprintf(&b, "Act as the ARCHITECT. %s paused the execution while it was working on task %s.\n", who, key)
		if note != "" {
			b.WriteString(note + "\n")
		}
	} else {
		fmt.Fprintf(&b, "Act as the ARCHITECT. The execution stopped for operator recovery while working on task %s. Recovery reason: %s.\n",
			key, st.rec.RecoveryReason)
		if ev := evidenceText(st.rec.RecoveryEvidence); ev != "" {
			b.WriteString("Recovery evidence: " + ev + "\n")
		}
	}
	if _, run, err := e.identicalFailures(ctx, st, st.unit.ID); err != nil {
		return "", err
	} else if run != nil {
		b.WriteString("\nThe task's last failed validation:\n")
		size := 0
		for _, r := range run.Records {
			if r.Outcome == execution.ValidationPass {
				continue
			}
			fmt.Fprintf(&b, "`%s %s` → %s\n", r.Program, strings.Join(r.Args, " "), r.Outcome)
			for _, l := range failureLines(r) {
				if size += len(l); size > 6000 {
					b.WriteString("    …\n")
					break
				}
				b.WriteString("    " + l + "\n")
			}
		}
	}
	goal := "remove the cause of the recovery"
	if paused {
		goal = "keep the task from failing or stalling"
	}
	fmt.Fprintf(&b, "\nAnalyze task %s end to end: its definition (get_task), the phase WBS and plan (get_context), its acceptance "+
		"criteria, required tests and validation commands, and the test cases and code they exercise. The overseer runs the validation "+
		"commands itself, in %s, as %s. Look for optimizations to the task definition, its tests or the code that %s. Do not change anything yet. "+
		"fixable is true only when the optimizations remove the cause.", key, st.cwd(), validationUser(), goal)
	return b.String(), nil
}

// fixCommand is "fix task <KEY>": implement the analysis's optimizations.
func fixCommand(st *engineState, key string) string {
	return fmt.Sprintf("fix task %s\n\nAct as the ARCHITECT. Implement the optimizations of the last analysis of task %s "+
		"(get_execution_history), and nothing else. You may change the task's tests and code, and its definition (keep its key, kind and "+
		"dependencies). Do not mark the task done. The overseer runs the validation commands itself, in %s, as %s.",
		key, key, st.cwd(), validationUser())
}

// evidenceText renders recovery evidence as sorted "key=value" pairs.
func evidenceText(ev map[string]any) string {
	keys := make([]string, 0, len(ev))
	for k := range ev {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, _ := json.Marshal(ev[k])
		s := string(v)
		if len(s) > 500 {
			s = s[:500] + "…"
		}
		parts = append(parts, k+"="+s)
	}
	return strings.Join(parts, "; ")
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, x := range items {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
