package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// The statefile workflow's results (Phase 19). The team reports through
// the overseer MCP server; no statefile is read or written to decide a
// transition. Each command carries a capability token; its check state
// reads the result submitted for the command's dispatch (task_reports).
// Overseer picks the next task itself, in plan order once its
// dependencies are DONE.

// mcpInstructions is appended to every command: the token, and what
// result the command expects.
func mcpInstructions(token, stateKey string) string {
	var b strings.Builder
	b.WriteString("\n\nThis project reports through the overseer MCP server: read the task and context with its tools " +
		"(get_current_state, get_task, get_context, get_role, get_execution_history) — do not read or write statefiles — and pass token " +
		token + " to every tool call. Give every reporting call a new idempotency_key.\n\n")
	switch stateKey {
	case "TEST":
		b.WriteString("When the tests are done, call submit_result with status COMPLETE when every acceptance check is met or INCOMPLETE " +
			"otherwise, criteria_met with one true/false per acceptance check of get_task (in order), a summary, and the test output as evidence.")
	case "PLAN":
		b.WriteString("When the phase is planned, call submit_result with status COMPLETE and plan: {summary, units: [{key, name, kind: ATOMIC, " +
			"objective, instructions, acceptance_checks: [{text, verification: COMMAND|TEST|MANUAL, command: {program, args}, tests}], " +
			"validation: [{program, args}], depends_on: [keys]}]} — every task of the phase, in order.")
	case "REPLAN":
		b.WriteString("Call submit_result with status COMPLETE and plan: {units: [the replanned task]} — exactly this task, with its key, kind " +
			"and depends_on unchanged.")
	case "ACCEPTANCE":
		b.WriteString("Review the finished task (get_task, get_execution_history). Call submit_result with status COMPLETE to accept and " +
			"archive it, or INCOMPLETE with the reason. With no finished task, submit COMPLETE. The overseer loads the next task.")
	case "IMPLEMENT", "CONTINUE":
		b.WriteString("When you stop, call submit_result with status COMPLETE (implemented), INCOMPLETE, BLOCKED or FAILED and a summary " +
			"of the changes. Use report_progress for long work and report_blocker when you cannot continue.")
	case "ANALYZE_TASK":
		b.WriteString("Call submit_result with status COMPLETE, a summary, and evidence: {fixable: true|false, findings: [...], " +
			"optimizations: [...]}. Do not change anything.")
	case "FIX_TASK":
		b.WriteString("When done, call submit_result with status COMPLETE and a summary; when the task definition changed, include " +
			"plan: {units: [the rewritten task]} with its key, kind and depends_on unchanged.")
	default:
		b.WriteString("This command takes no result.")
		return b.String() + " Reply when it is done."
	}
	return b.String() + " Then reply with a one-line summary: a result given only in the reply is not read."
}

// commandPrompt creates the worker prompt of a command: the command
// carries a new capability token, stored with the prompt (the token is
// bound to it).
func (e *Engine) commandPrompt(ctx context.Context, st *engineState, cmd, stateKey, roleKey string) (*compression.WorkerPrompt, error) {
	if e.mcp == nil {
		return nil, fmt.Errorf("%w: the MCP repository is not configured", ErrInfrastructure)
	}
	token, err := newMCPToken()
	if err != nil {
		return nil, err
	}
	cmd += mcpInstructions(token, stateKey)
	wp, err := compression.WorkerPromptFromCommand(cmd, e.d.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("command %q: %w", cmd, err)
	}
	if err := e.d.CommandPrompts.CreateWorkerPrompt(ctx, st.rec.ProjectID, &wp); err != nil {
		return nil, err
	}
	if roleKey == "" {
		roleKey = "TEAM"
	}
	if err := e.mcp.storeToken(ctx, nil, st, wp.ID, roleKey, stateKey, token); err != nil {
		return nil, err
	}
	return &wp, nil
}

// promptHasToken reports whether a command prompt carries a capability
// token (one prepared for the statefiles, before they were retired, does
// not and is prepared again).
func (e *Engine) promptHasToken(ctx context.Context, promptID uuid.UUID) (bool, error) {
	if e.mcp == nil {
		return false, nil
	}
	return e.mcp.repo.PromptHasToken(ctx, nil, promptID)
}

// mcpReport returns the result submitted for a dispatch (nil when none).
func (e *Engine) mcpReport(ctx context.Context, dispatchID uuid.UUID, kind string) (*persistence.TaskReportRecord, error) {
	if e.mcp == nil {
		return nil, nil
	}
	r, err := e.mcp.repo.GetTaskReport(ctx, nil, dispatchID, kind)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, nil
	}
	return r, err
}

