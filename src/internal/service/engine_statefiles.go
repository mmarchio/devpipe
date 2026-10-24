package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
	"overseer/internal/worker"
)

// ---------------------------------------------------------------------------
// Statefile workflow. Every action state sends one command to the worker
// (its CMD_ template, rendered as is, with an MCP capability token) and
// the team submits the command's result through the overseer MCP server:
//
//	SKILL       "create the <project>-output-format skill"
//	PLAN        "plan phase <N>"   ARCHITECT + PLANNER submit the phase plan
//	ACCEPTANCE  "...acceptance..."  PROJECT_MANAGER accepts the finished task;
//	                               the overseer archives it and loads the next
//	IMPLEMENT   "implement task <K>" ENGINEER implements the active task
//	CONTINUE    "continue"         ENGINEER resumes an interrupted run
//	TEST        "test task <K>"    TESTER reports the criteria; all met → COMPLETE
//
// The check states read the submitted results (task_reports) and decide
// every transition; the worker's reply is never parsed. Deterministic
// validation and the checkpoint run under overseer control between TEST
// and acceptance. The statefiles that once held the workflow state are no
// longer read or written; only the generated team files remain on disk.

// maxCommandRetries bounds resending a command whose dispatch failed.
const maxCommandRetries = 3

// Command template variables.
const (
	CommandVarPhase = "command.phase" // the working phase's order
	CommandVarTask  = "command.task"  // the active task's key
	CommandVarSkill = "command.skill" // the project's output-format skill name
)

// commandText renders the current state's command template.
func (e *Engine) commandText(ctx context.Context, st *engineState) (string, error) {
	key := ""
	if st.curRec != nil {
		key = st.curRec.PromptTemplate
	}
	var tmpl *execution.PromptTemplateSnapshot
	for i := range st.content.PromptTemplates {
		if st.content.PromptTemplates[i].Key == key {
			tmpl = &st.content.PromptTemplates[i]
		}
	}
	if tmpl == nil || e.d.TemplateVers == nil {
		return "", fmt.Errorf("%w: state %s has no captured command template %q", ErrInvalidInput, st.cur.Key, key)
	}
	v, err := e.d.TemplateVers.GetVersion(ctx, tmpl.TemplateID, tmpl.Version)
	if err != nil {
		return "", fmt.Errorf("command template %s v%d: %w", key, tmpl.Version, err)
	}
	vars := map[string]string{}
	if work := st.workPhase(); work != nil {
		vars[CommandVarPhase] = strconv.Itoa(work.Order)
	}
	if st.unit != nil {
		vars[CommandVarTask] = wbs.UnitKey(st.unit)
	}
	p, err := e.d.Projects.Get(ctx, st.rec.ProjectID.String())
	if err != nil {
		return "", err
	}
	vars[CommandVarSkill] = OutputFormatSkillName(p.Name)
	cmd, err := renderCommand(v.Content, vars)
	if err != nil {
		return "", err
	}
	if key == "CMD_SKILL" {
		format, err := e.responseFormat(ctx, st.rec.ProjectID)
		if err != nil {
			return "", err
		}
		return cmd + "\n\nCreate this Hermes skill now with your skill management tool, named " + vars[CommandVarSkill] +
			" (replace an existing skill of that name), with exactly the SKILL.md content below. Load and follow it for every command of this project. " +
			"Reply once it is saved.\n\n````markdown\n" + feedback(OutputFormatSkill(p.Name, format)) + "````", nil
	}
	problem, err := e.lastStatefileProblem(ctx, st)
	if err != nil {
		return "", err
	}
	if problem != "" {
		cmd += "\n\nThe result of this command was rejected: " + feedback(problem) + ". Redo this command and submit its result."
	}
	if key == "CMD_ACCEPTANCE" {
		hint, err := e.requeueHint(ctx, st)
		if err != nil {
			return "", err
		}
		cmd += hint
	}
	if key == "CMD_REPLAN" {
		rc, err := e.replanContext(ctx, st)
		if err != nil {
			return "", err
		}
		return cmd + rc, nil
	}
	if key != "CMD_PLAN" {
		return cmd, nil
	}
	// A rejected plan: the problems travel in the command.
	var problems wbs.PlanProblems
	if work := st.workPhase(); work != nil {
		c, err := e.d.Repo.LatestCandidate(ctx, nil, st.rec.ID, work.ID)
		switch {
		case err == nil && c.Status == persistence.CandidateRejected:
			problems = c.Report
		case err != nil && !errors.Is(err, persistence.ErrNotFound):
			return "", err
		}
	}
	if len(problems) == 0 {
		return cmd, nil
	}
	lines := make([]string, 0, len(problems))
	for _, p := range problems {
		if p.Unit != "" {
			lines = append(lines, fmt.Sprintf("- %s %s: %s", p.Unit, p.Code, p.Message))
		} else {
			lines = append(lines, fmt.Sprintf("- %s: %s", p.Code, p.Message))
		}
	}
	return cmd + "\n\nThe previous plan was rejected:\n" + feedback(strings.Join(lines, "\n")) + "\nFix every problem and submit the corrected plan.", nil
}

