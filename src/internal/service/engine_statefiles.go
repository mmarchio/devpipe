package service

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/bounds"
	"overseer/internal/compression"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
	"overseer/internal/worker"
)

// ---------------------------------------------------------------------------
// Statefile workflow. Every action state sends one command to the worker
// (its CMD_ template, rendered as is) and the team works in
// <working directory>/statefiles:
//
//	SKILL       "create the <project>-output-format skill"
//	PLAN        "plan phase <N>"   ARCHITECT writes PLAN.md, PLANNER BACKLOG.md
//	ACCEPTANCE  "...acceptance..."  PROJECT_MANAGER archives the finished task
//	                               and loads the next one into ACTIVE_TASK.md
//	IMPLEMENT   "implement task <K>" ENGINEER implements ACTIVE_TASK.md
//	CONTINUE    "continue"         ENGINEER resumes an interrupted run
//	TEST        "test task <K>"    TESTER checks the criteria; all met → COMPLETE
//
// The check states read the statefiles back and decide every transition;
// the worker's reply is never parsed. Deterministic validation and the
// checkpoint run under overseer control between TEST and acceptance.

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
	format, err := e.statefileFormat(ctx, st.rec.ProjectID)
	if err != nil {
		return "", err
	}
	if key == "CMD_SKILL" {
		return cmd + "\n\nCreate this Hermes skill now with your skill management tool, named " + vars[CommandVarSkill] +
			" (replace an existing skill of that name), with exactly the SKILL.md content below. Load and follow it for every command of this project. " +
			"Reply once it is saved.\n\n````markdown\n" + feedback(OutputFormatSkill(p.Name, format)) + "````", nil
	}
	problem, err := e.lastStatefileProblem(ctx, st)
	if err != nil {
		return "", err
	}
	if problem != "" {
		cmd += "\n\nThe statefiles you wrote for this command were rejected: " + feedback(problem) +
			". Fix them to the layout and rules in " + GuideFile + " and redo this command." + expectedFormat(format)
	}
	if key == "CMD_REPLAN" {
		rc, err := e.replanContext(ctx, st, format)
		if err != nil {
			return "", err
		}
		return cmd + rc, nil
	}
	if key != "CMD_PLAN" {
		return cmd, nil
	}
	// A rejected plan: the problems travel in the command (and in
	// PLAN_REJECTED.md, which also covers a rejection from an earlier
	// execution of the project).
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
	if len(problems) > 0 {
		lines := make([]string, 0, len(problems))
		for _, p := range problems {
			if p.Unit != "" {
				lines = append(lines, fmt.Sprintf("- %s %s: %s", p.Unit, p.Code, p.Message))
			} else {
				lines = append(lines, fmt.Sprintf("- %s: %s", p.Code, p.Message))
			}
		}
		cmd += "\n\nThe previous plan was rejected:\n" + feedback(strings.Join(lines, "\n")) + "\nFix every problem (also listed in " +
			wbs.PlanRejectedFile + ") and rewrite " + wbs.BacklogFile + "; do not keep the files as they are." + expectedFormat(format)
		return cmd, nil
	}
	if abs, err := statefile.SafeRelPath(st.cwd(), wbs.PlanRejectedFile); err == nil {
		if _, err := os.Stat(abs); err == nil {
			cmd += "\n\nThe previous plan was rejected. Fix every problem listed in " + wbs.PlanRejectedFile +
				" and rewrite " + wbs.BacklogFile + "; do not keep the files as they are." + expectedFormat(format)
		}
	}
	return cmd, nil
}

// expectedFormat is the task file layout a resent command carries, so the
// team can fix its files without looking the format up.
func expectedFormat(format worker.ResponseFormat) string {
	return "\n\nExpected format:\n\n" + TaskFileFormat(format)
}