// resultMissing: the command's dispatch ended without submit_result. The
// command is resent with the problem (bounded).
func (e *Engine) resultMissing(ctx context.Context, st *engineState, step *persistence.StepRecord, kind string, refs persistence.StepRefs) (execution.StepResult, error) {
	payload := map[string]any{"kind": kind, "state": st.cur.Key}
	if refs.DispatchID != nil {
		payload["dispatch_id"] = refs.DispatchID.String()
	}
	if err := e.event(ctx, nil, st, execution.EventMCPResultMissing, payload); err != nil {
		return execution.StepResult{}, err
	}
	return e.statefileRetry(ctx, st, step, "no "+kind+" result was submitted for the command: call submit_result before replying", refs)
}

// testResult: the tester's TEST result decides COMPLETE (every acceptance
// check met → overseer validation) or INCOMPLETE (back to the engineer); a
// blocked tester needs the operator.
func (e *Engine) testResult(ctx context.Context, st *engineState, step *persistence.StepRecord, pend uuid.UUID, refs persistence.StepRefs) (execution.StepResult, error) {
	rep, err := e.mcpReport(ctx, pend, persistence.ReportTest)
	if err != nil {
		return execution.StepResult{}, err
	}
	if rep == nil {
		return e.resultMissing(ctx, st, step, persistence.ReportTest, refs)
	}
	var checks []wbs.AcceptanceCheckSpec
	if st.unit.Specification != nil {
		checks = st.unit.Specification.AcceptanceChecks
	}
	if len(rep.CriteriaMet) != len(checks) {
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("the submitted test result has %d criteria; task %s has %d", len(rep.CriteriaMet),
			wbs.UnitKey(st.unit), len(checks)), refs)
	}
	refs.Detail = map[string]any{"work_unit_id": st.unit.ID.String(), "status": rep.Status, "met": rep.CriteriaMet, "report_id": rep.ID.String()}
	allMet := true
	for _, m := range rep.CriteriaMet {
		allMet = allMet && m
	}
	switch {
	case rep.Status == persistence.ReportBlocked:
		return e.requireRecovery(ctx, st, step, "the tester is blocked on task "+wbs.UnitKey(st.unit)+": "+rep.Summary, refs.Detail)
	case rep.Status != persistence.ReportComplete || !allMet:
		return e.advance(ctx, st, step, "INCOMPLETE", advanceOpts{Refs: refs, Outcome: "INCOMPLETE"})
	}
	for i, ch := range checks {
		if ch.Verification == wbs.VerifyManual {
			if err := e.d.Repo.ApproveCriterion(ctx, st.rec.ID, st.unit.ID, i, "TESTER", "met in the submitted test result"); err != nil && !errors.Is(err, persistence.ErrConflict) {
				return execution.StepResult{}, translate(err)
			}
		}
	}
	uid := st.unit.ID
	return e.advance(ctx, st, step, "COMPLETE", advanceOpts{Refs: refs, Outcome: "COMPLETE", Tx: toValidating(ctx, e, uid)})
}

// implementResult checks the engineer's IMPLEMENT result after a reply:
// nil to proceed to the test; a missing result resumes the work (CONTINUE
// asks for the result), a blocked engineer needs the operator.
func (e *Engine) implementResult(ctx context.Context, st *engineState, step *persistence.StepRecord, pend uuid.UUID, refs persistence.StepRefs) (*execution.StepResult, error) {
	rep, err := e.mcpReport(ctx, pend, persistence.ReportImplement)
	if err != nil {
		return nil, err
	}
	if rep == nil {
		if err := e.event(ctx, nil, st, execution.EventMCPResultMissing, map[string]any{"kind": persistence.ReportImplement, "state": st.cur.Key,
			"dispatch_id": pend.String()}); err != nil {
			return nil, err
		}
		refs.Detail = map[string]any{detailStatefileProblem: "no IMPLEMENT result was submitted: call submit_result before replying"}
		r, err := e.advance(ctx, st, step, "INTERRUPTED", advanceOpts{Refs: refs, Outcome: "INTERRUPTED"})
		return &r, err
	}
	if rep.Status == persistence.ReportBlocked {
		r, err := e.requireRecovery(ctx, st, step, "the engineer is blocked on task "+wbs.UnitKey(st.unit)+": "+rep.Summary,
			map[string]any{"report_id": rep.ID.String()})
		return &r, err
	}
	return nil, nil
}