// responseFormat is the project's worker response format (Markdown when
// no worker service is wired): how the team is told to format its replies.
func (e *Engine) responseFormat(ctx context.Context, projectID uuid.UUID) (worker.ResponseFormat, error) {
	if e.d.Worker == nil {
		return worker.FormatMarkdown, nil
	}
	ws, _, err := e.d.Worker.WorkerSettings(ctx, projectID)
	if err != nil {
		return "", err
	}
	return ws.ResponseFormat.OrDefault(), nil
}

// feedback makes problem text safe to send in a command: a placeholder
// marker quoted from a submitted result would make the command unsendable.
func feedback(s string) string {
	return strings.ReplaceAll(s, compression.PlaceholderMarker, "[placeholder]")
}

// renderCommand substitutes {{command.*}} variables; an unknown or
// unavailable variable is an error (a command is never sent half-filled).
func renderCommand(src string, vars map[string]string) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(src, "{{")
		if i < 0 {
			b.WriteString(src)
			break
		}
		j := strings.Index(src[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("command template: unterminated {{")
		}
		name := strings.TrimSpace(src[i+2 : i+j])
		val, ok := vars[name]
		if !ok {
			return "", fmt.Errorf("command template: %s is not available here", name)
		}
		b.WriteString(src[:i])
		b.WriteString(val)
		src = src[i+j+2:]
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", fmt.Errorf("command template renders an empty command")
	}
	return out, nil
}

// prepareCommand persists the step's command as its worker prompt (once:
// a resumed step reuses it).
func (e *Engine) prepareCommand(ctx context.Context, st *engineState, step *persistence.StepRecord) (*prepared, error) {
	p := &prepared{}
	if len(st.cur.Roles) > 0 {
		p.roleKey = st.cur.Roles[0]
	}
	if step.WorkerPromptID != nil {
		// A command prepared without a capability token (for the retired
		// statefiles, before an upgrade) is prepared again.
		if ok, err := e.promptHasToken(ctx, *step.WorkerPromptID); err != nil {
			return nil, err
		} else if ok {
			p.promptID = *step.WorkerPromptID
			return p, nil
		}
	}
	if e.d.CommandPrompts == nil {
		return nil, fmt.Errorf("%w: command prompts are not configured", ErrInfrastructure)
	}
	cmd, err := e.commandText(ctx, st)
	if err != nil {
		return nil, err
	}
	wp, err := e.commandPrompt(ctx, st, cmd, st.cur.Key, p.roleKey)
	if err != nil {
		return nil, err
	}
	p.promptID = wp.ID
	if err := e.d.Repo.SetStepRefs(ctx, nil, step.ID, persistence.StepRefs{WorkerPromptID: &p.promptID,
		Detail: map[string]any{"command": cmd, "role": p.roleKey}}); err != nil {
		return nil, err
	}
	step.WorkerPromptID = &p.promptID
	return p, nil
}