// statefileFormat is the project's statefile format: its worker response
// format (Markdown when no worker service is wired). Task files are read
// in either format; this decides what the team is told to write.
func (e *Engine) statefileFormat(ctx context.Context, projectID uuid.UUID) (worker.ResponseFormat, error) {
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
// marker quoted from a statefile would make the command unsendable.
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
		p.promptID = *step.WorkerPromptID
		return p, nil
	}
	if e.d.CommandPrompts == nil {
		return nil, fmt.Errorf("%w: command prompts are not configured", ErrInfrastructure)
	}
	cmd, err := e.commandText(ctx, st)
	if err != nil {
		return nil, err
	}
	wp, err := compression.WorkerPromptFromCommand(cmd, e.d.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("command %q: %w", cmd, err)
	}
	if err := e.d.CommandPrompts.CreateWorkerPrompt(ctx, st.rec.ProjectID, &wp); err != nil {
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
			// The team plans from the approved WBS, with a clear context.
			if err := e.loadWBSFiles(ctx, st); err != nil {
				return execution.StepResult{}, err
			}
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
			implements, stop, err := e.boundTaskCommand(ctx, st, step, tmpl)
			if stop != nil || err != nil {
				if err != nil {
					return execution.StepResult{}, err
				}
				return *stop, nil
			}
			if tmpl == "CMD_IMPLEMENT" && implements == 0 {
				// A new task starts with a clear context.
				if stop, err := e.freshSession(ctx, st, step); stop != nil || err != nil {
					if err != nil {
						return execution.StepResult{}, err
					}
					return *stop, nil
				}
			}
			if tmpl == "CMD_TEST" {
				// The tester must record its result in ACTIVE_TASK.md: its
				// hash before the command tells the test check whether it did.
				if _, err := e.verifyStatefiles(ctx, st, wbs.ActiveTaskFile); err != nil {
					return execution.StepResult{}, err
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
	// implements counts every send (a new task's first one clears the
	// context); limited only those since the fix-attempt reset.
	implements, limited, continues := 0, 0, 0
	for _, s := range steps {
		if s.ID == step.ID || s.Status != execution.StepApplied || s.Action != execution.ActionCommand || s.WorkUnitID == nil || *s.WorkUnitID != st.unit.ID {
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

// loadWBSFiles writes every phase's current WBS to statefiles/wbs
// (creating the directory), so the team plans from the approved WBS.
func (e *Engine) loadWBSFiles(ctx context.Context, st *engineState) error {
	for _, ph := range st.phases {
		v, err := e.d.WBSVersions.GetCurrentVersion(ctx, ph.ID.String())
		if err != nil || v <= 0 {
			continue
		}
		rec, err := e.d.WBSVersions.GetVersion(ctx, ph.ID.String(), v)
		if err != nil {
			return err
		}
		if err := writeStatefile(st.cwd(), wbs.WBSFile(ph.Order), rec.Content); err != nil {
			return err
		}
	}
	return nil
}

// writeStatefile writes content to rel under the working directory
// (creating directories) unless it already holds exactly that content.
func writeStatefile(cwd, rel, content string) error {
	// Both the directory and the file are confined to the working
	// directory (symlinks resolved) before anything is written.
	dir, err := statefile.SafeRelPath(cwd, filepath.Dir(rel))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	abs, err := statefile.SafeRelPath(cwd, rel)
	if err != nil {
		return err
	}
	if cur, err := os.ReadFile(abs); err == nil && bytes.Equal(cur, []byte(content)) {
		return nil
	}
	tmp := abs + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, abs)
}

// removeStatefile removes rel under the working directory (confined like
// writeStatefile); a missing file is not an error.
func removeStatefile(cwd, rel string) error {
	abs, err := statefile.SafeRelPath(cwd, rel)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// readPlanningDoc reads a statefile the team wrote under the working
// directory. The problem is non-empty when the file is missing, unsafe,
// empty or too large.
func readPlanningDoc(cwd, rel string) (content, problem string) {
	abs, err := statefile.SafeRelPath(cwd, rel)
	if err != nil {
		return "", fmt.Sprintf("%s is not a safe path in the working directory: %v", rel, err)
	}
	b, err := os.ReadFile(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", rel + " was not written"
	case err != nil:
		return "", fmt.Sprintf("%s cannot be read: %v", rel, err)
	}
	content = string(b)
	if strings.TrimSpace(content) == "" {
		return "", rel + " is empty"
	}
	if err := bounds.Check(bounds.WBS, content); err != nil {
		return "", fmt.Sprintf("%s: %v", rel, err)
	}
	return content, ""
}

// readTaskFile parses a task statefile; a missing file (when allowed) or
// one without task sections has no tasks.
func readTaskFile(cwd, rel string, allowMissing bool) (*wbs.TaskDoc, string) {
	content, problem := readPlanningDoc(cwd, rel)
	if problem != "" {
		if allowMissing && (strings.HasSuffix(problem, "was not written") || strings.HasSuffix(problem, "is empty")) {
			return &wbs.TaskDoc{}, ""
		}
		return nil, problem
	}
	d, err := wbs.ParseTasks(content)
	if err != nil {
		return nil, rel + ": " + err.Error()
	}
	return d, ""
}

// Statefile verification outcomes.
const (
	FileCreated   = "CREATED"   // first verification of the file
	FileChanged   = "CHANGED"   // overwritten with different content
	FileUnchanged = "UNCHANGED" // same content as the last verification
)

// verifyStatefiles hashes each statefile the team wrote (MD5 of its
// bytes, read at its expected path under the working directory), stores
// the hash and compares it with the stored one. Missing files are
// skipped: the reads before it already decided whether they may be
// missing. The result maps each verified path to its outcome.
func (e *Engine) verifyStatefiles(ctx context.Context, st *engineState, rels ...string) (map[string]string, error) {
	out := map[string]string{}
	for _, rel := range rels {
		abs, err := statefile.SafeRelPath(st.cwd(), rel)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		sum := md5.Sum(b)
		cur := hex.EncodeToString(sum[:])
		prev, err := e.d.Repo.RecordStatefileHash(ctx, st.rec.ID, rel, cur)
		if err != nil {
			return nil, err
		}
		switch prev {
		case "":
			out[rel] = FileCreated
		case cur:
			out[rel] = FileUnchanged
		default:
			out[rel] = FileChanged
		}
	}
	return out, nil
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
// rejected the team's statefiles; the resent command carries it.
const detailStatefileProblem = "statefile_problem"

// statefileRetry handles statefiles the team wrote wrongly (unparsable,
// missing, or contradicting the workflow rules): the command is resent
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
// applied steps that rejected the statefiles, back to the last one that
// did not.
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
// when that step rejected the statefiles and resent this command ("" otherwise).
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
		return e.advance(ctx, st, step, "REPLIED", advanceOpts{Refs: refs, Outcome: "REPLIED"})
	}
	switch we.Code {
	case worker.CodeTimeout, worker.CodeDeliveryUnknown, worker.CodeContextLimit, worker.CodeCancelled, worker.CodeUnavailable:
		return e.advance(ctx, st, step, "INTERRUPTED", advanceOpts{Refs: refs, Outcome: "INTERRUPTED"})
	}
	return e.requireRecovery(ctx, st, step, "the implementation command failed: "+we.Error(),
		map[string]any{"dispatch_id": pend.String(), "code": string(we.Code)})
}

// activeTask reads ACTIVE_TASK.md and requires it to hold exactly the
// active unit.
func (e *Engine) activeTask(st *engineState) (*wbs.Task, string) {
	d, problem := readTaskFile(st.cwd(), wbs.ActiveTaskFile, false)
	if problem != "" {
		return nil, problem
	}
	key := wbs.UnitKey(st.unit)
	if len(d.Tasks) != 1 || d.Tasks[0].Unit.Key != key {
		names := make([]string, 0, len(d.Tasks))
		for _, t := range d.Tasks {
			names = append(names, t.Unit.Key)
		}
		return nil, fmt.Sprintf("%s must hold the active task %s; it holds [%s]", wbs.ActiveTaskFile, key, strings.Join(names, ", "))
	}
	return &d.Tasks[0], ""
}

// handleTestCheck: the TESTER marks every acceptance criterion and sets
// COMPLETE when all are met → overseer validation; otherwise the ENGINEER
// gets the task again. Checked MANUAL criteria are recorded as the
// tester's approvals.
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
	t, problem := e.activeTask(st)
	if problem != "" {
		return e.statefileRetry(ctx, st, step, problem, refs)
	}
	var checks []wbs.AcceptanceCheckSpec
	if st.unit.Specification != nil {
		checks = st.unit.Specification.AcceptanceChecks
	}
	if len(t.Met) != len(checks) {
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("%s lists %d acceptance criteria; the planned task has %d", wbs.ActiveTaskFile, len(t.Met), len(checks)),
			refs)
	}
	files, err := e.verifyStatefiles(ctx, st, wbs.ActiveTaskFile)
	if err != nil {
		return execution.StepResult{}, err
	}
	if files[wbs.ActiveTaskFile] == FileUnchanged && (t.Status != wbs.TaskStatusComplete || !t.AllMet()) {
		// Nothing was recorded: not a failed test, so the engineer does
		// not get the task again; the tester is asked to record it.
		format, err := e.statefileFormat(ctx, st.rec.ProjectID)
		if err != nil {
			return execution.StepResult{}, err
		}
		return e.statefileRetry(ctx, st, step, reasonTestNotRecorded(format), refs)
	}
	ev := map[string]any{"work_unit_id": st.unit.ID.String(), "status": t.Status, "met": t.Met, "files": files}
	refs.Detail = ev
	if t.Status != wbs.TaskStatusComplete || !t.AllMet() {
		return e.advance(ctx, st, step, "INCOMPLETE", advanceOpts{Refs: refs, Outcome: "INCOMPLETE"})
	}
	for i, ch := range checks {
		if ch.Verification == wbs.VerifyManual {
			if err := e.d.Repo.ApproveCriterion(ctx, st.rec.ID, st.unit.ID, i, "TESTER", "checked in "+wbs.ActiveTaskFile); err != nil && !errors.Is(err, persistence.ErrConflict) {
				return execution.StepResult{}, translate(err)
			}
		}
	}
	uid := st.unit.ID
	return e.advance(ctx, st, step, "COMPLETE", advanceOpts{Refs: refs, Outcome: "COMPLETE", Tx: toValidating(ctx, e, uid)})
}

// reasonTestNotRecorded is the problem of a test command whose result was
// not written to ACTIVE_TASK.md (the file is byte-for-byte unchanged).
func reasonTestNotRecorded(format worker.ResponseFormat) string {
	sx := syntaxFor(format)
	return "the tester did not record the test result: " + wbs.ActiveTaskFile + " is unchanged since the test command was sent. " +
		"Check " + sx.checked() + " each acceptance criterion that is met and set " + sx.status("COMPLETE") + " when all are met; otherwise leave the status and note what fails " +
		strings.ReplaceAll(sx.notes(), "`", "") + ". A result reported only in the reply is not read"
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

// handleAcceptanceCheck: the PROJECT_MANAGER archived the finished task
// (DONE in TASK_ARCHIVE.md and BACKLOG.md, gone from ACTIVE_TASK.md) and
// ACTIVE_TASK.md holds the next executable task (NEXT) or nothing when
// every task is done (PHASE_COMPLETE). A task found already COMPLETE
// (resumed execution) goes straight to validation (TESTED).
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
	backlog, problem := readTaskFile(st.cwd(), wbs.BacklogFile, false)
	if problem != "" {
		return e.statefileRetry(ctx, st, step, problem, refs)
	}
	active, problem := readTaskFile(st.cwd(), wbs.ActiveTaskFile, true)
	if problem != "" {
		return e.statefileRetry(ctx, st, step, problem, refs)
	}
	files, err := e.verifyStatefiles(ctx, st, wbs.BacklogFile, wbs.ActiveTaskFile, wbs.ArchiveFile)
	if err != nil {
		return execution.StepResult{}, err
	}
	refs.Detail = map[string]any{"files": files}
	byKey := map[string]*wbs.WorkUnit{}
	for _, u := range st.units {
		byKey[wbs.UnitKey(u)] = u
	}
	var txs []func(tx persistence.Transaction) error
	var events []pendingEvent
	done := map[uuid.UUID]bool{}
	for _, u := range st.units {
		done[u.ID] = u.Status == wbs.WorkUnitStatusDONE
	}
	// The finished task.
	if prev := st.unit; prev != nil && prev.Status != wbs.WorkUnitStatusDONE {
		key := wbs.UnitKey(prev)
		if pend == nil {
			// Nothing was sent: the task is still being worked on.
			if t := active.Find(key); t != nil {
				return e.resumeTask(ctx, st, step, prev, t, refs)
			}
		}
		archive, problem := readTaskFile(st.cwd(), wbs.ArchiveFile, false)
		if problem != "" {
			return e.statefileRetry(ctx, st, step, "the project manager did not archive task "+key+": "+problem, refs)
		}
		if t := archive.Find(key); t == nil || t.Status != wbs.TaskStatusDone {
			return e.statefileRetry(ctx, st, step, fmt.Sprintf("task %s is not archived as DONE in %s", key, wbs.ArchiveFile), refs)
		}
		if t := backlog.Find(key); t == nil || t.Status != wbs.TaskStatusDone {
			return e.statefileRetry(ctx, st, step, fmt.Sprintf("task %s is not marked DONE in %s", key, wbs.BacklogFile), refs)
		}
		if active.Find(key) != nil {
			return e.statefileRetry(ctx, st, step, fmt.Sprintf("task %s is archived but still in %s", key, wbs.ActiveTaskFile), refs)
		}
		tx, ev, stop, err := e.finishUnit(ctx, st, step, prev)
		if stop != nil || err != nil {
			if err != nil {
				return execution.StepResult{}, err
			}
			return *stop, nil
		}
		txs, events = append(txs, tx), append(events, ev)
		done[prev.ID] = true
	}
	// Work the statefiles record as done but the overseer has not
	// validated (a recreated project planned from an existing backlog) is
	// adopted one task at a time, through the overseer's own validation.
	deps, err := e.d.Repo.UnitDependencies(ctx, nil, st.plan.ID)
	if err != nil {
		return execution.StepResult{}, err
	}
	if u, t, problem := adoptable(st, backlog, active, done, deps); problem != "" {
		return e.statefileRetry(ctx, st, step, problem, refs)
	} else if u != nil {
		return e.adoptTask(ctx, st, step, u, t, refs, txs, events)
	}
	// The next task.
	switch len(active.Tasks) {
	case 0:
		var remaining []string
		for _, u := range st.units {
			if !done[u.ID] {
				remaining = append(remaining, wbs.UnitKey(u))
			}
		}
		if len(remaining) > 0 {
			return e.statefileRetry(ctx, st, step, fmt.Sprintf("%s holds no task but %d tasks remain (%s)", wbs.ActiveTaskFile, len(remaining), strings.Join(remaining, ", ")), refs)
		}
		return e.advance(ctx, st, step, "PHASE_COMPLETE", advanceOpts{Refs: refs, Outcome: "PHASE_COMPLETE", SetUnit: true,
			Tx: chainTx(txs), Events: events})
	case 1:
	default:
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("%s holds %d tasks; it must hold exactly one", wbs.ActiveTaskFile, len(active.Tasks)), refs)
	}
	t := active.Tasks[0]
	next := byKey[t.Unit.Key]
	switch {
	case next == nil:
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("%s holds task %s, which is not in the active plan", wbs.ActiveTaskFile, t.Unit.Key), refs)
	case done[next.ID]:
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("%s holds task %s, which is already DONE", wbs.ActiveTaskFile, t.Unit.Key), refs)
	}
	for _, d := range deps[next.ID] {
		if !done[d] {
			return e.statefileRetry(ctx, st, step, fmt.Sprintf("task %s was loaded before its dependencies are DONE", t.Unit.Key), refs)
		}
	}
	nid := next.ID
	txs = append(txs, func(tx persistence.Transaction) error { return e.d.Repo.ClaimUnit(ctx, tx, nid, st.rec.ID) })
	events = append(events, pendingEvent{Type: execution.EventUnitClaimed, Payload: map[string]any{"work_unit_id": nid.String(), "sequence": next.Sequence, "name": next.Name}})
	token := "NEXT"
	if t.Status == wbs.TaskStatusComplete && t.AllMet() && next.Status != wbs.WorkUnitStatusPENDING && next.Status != wbs.WorkUnitStatusREADY {
		token = "TESTED"
		txs = append(txs, toValidating(ctx, e, nid))
	}
	return e.advance(ctx, st, step, token, advanceOpts{Refs: refs, Outcome: token, SetUnit: true, UnitID: &nid, Tx: chainTx(txs), Events: events})
}