// acceptanceResult: the project manager's ACCEPTANCE result accepts the
// finished task, which is marked DONE (after its passing validation) and
// archived; Overseer then claims the next task in plan order whose
// dependencies are DONE (NEXT), or completes the phase.
func (e *Engine) acceptanceResult(ctx context.Context, st *engineState, step *persistence.StepRecord, pend *uuid.UUID, refs persistence.StepRefs) (execution.StepResult, error) {
	var txs []func(tx persistence.Transaction) error
	var events []pendingEvent
	done := map[uuid.UUID]bool{}
	for _, u := range st.units {
		done[u.ID] = u.Status == wbs.WorkUnitStatusDONE
	}
	if prev := st.unit; prev != nil && prev.Status != wbs.WorkUnitStatusDONE {
		uid := prev.ID
		if pend == nil {
			// Nothing was sent (a resumed execution): the task continues.
			txs := []func(persistence.Transaction) error{func(tx persistence.Transaction) error { return e.d.Repo.ClaimUnit(ctx, tx, uid, st.rec.ID) }}
			token := "NEXT"
			if prev.Status == wbs.WorkUnitStatusVALIDATING {
				token = "TESTED"
			}
			return e.advance(ctx, st, step, token, advanceOpts{Refs: refs, Outcome: token, SetUnit: true, UnitID: &uid, Tx: chainTx(txs)})
		}
		rep, err := e.mcpReport(ctx, *pend, persistence.ReportAcceptance)
		if err != nil {
			return execution.StepResult{}, err
		}
		if rep == nil {
			return e.resultMissing(ctx, st, step, persistence.ReportAcceptance, refs)
		}
		key := wbs.UnitKey(prev)
		if rep.Status != persistence.ReportComplete {
			return e.statefileRetry(ctx, st, step, fmt.Sprintf("task %s was not accepted (%s: %s); submit an ACCEPTANCE result with status COMPLETE once it is",
				key, rep.Status, rep.Summary), refs)
		}
		tx, ev, stop, err := e.finishUnit(ctx, st, step, prev)
		if stop != nil || err != nil {
			if err != nil {
				return execution.StepResult{}, err
			}
			return *stop, nil
		}
		dispatch := *pend
		archive := func(tx persistence.Transaction) error {
			return e.mcp.repo.ArchiveUnit(ctx, tx, uid, &dispatch, e.d.Now())
		}
		txs, events = append(txs, tx, archive), append(events, ev)
		done[uid] = true
		refs.Detail = map[string]any{"archived": key, "report_id": rep.ID.String()}
	}
	deps, err := e.d.Repo.UnitDependencies(ctx, nil, st.plan.ID)
	if err != nil {
		return execution.StepResult{}, err
	}
	units := append([]*wbs.WorkUnit(nil), st.units...)
	sort.SliceStable(units, func(i, j int) bool { return units[i].Sequence < units[j].Sequence })
	var next *wbs.WorkUnit
	var waiting []string
	for _, u := range units {
		if done[u.ID] {
			continue
		}
		ready := true
		for _, d := range deps[u.ID] {
			ready = ready && done[d]
		}
		if ready {
			next = u
			break
		}
		waiting = append(waiting, wbs.UnitKey(u))
	}
	if next == nil {
		if len(waiting) > 0 {
			return e.requireRecovery(ctx, st, step, "no task can start: "+strings.Join(waiting, ", ")+" wait on tasks that are not DONE",
				map[string]any{"waiting": waiting})
		}
		return e.advance(ctx, st, step, "PHASE_COMPLETE", advanceOpts{Refs: refs, Outcome: "PHASE_COMPLETE", SetUnit: true,
			Tx: chainTx(txs), Events: events})
	}
	nid := next.ID
	txs = append(txs, func(tx persistence.Transaction) error { return e.d.Repo.ClaimUnit(ctx, tx, nid, st.rec.ID) })
	events = append(events, pendingEvent{Type: execution.EventUnitClaimed, Payload: map[string]any{"work_unit_id": nid.String(), "sequence": next.Sequence, "name": next.Name}})
	return e.advance(ctx, st, step, "NEXT", advanceOpts{Refs: refs, Outcome: "NEXT", SetUnit: true, UnitID: &nid, Tx: chainTx(txs), Events: events})
}