// handleCommand sends the current command state's command.
func (e *Engine) handleCommand(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	tmpl := ""
	if st.curRec != nil {
		tmpl = st.curRec.PromptTemplate
	}
	var opts advanceOpts
	var req DispatchRequest
	if tmpl == "CMD_SKILL" {
		// The skill is saved before any phase work is routed: no phase
		// is needed (every phase may be done already).
		if work := st.workPhase(); work != nil {
			pid := work.ID
			req.PhaseID = &pid
		}
	} else {
		work := st.workPhase()
		if work == nil {
			return e.requireRecovery(ctx, st, step, "no phase to work on", nil)
		}
		pid := work.ID
		opts = advanceOpts{SetPhase: true, PhaseID: &pid}
		req = DispatchRequest{PhaseID: &pid, WorkUnitID: unitIDOf(st)}
		switch {
		case tmpl == "CMD_PLAN":
			if st.reusablePlan() {
				// A valid current plan is never regenerated (WBS 16.11).
				return e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), advanceOpts{SetPhase: true, PhaseID: &pid,
					Refs: persistence.StepRefs{PlanID: &st.plan.ID, Detail: map[string]any{"reuse_plan": true}}})
			}
			if st.wbsVer <= 0 {
				return e.requireRecovery(ctx, st, step, "the phase has no WBS to plan from", map[string]any{"phase_id": pid.String()})
			}
			// The team plans from the approved WBS (get_context), with a
			// clear context.
			if stop, err := e.freshSession(ctx, st, step); stop != nil || err != nil {
				if err != nil {
					return execution.StepResult{}, err
				}
				return *stop, nil
			}
		case tmpl == "CMD_IMPLEMENT" || tmpl == "CMD_CONTINUE" || tmpl == "CMD_TEST":
			if st.unit == nil {
				return e.requireRecovery(ctx, st, step, "no active task", nil)
			}
			if stop, err := e.repeatedCommand(ctx, st, step); stop != nil || err != nil {
				if err != nil {
					return execution.StepResult{}, err
				}
				return *stop, nil
			}
			implements, stop, err := e.boundTaskCommand(ctx, st, step, tmpl)
			if stop != nil || err != nil {
				if err != nil {
					return execution.StepResult{}, err
				}
				return *stop, nil
			}
			if tmpl == "CMD_IMPLEMENT" && implements == 0 {
				// The next task is loaded: a requested pause applies now.
				if stop, err := e.pauseBeforeTask(ctx, st, step); stop != nil || err != nil {
					if err != nil {
						return execution.StepResult{}, err
					}
					return *stop, nil
				}
				// A new task starts with a clear context.
				if stop, err := e.freshSession(ctx, st, step); stop != nil || err != nil {
					if err != nil {
						return execution.StepResult{}, err
					}
					return *stop, nil
				}
			}
			if tmpl != "CMD_TEST" && st.unit.Status != wbs.WorkUnitStatusEXECUTING {
				// Implementation (re)starts: the unit is EXECUTING again.
				if err := e.d.Repo.SetUnitStatus(ctx, nil, st.unit.ID, []wbs.WorkUnitStatus{wbs.WorkUnitStatusINCOMPLETE, wbs.WorkUnitStatusVALIDATING,
					wbs.WorkUnitStatusEXECUTING}, wbs.WorkUnitStatusEXECUTING); err != nil && !errors.Is(err, persistence.ErrConflict) {
					return execution.StepResult{}, err
				}
			}
		}
	}
	if reset, err := e.pendingContextReset(ctx, st); err != nil {
		return execution.StepResult{}, err
	} else if reset {
		if stop, err := e.freshSession(ctx, st, step); stop != nil || err != nil {
			if err != nil {
				return execution.StepResult{}, err
			}
			return *stop, nil
		}
	}
	out, stop, err := e.dispatchStep(ctx, st, step, req)
	if err != nil || stop != nil {
		if stop == nil {
			return execution.StepResult{}, err
		}
		return *stop, err
	}
	id := out.dispatch.ID
	opts.Refs = persistence.StepRefs{DispatchID: &id}
	return e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), opts)
}

func unitIDOf(st *engineState) *uuid.UUID {
	if st.unit == nil {
		return nil
	}
	id := st.unit.ID
	return &id
}