// adoptable returns the first planned task the statefiles record as DONE
// (in BACKLOG.md and TASK_ARCHIVE.md, not in ACTIVE_TASK.md) that the
// overseer has not marked DONE and whose dependencies it has — or nil. The
// problem is non-empty when the archive cannot be read or the archived
// criteria no longer match the plan.
func adoptable(st *engineState, backlog, active *wbs.TaskDoc, done map[uuid.UUID]bool, deps map[uuid.UUID][]uuid.UUID) (*wbs.WorkUnit, *wbs.Task, string) {
	var archive *wbs.TaskDoc
	for _, u := range st.units {
		key := wbs.UnitKey(u)
		if done[u.ID] || active.Find(key) != nil {
			continue
		}
		if t := backlog.Find(key); t == nil || t.Status != wbs.TaskStatusDone {
			continue
		}
		if archive == nil {
			var problem string
			if archive, problem = readTaskFile(st.cwd(), wbs.ArchiveFile, true); problem != "" {
				return nil, nil, problem
			}
		}
		t := archive.Find(key)
		if t == nil || t.Status != wbs.TaskStatusDone {
			continue
		}
		ready := true
		for _, d := range deps[u.ID] {
			ready = ready && done[d]
		}
		if !ready {
			continue
		}
		var checks []wbs.AcceptanceCheckSpec
		if u.Specification != nil {
			checks = u.Specification.AcceptanceChecks
		}
		if len(t.Met) != len(checks) {
			return nil, nil, fmt.Sprintf("task %s is DONE in %s with %d acceptance criteria; the planned task has %d", key, wbs.ArchiveFile, len(t.Met), len(checks))
		}
		return u, t, ""
	}
	return nil, nil, ""
}