// planCandidate reads the planner's PLAN result as the plan candidate.
func (e *Engine) planCandidate(ctx context.Context, st *engineState, dispatchID uuid.UUID, mode string, rec *persistence.PlanCandidateRecord,
	reject func(code, msg string)) (*wbs.PlanCandidate, error) {
	d, err := e.d.Dispatches.Get(ctx, st.rec.ProjectID, dispatchID)
	if err != nil {
		return nil, err
	}
	if we := dispatchErr(d); we != nil {
		reject("NOT_COMPLETE", "the plan command did not complete: "+we.Error())
		return nil, nil
	}
	rep, err := e.mcpReport(ctx, dispatchID, persistence.ReportPlan)
	if err != nil {
		return nil, err
	}
	switch {
	case rep == nil:
		reject("MISSING_PLAN", "no plan was submitted: call submit_result with status COMPLETE and the plan")
		return nil, nil
	case rep.Status != persistence.ReportComplete:
		reject("NOT_COMPLETE", "the planner reported "+rep.Status+": "+rep.Summary)
		return nil, nil
	}
	raw, err := json.Marshal(rep.Evidence["plan"])
	if err != nil {
		return nil, err
	}
	pc, err := wbs.DecodePlanCandidate(raw)
	if err != nil {
		reject("MALFORMED_PLAN", err.Error())
		return nil, nil
	}
	other, err := e.otherPhaseKeys(ctx, st)
	if err != nil {
		return nil, err
	}
	pc = withoutOtherPhases(pc, other)
	if mode == wbs.PlanKindAmendment {
		pc = withoutCarried(pc, st.units)
	}
	if rec.Payload, err = json.Marshal(pc); err != nil {
		return nil, err
	}
	return pc, nil
}

// replannedUnit reads the rewritten task of a REPLAN or FIX_TASK result
// for the dispatch. ok is false when the result carries no task (a fix
// that left the definition as it was); problem is non-empty when the
// result is missing or does not hold exactly the current task.
func (e *Engine) replannedUnit(ctx context.Context, st *engineState, dispatchID *uuid.UUID) (u wbs.CandidateUnit, ok bool, problem string, err error) {
	key := wbs.UnitKey(st.unit)
	if dispatchID == nil {
		return u, false, "no replan command to read", nil
	}
	rep, err := e.mcpReport(ctx, *dispatchID, persistence.ReportPlan)
	if err != nil || rep == nil {
		return u, false, "no replanned task " + key + " was submitted: call submit_result with the plan", err
	}
	if rep.Status != persistence.ReportComplete {
		return u, false, "the replan was reported " + rep.Status + ": " + rep.Summary, nil
	}
	p, found := rep.Evidence["plan"]
	if !found {
		return u, false, "", nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return u, false, "", err
	}
	pc, derr := wbs.DecodePlanCandidate(raw)
	if derr != nil {
		return u, false, "the submitted plan is malformed: " + derr.Error(), nil
	}
	if len(pc.Units) != 1 || pc.Units[0].Key != key {
		return u, false, fmt.Sprintf("the submitted plan must hold exactly task %s", key), nil
	}
	return pc.Units[0], true, "", nil
}

// analysisResult reads the architect's ANALYSIS result.
func (e *Engine) analysisResult(ctx context.Context, dispatchID uuid.UUID) (*wbs.TaskAnalysis, string, error) {
	rep, err := e.mcpReport(ctx, dispatchID, persistence.ReportAnalysis)
	if err != nil || rep == nil {
		return nil, "the architect did not submit the analysis (submit_result)", err
	}
	a := &wbs.TaskAnalysis{}
	a.Fixable, _ = rep.Evidence["fixable"].(bool)
	a.Findings, a.Optimizations = stringList(rep.Evidence["findings"]), stringList(rep.Evidence["optimizations"])
	return a, "", nil
}

// pendingContextReset reports a context reset the team requested
// (request_context_reset) after the active worker session was opened: the
// next command then starts in a new session. The engine applies it at the
// command boundary, so no session is replaced under a running dispatch.
func (e *Engine) pendingContextReset(ctx context.Context, st *engineState) (bool, error) {
	if e.d.Worker == nil {
		return false, nil
	}
	ev, err := e.d.Repo.LastEvent(ctx, nil, st.rec.ID, execution.EventMCPContextResetRequest, nil)
	if err != nil || ev == nil {
		return false, err
	}
	sess, err := e.d.Worker.ActiveSession(ctx, st.rec.ProjectID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoActiveSession) || (err == nil && sess == nil) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return ev.At.After(sess.CreatedAt), nil
}