// boundTaskCommand stops repeated work on one task: IMPLEMENT runs at
// most 1 + MaxFixAttempts times (each repeat follows a failed test or
// validation), CONTINUE at most the project's continuations per unit
// since the last IMPLEMENT. It returns the task's earlier IMPLEMENT sends.
func (e *Engine) boundTaskCommand(ctx context.Context, st *engineState, step *persistence.StepRecord, tmpl string) (int, *execution.StepResult, error) {
	if tmpl == "CMD_TEST" {
		return 0, nil, nil
	}
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return 0, nil, err
	}
	// A resolved fix-attempt / continuation limit recovery resets its
	// counter: only sends after the reset count.
	fixReset, err := e.limitResetAt(ctx, st, LimitFixAttempts)
	if err != nil {
		return 0, nil, err
	}
	contReset, err := e.limitResetAt(ctx, st, LimitContinuations)
	if err != nil {
		return 0, nil, err
	}
	// A requeued task starts over: sends before its last requeue do not
	// count (its first send again clears the context).
	var requeuedAt time.Time
	if rq, err := e.d.Repo.LastEvent(ctx, nil, st.rec.ID, execution.EventUnitRequeued, &st.unit.ID); err != nil {
		return 0, nil, err
	} else if rq != nil {
		requeuedAt = rq.At
	}
	// implements counts every send (a new task's first one clears the
	// context); limited only those since the fix-attempt reset.
	implements, limited, continues := 0, 0, 0
	for _, s := range steps {
		if s.ID == step.ID || s.Status != execution.StepApplied || s.Action != execution.ActionCommand || s.WorkUnitID == nil || *s.WorkUnitID != st.unit.ID ||
			!s.IntentAt.After(requeuedAt) {
			continue
		}
		switch s.StateKey {
		case "IMPLEMENT":
			implements++
			if s.IntentAt.After(fixReset) {
				limited++
			}
			continues = 0
		case "CONTINUE":
			if s.IntentAt.After(contReset) {
				continues++
			}
		}
	}
	uid := st.unit.ID.String()
	switch tmpl {
	case "CMD_IMPLEMENT":
		if limited > 0 && execution.AttemptDecision(limited, st.engine.MaxFixAttempts+1) == execution.AttemptExhausted {
			r, err := e.requireRecovery(ctx, st, step, reasonFixExhausted, map[string]any{"work_unit_id": uid, "attempts": limited - 1})
			return implements, &r, err
		}
		if implements > 0 {
			if err := e.event(ctx, nil, st, execution.EventFixAttempt, map[string]any{"work_unit_id": uid, "attempt": implements}); err != nil {
				return implements, nil, err
			}
		}
	case "CMD_CONTINUE":
		limit := -1
		if e.d.Continuations != nil {
			if cs, _, err := e.d.Continuations.ContinuationSettings(ctx, st.rec.ProjectID); err == nil {
				limit = cs.MaxContinuationsPerWorkUnit
			}
		}
		if limit >= 0 && continues >= limit {
			r, err := e.requireRecovery(ctx, st, step, reasonContinuationPrefix+": the task was interrupted again", map[string]any{"work_unit_id": uid, "continuations": continues})
			return implements, &r, err
		}
	}
	return implements, nil, nil
}

// checkSkillCommand checks the latest SKILL dispatch: nil when Hermes
// replied (or nothing was sent); otherwise the command is resent (RETRY →
// SKILL, bounded) or the execution needs recovery.
func (e *Engine) checkSkillCommand(ctx context.Context, st *engineState, step *persistence.StepRecord) (*execution.StepResult, error) {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return nil, err
	}
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		if s.StateKey != "SKILL" || s.Status != execution.StepApplied || s.DispatchID == nil {
			continue
		}
		return e.commandFailed(ctx, st, step, *s.DispatchID, "the skill command", persistence.StepRefs{DispatchID: s.DispatchID})
	}
	return nil, nil
}

// detailFreshSession records on a step the worker session it opened.
const detailFreshSession = "fresh_session"