// adoptTask claims a task the statefiles record as done and sends it to
// the overseer's validation (TESTED → VALIDATE); a pass returns through
// acceptance, which marks it DONE. Checked MANUAL criteria in the archive
// are recorded as the tester's approvals.
func (e *Engine) adoptTask(ctx context.Context, st *engineState, step *persistence.StepRecord, u *wbs.WorkUnit, t *wbs.Task,
	refs persistence.StepRefs, txs []func(persistence.Transaction) error, events []pendingEvent) (execution.StepResult, error) {
	if u.Specification != nil {
		for i, ch := range u.Specification.AcceptanceChecks {
			if ch.Verification == wbs.VerifyManual && t.Met[i] {
				if err := e.d.Repo.ApproveCriterion(ctx, st.rec.ID, u.ID, i, "TESTER", "checked in "+wbs.ArchiveFile+" (adopted)"); err != nil && !errors.Is(err, persistence.ErrConflict) {
					return execution.StepResult{}, translate(err)
				}
			}
		}
	}
	uid := u.ID
	txs = append(txs, func(tx persistence.Transaction) error { return e.d.Repo.ClaimUnit(ctx, tx, uid, st.rec.ID) }, toValidating(ctx, e, uid))
	events = append(events, pendingEvent{Type: execution.EventUnitClaimed, Payload: map[string]any{"work_unit_id": uid.String(), "sequence": u.Sequence,
		"name": u.Name, "adopted": true}})
	if refs.Detail == nil {
		refs.Detail = map[string]any{}
	}
	refs.Detail["adopted"] = wbs.UnitKey(u)
	return e.advance(ctx, st, step, "TESTED", advanceOpts{Refs: refs, Outcome: "TESTED", SetUnit: true, UnitID: &uid, Tx: chainTx(txs), Events: events})
}

// resumeTask continues a task that is still active (a resumed execution):
// COMPLETE goes to validation, anything else to the engineer.
func (e *Engine) resumeTask(ctx context.Context, st *engineState, step *persistence.StepRecord, u *wbs.WorkUnit, t *wbs.Task, refs persistence.StepRefs) (execution.StepResult, error) {
	uid := u.ID
	txs := []func(persistence.Transaction) error{func(tx persistence.Transaction) error { return e.d.Repo.ClaimUnit(ctx, tx, uid, st.rec.ID) }}
	token := "NEXT"
	if t.Status == wbs.TaskStatusComplete && t.AllMet() {
		token = "TESTED"
		txs = append(txs, toValidating(ctx, e, uid))
	}
	return e.advance(ctx, st, step, token, advanceOpts{Refs: refs, Outcome: token, SetUnit: true, UnitID: &uid, Tx: chainTx(txs)})
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