// freshSession clears the worker's context before a new phase is planned
// or a new task starts: a new worker session, once per step (a resumed
// step keeps the session it opened). The dispatch then restores the
// project's working directory ("cd <working directory>") before the
// command.
func (e *Engine) freshSession(ctx context.Context, st *engineState, step *persistence.StepRecord) (*execution.StepResult, error) {
	if _, ok := step.Detail[detailFreshSession]; ok {
		return nil, nil
	}
	if e.d.Worker == nil {
		return nil, nil
	}
	sess, err := e.d.Worker.NewSession(ctx, st.rec.ProjectID)
	if err != nil {
		r, rerr := e.requireRecovery(ctx, st, step, "a new worker session could not be opened", map[string]any{"error": err.Error()})
		return &r, rerr
	}
	detail := map[string]any{detailFreshSession: sess.ID.String()}
	if err := e.d.Repo.SetStepRefs(ctx, nil, step.ID, persistence.StepRefs{Detail: detail}); err != nil {
		return nil, err
	}
	if step.Detail == nil {
		step.Detail = map[string]any{}
	}
	step.Detail[detailFreshSession] = sess.ID.String()
	return nil, nil
}

// statefileWorkflow reports whether the execution runs the statefile
// workflow (its machine has the ACCEPTANCE_CHECK state).
func (st *engineState) statefileWorkflow() bool {
	for _, s := range st.machine.States {
		if s.Key == "ACCEPTANCE_CHECK" {
			return true
		}
	}
	return false
}

// commandFailed classifies the pending command dispatch: nil when the
// worker replied; otherwise the step either resends the command (RETRY,
// bounded) or requires recovery.
func (e *Engine) commandFailed(ctx context.Context, st *engineState, step *persistence.StepRecord, dispatchID uuid.UUID, what string, refs persistence.StepRefs) (*execution.StepResult, error) {
	d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, dispatchID)
	if err != nil {
		return nil, err
	}
	we := dispatchErr(d)
	if we == nil {
		return nil, nil
	}
	ev := map[string]any{"dispatch_id": dispatchID.String(), "error": we.Error(), "code": string(we.Code)}
	retries, err := e.consecutiveFailedCommands(ctx, st, d.StateKey)
	if err != nil {
		return nil, err
	}
	if retries >= maxCommandRetries {
		r, err := e.requireRecovery(ctx, st, step, what+reasonCommandResendsMarker+strconv.Itoa(retries)+" attempts: "+we.Error(), ev)
		return &r, err
	}
	r, err := e.advance(ctx, st, step, "RETRY", advanceOpts{Refs: refs, Outcome: "RETRY"})
	return &r, err
}

// detailStatefileProblem is the step detail key holding why a check state
// rejected the team's submitted result; the resent command carries it.
// (The key predates the MCP results and is kept: step history uses it.)
const detailStatefileProblem = "statefile_problem"

// statefileRetry handles a result the team submitted wrongly (missing,
// malformed, or contradicting the workflow rules): the command is resent
// with the problem appended (RETRY, bounded by maxCommandRetries), then
// recovery.
func (e *Engine) statefileRetry(ctx context.Context, st *engineState, step *persistence.StepRecord, problem string, refs persistence.StepRefs) (execution.StepResult, error) {
	n, err := e.consecutiveStatefileRetries(ctx, st, st.cur.Key)
	if err != nil {
		return execution.StepResult{}, err
	}
	if n >= maxCommandRetries {
		return e.requireRecovery(ctx, st, step, fmt.Sprintf("%s %s%d resends with the problem)", problem, reasonStatefileMarker, n),
			map[string]any{detailStatefileProblem: problem, "resends": n})
	}
	if refs.Detail == nil {
		refs.Detail = map[string]any{}
	}
	refs.Detail[detailStatefileProblem] = problem
	return e.advance(ctx, st, step, "RETRY", advanceOpts{Refs: refs, Outcome: "RETRY"})
}

// consecutiveStatefileRetries counts the check state's most recent
// applied steps that rejected the submitted result, back to the last one
// that did not.
func (e *Engine) consecutiveStatefileRetries(ctx context.Context, st *engineState, stateKey string) (int, error) {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return 0, err
	}
	reset, err := e.limitResetAt(ctx, st, LimitStatefileResends)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		if !s.IntentAt.After(reset) {
			break // counted from the last reset
		}
		if s.StateKey != stateKey || s.Status != execution.StepApplied {
			continue
		}
		if _, ok := s.Detail[detailStatefileProblem].(string); !ok {
			break
		}
		n++
	}
	return n, nil
}

// lastStatefileProblem returns the problem of the most recent applied step
// when that step rejected the submitted result and resent this command
// ("" otherwise).
func (e *Engine) lastStatefileProblem(ctx context.Context, st *engineState) (string, error) {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return "", err
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if s := steps[i]; s.Status == execution.StepApplied {
			p, _ := s.Detail[detailStatefileProblem].(string)
			if s.Outcome != "RETRY" {
				return "", nil
			}
			return p, nil
		}
	}
	return "", nil
}

// consecutiveFailedCommands counts the most recent sends of one command
// state that ended without a reply.
func (e *Engine) consecutiveFailedCommands(ctx context.Context, st *engineState, stateKey string) (int, error) {
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return 0, err
	}
	reset, err := e.limitResetAt(ctx, st, LimitCommandResends)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		if !s.IntentAt.After(reset) {
			break // counted from the last reset
		}
		if s.Action != execution.ActionCommand || s.StateKey != stateKey || s.Status != execution.StepApplied || s.DispatchID == nil {
			continue
		}
		d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, *s.DispatchID)
		if err != nil {
			return 0, err
		}
		if dispatchErr(d) == nil {
			break
		}
		n++
	}
	return n, nil
}

// handleImplementCheck: a reply from IMPLEMENT / CONTINUE leads to TEST;
// a run cut off before the reply (timeout, unknown delivery, context
// limit) is resumed with CONTINUE; anything else needs recovery.
func (e *Engine) handleImplementCheck(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	pend, err := e.pendingDispatch(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	if pend == nil || st.unit == nil {
		return e.requireRecovery(ctx, st, step, "no implementation dispatch to check", nil)
	}
	refs := persistence.StepRefs{DispatchID: pend}
	d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, *pend)
	if err != nil {
		return execution.StepResult{}, err
	}
	we := dispatchErr(d)
	if we == nil {
		if stop, err := e.implementResult(ctx, st, step, *pend, refs); stop != nil || err != nil {
			if err != nil {
				return execution.StepResult{}, err
			}
			return *stop, nil
		}
		return e.advance(ctx, st, step, "REPLIED", advanceOpts{Refs: refs, Outcome: "REPLIED"})
	}
	switch we.Code {
	case worker.CodeTimeout, worker.CodeDeliveryUnknown, worker.CodeContextLimit, worker.CodeCancelled, worker.CodeUnavailable:
		return e.advance(ctx, st, step, "INTERRUPTED", advanceOpts{Refs: refs, Outcome: "INTERRUPTED"})
	}
	return e.requireRecovery(ctx, st, step, "the implementation command failed: "+we.Error(),
		map[string]any{"dispatch_id": pend.String(), "code": string(we.Code)})
}

// handleTestCheck: the TESTER reports every acceptance criterion and
// COMPLETE when all are met → overseer validation; otherwise the ENGINEER
// gets the task again. Met MANUAL criteria are recorded as the tester's
// approvals.
func (e *Engine) handleTestCheck(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	pend, err := e.pendingDispatch(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	if pend == nil || st.unit == nil {
		return e.requireRecovery(ctx, st, step, "no test dispatch to check", nil)
	}
	refs := persistence.StepRefs{DispatchID: pend}
	if stop, err := e.commandFailed(ctx, st, step, *pend, "the test command", refs); stop != nil || err != nil {
		if err != nil {
			return execution.StepResult{}, err
		}
		return *stop, nil
	}
	return e.testResult(ctx, st, step, *pend, refs)
}

// toValidating moves a unit to VALIDATING (its tester reported every
// criterion met; overseer validation follows).
func toValidating(ctx context.Context, e *Engine, uid uuid.UUID) func(persistence.Transaction) error {
	return func(tx persistence.Transaction) error {
		err := e.d.Repo.SetUnitStatus(ctx, tx, uid, []wbs.WorkUnitStatus{wbs.WorkUnitStatusEXECUTING, wbs.WorkUnitStatusINCOMPLETE,
			wbs.WorkUnitStatusVALIDATING}, wbs.WorkUnitStatusVALIDATING)
		if errors.Is(err, persistence.ErrConflict) {
			return nil
		}
		return err
	}
}

// handleAcceptanceCheck: the PROJECT_MANAGER's result accepts the
// finished task, and the overseer loads the next executable task (NEXT)
// or completes the phase (PHASE_COMPLETE). A task still in progress
// (resumed execution) continues, or goes straight to validation (TESTED)
// when it was tested already.
func (e *Engine) handleAcceptanceCheck(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if st.plan == nil {
		return e.requireRecovery(ctx, st, step, "no current plan", nil)
	}
	pend, err := e.pendingDispatch(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	refs := persistence.StepRefs{DispatchID: pend}
	if pend != nil {
		if stop, err := e.commandFailed(ctx, st, step, *pend, "the acceptance command", refs); stop != nil || err != nil {
			if err != nil {
				return execution.StepResult{}, err
			}
			return *stop, nil
		}
	}
	return e.acceptanceResult(ctx, st, step, pend, refs)
}

// finishUnit checks the unit's passing validation, checkpoints it and
// returns the write marking it DONE (the unit gates, WBS 16.53–16.58).
func (e *Engine) finishUnit(ctx context.Context, st *engineState, step *persistence.StepRecord, u *wbs.WorkUnit) (func(persistence.Transaction) error, pendingEvent, *execution.StepResult, error) {
	uid := u.ID
	stop := func(reason string, ev map[string]any) (func(persistence.Transaction) error, pendingEvent, *execution.StepResult, error) {
		r, err := e.requireRecovery(ctx, st, step, reason, ev)
		return nil, pendingEvent{}, &r, err
	}
	run, err := e.d.Repo.LatestValidationRun(ctx, nil, st.rec.ID, execution.ScopeUnit, uid)
	if err != nil || run.Status != string(execution.ValidationPass) || run.Acceptance == nil || run.Acceptance.Outcome != execution.AcceptancePass {
		return stop("task "+wbs.UnitKey(u)+" was accepted without a passing overseer validation", map[string]any{"work_unit_id": uid.String()})
	}
	cp, err := e.checkpoint(ctx, st, step, execution.RepoCheckpointUnit, uid, &run.ID, st.engine.CheckpointPolicy, fmt.Sprintf("task %s %s", wbs.UnitKey(u), u.Name))
	if err != nil {
		return nil, pendingEvent{}, nil, err
	}
	if cp.Status == persistence.CheckpointFailed && execution.OnCheckpointFailure(st.engine.CheckpointPolicy) == execution.CheckpointBlock {
		reason, ev := e.checkpointFailure(ctx, st, "required checkpoint failed", cp.Error, map[string]any{"checkpoint_id": cp.ID.String(), "error": cp.Error})
		return stop(reason, ev)
	}
	runID := run.ID
	return func(tx persistence.Transaction) error { return e.d.Repo.MarkUnitDone(ctx, tx, uid, st.rec.ID, runID) },
		pendingEvent{Type: execution.EventUnitDone, Payload: map[string]any{"work_unit_id": uid.String(), "validation_run_id": runID.String(),
			"checkpoint_id": cp.ID.String(), "checkpoint_status": cp.Status}}, nil, nil
}

// chainTx runs the writes in order inside one transaction (nil when none).
func chainTx(fns []func(persistence.Transaction) error) func(persistence.Transaction) error {
	if len(fns) == 0 {
		return nil
	}
	return func(tx persistence.Transaction) error {
		for _, f := range fns {
			if f == nil {
				continue
			}
			if err := f(tx); err != nil {
				return err
			}
		}
		return nil
	}
}
