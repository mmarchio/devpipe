package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"overseer/internal/execution"
	"overseer/internal/gitcheckpoint"
	"overseer/internal/persistence"
	"overseer/internal/testintegrity"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// Git-gated TDD workflow (template v5). The overseer drives the
// repository as a state machine around each task:
//
//	QA_TESTS        task/<key> is started from main; the TESTER writes the
//	                task's specification tests ("write the tests for task <K>")
//	QA_TESTS_CHECK  the tests are committed on task/<key>, their MD5
//	                fingerprint is recorded, and sandbox/<key> is opened
//	                at that commit for the coder
//	IMPLEMENT       the ENGINEER works in the sandbox, always with a fresh
//	                context; it never commits and never touches a test
//	VALIDATE        the sandbox must still be on sandbox/<key> at the test
//	                commit with an unchanged test fingerprint — otherwise it
//	                is hard-reset (TAMPERED); a failed validation also
//	                discards the sandbox; a pass is committed and
//	                fast-forwarded into main at acceptance
//
// main therefore only ever holds validated tasks. The fingerprint and the
// test commit live in the QA_TESTS_CHECK step's detail; a requeue or a
// replan of the task makes the QA role write its tests again.

// gitMainBranch is the branch holding validated work.
const gitMainBranch = "main"

// Step detail keys of the git-gated workflow.
const (
	detailTestsMD5    = "tests_md5"
	detailTestsCommit = "tests_commit"
	detailTestFiles   = "test_files"
	detailReuseTests  = "reuse_tests"
	detailIntegrity   = "integrity"
	detailErrorLog    = "error_log"
)

// integrityErrorLog is what a coder whose sandbox was discarded for
// touching the locked tests is told.
const integrityErrorLog = "System Integrity Error: Unauthorized modification of the test suite detected. Code generation aborted."

// GitFlow runs the git-gated workflow's branch operations
// (*gitcheckpoint.Git).
type GitFlow interface {
	CurrentBranch(ctx context.Context, dir string) (string, error)
	Resolve(ctx context.Context, dir, rev string) (string, error)
	StartBranch(ctx context.Context, dir, branch, at string) error
	Discard(ctx context.Context, dir, commit string) error
	Promote(ctx context.Context, dir, main, commit string, drop ...string) error
	FromExecution(ctx context.Context, dir, rev, executionID string) (bool, error)
	ChangedPaths(ctx context.Context, dir, commit string) ([]string, error)
	// Reserved returns the worker's reserved paths (never part of the
	// test fingerprint).
	Reserved() []string
}

// gitGated reports whether the execution runs the git-gated workflow (its
// machine has the QA_TESTS state).
func (st *engineState) gitGated() bool { return st.byKey["QA_TESTS"] != nil }

func taskBranch(u *wbs.WorkUnit) string    { return "task/" + wbs.UnitKey(u) }
func sandboxBranch(u *wbs.WorkUnit) string { return "sandbox/" + wbs.UnitKey(u) }

// testsBaseline is a task's locked QA test suite.
type testsBaseline struct {
	MD5, Commit string
	Files       int
}

// taskRestartedAt is when the active task last started over (a requeue or
// a replan): QA work before it no longer counts.
func (e *Engine) taskRestartedAt(ctx context.Context, st *engineState) (time.Time, error) {
	var at time.Time
	for _, typ := range []execution.EventType{execution.EventUnitRequeued, execution.EventUnitReplanned} {
		ev, err := e.d.Repo.LastEvent(ctx, nil, st.rec.ID, typ, &st.unit.ID)
		if err != nil {
			return at, err
		}
		if ev != nil && ev.At.After(at) {
			at = ev.At
		}
	}
	return at, nil
}

// unitSteps returns the active task's applied steps of stateKey since its
// last restart, oldest first.
func (e *Engine) unitSteps(ctx context.Context, st *engineState, stateKey string) ([]*persistence.StepRecord, error) {
	since, err := e.taskRestartedAt(ctx, st)
	if err != nil {
		return nil, err
	}
	steps, _, err := e.d.Repo.ListSteps(ctx, st.rec.ID, -1, 0)
	if err != nil {
		return nil, err
	}
	var out []*persistence.StepRecord
	for _, s := range steps {
		if s.StateKey == stateKey && s.Status == execution.StepApplied && s.WorkUnitID != nil && *s.WorkUnitID == st.unit.ID && s.IntentAt.After(since) {
			out = append(out, s)
		}
	}
	return out, nil
}

// testsBaseline returns the active task's locked tests (nil when the QA
// role has not committed them since the task last started over).
func (e *Engine) testsBaseline(ctx context.Context, st *engineState) (*testsBaseline, error) {
	if st.unit == nil {
		return nil, nil
	}
	checks, err := e.unitSteps(ctx, st, "QA_TESTS_CHECK")
	if err != nil {
		return nil, err
	}
	for i := len(checks) - 1; i >= 0; i-- {
		d := checks[i].Detail
		md5, _ := d[detailTestsMD5].(string)
		commit, _ := d[detailTestsCommit].(string)
		if md5 == "" || commit == "" {
			continue
		}
		b := &testsBaseline{MD5: md5, Commit: commit}
		if n, ok := d[detailTestFiles].(float64); ok {
			b.Files = int(n)
		}
		return b, nil
	}
	return nil, nil
}

// gitFlowStop is a recovery the git-gated workflow requires.
func (e *Engine) gitFlowStop(ctx context.Context, st *engineState, step *persistence.StepRecord, reason string, ev map[string]any) (*execution.StepResult, error) {
	if ev == nil {
		ev = map[string]any{}
	}
	ev["working_directory"] = st.cwd()
	r, err := e.requireRecovery(ctx, st, step, reason, ev)
	return &r, err
}

// gitFlowReady reports a recovery when the repository cannot run the
// git-gated workflow (no git operations wired, not a repository).
func (e *Engine) gitFlowReady(ctx context.Context, st *engineState, step *persistence.StepRecord) (string, *execution.StepResult, error) {
	if e.d.GitFlow == nil {
		r, err := e.gitFlowStop(ctx, st, step, "the git-gated workflow needs git, but no git operations are configured", nil)
		return "", r, err
	}
	branch, err := e.d.GitFlow.CurrentBranch(ctx, st.cwd())
	if errors.Is(err, gitcheckpoint.ErrNotRepository) {
		r, err := e.gitFlowStop(ctx, st, step, "the git-gated workflow needs a git repository: "+st.cwd()+" is not one (run git init and commit a baseline on main)", nil)
		return "", r, err
	}
	return branch, nil, err
}

// recordHead records the overseer's own branch move as the expected HEAD.
func (e *Engine) recordHead(ctx context.Context, st *engineState, branch, head string) error {
	if e.d.Identities == nil {
		return nil
	}
	_, err := e.d.Identities.AppendExpectedHead(ctx, &persistence.ExpectedHeadRecord{ExecutionID: st.rec.ID, ProjectID: st.rec.ProjectID,
		Head: head, Branch: branch, Source: persistence.HeadFromGitFlow, RecordedAt: e.d.Now().UTC()})
	return err
}

// gitFlowPosition reports whether an observed branch / HEAD the guard
// does not expect is explained by the git-gated workflow: the repository
// is on main or the active task's own branches, at the expected commit
// or at one of this execution's own commits (a crash between a branch
// operation and its record).
func (e *Engine) gitFlowPosition(ctx context.Context, st *engineState, branch, head, expected string) bool {
	if !st.gitGated() || e.d.GitFlow == nil || branch == "" {
		return false
	}
	ok := branch == gitMainBranch
	if st.unit != nil {
		ok = ok || branch == taskBranch(st.unit) || branch == sandboxBranch(st.unit)
	}
	if !ok {
		return false
	}
	if head == expected {
		return true
	}
	own, err := e.d.GitFlow.FromExecution(ctx, st.cwd(), head, st.rec.ID.String())
	return err == nil && own
}

// prepareTaskBranch starts task/<key> from main before the QA role's
// first command for the task. Uncommitted changes on main travel with the
// task (and are committed with its tests); a restarted task's own branches
// are discarded; any other branch needs the operator.
func (e *Engine) prepareTaskBranch(ctx context.Context, st *engineState, step *persistence.StepRecord) (*execution.StepResult, error) {
	if err := e.guardRepository(ctx, st, "task branch"); err != nil {
		return nil, err
	}
	cur, stop, err := e.gitFlowReady(ctx, st, step)
	if stop != nil || err != nil {
		return stop, err
	}
	dir, key := st.cwd(), wbs.UnitKey(st.unit)
	base, err := e.d.GitFlow.Resolve(ctx, dir, "refs/heads/"+gitMainBranch)
	if err != nil {
		return nil, err
	}
	if base == "" {
		return e.gitFlowStop(ctx, st, step, "the git-gated workflow needs a "+gitMainBranch+" branch to start task "+key+" from", nil)
	}
	switch cur {
	case gitMainBranch:
	case taskBranch(st.unit), sandboxBranch(st.unit):
		// The task starts over: its earlier work is discarded.
		if err := e.d.GitFlow.Discard(ctx, dir, "HEAD"); err != nil {
			return nil, err
		}
	default:
		return e.gitFlowStop(ctx, st, step, fmt.Sprintf("the repository is on branch %q; the git-gated workflow starts task %s from %s", cur, key, gitMainBranch),
			map[string]any{"branch": cur})
	}
	if err := e.d.GitFlow.StartBranch(ctx, dir, taskBranch(st.unit), gitMainBranch); err != nil {
		return nil, err
	}
	return nil, e.recordHead(ctx, st, taskBranch(st.unit), base)
}

// prepareQATests runs before the QA_TESTS command: locked tests are
// reused (a resumed execution); the first send of a task applies a
// requested pause, clears the context and starts the task branch.
func (e *Engine) prepareQATests(ctx context.Context, st *engineState, step *persistence.StepRecord, opts advanceOpts) (*execution.StepResult, error) {
	if st.unit == nil {
		r, err := e.requireRecovery(ctx, st, step, "no active task", nil)
		return &r, err
	}
	if b, err := e.testsBaseline(ctx, st); err != nil {
		return nil, err
	} else if b != nil {
		opts.Refs = persistence.StepRefs{Detail: map[string]any{detailReuseTests: b.Commit}}
		r, err := e.advance(ctx, st, step, forwardToken(st.machine, st.cur.Key), opts)
		return &r, err
	}
	sent, err := e.unitSteps(ctx, st, "QA_TESTS")
	if err != nil {
		return nil, err
	}
	if len(sent) == 0 {
		if stop, err := e.pauseBeforeTask(ctx, st, step); stop != nil || err != nil {
			return stop, err
		}
		if stop, err := e.freshSession(ctx, st, step); stop != nil || err != nil {
			return stop, err
		}
		if stop, err := e.prepareTaskBranch(ctx, st, step); stop != nil || err != nil {
			return stop, err
		}
	}
	if st.unit.Status != wbs.WorkUnitStatusEXECUTING {
		if err := e.d.Repo.SetUnitStatus(ctx, nil, st.unit.ID, []wbs.WorkUnitStatus{wbs.WorkUnitStatusINCOMPLETE, wbs.WorkUnitStatusVALIDATING,
			wbs.WorkUnitStatusEXECUTING}, wbs.WorkUnitStatusEXECUTING); err != nil && !errors.Is(err, persistence.ErrConflict) {
			return nil, err
		}
	}
	return nil, nil
}

// handleQATestsCheck commits the QA role's tests on task/<key>, records
// their fingerprint and opens sandbox/<key> for the coder (READY → the
// engineer); a missing, incomplete or empty result sends the command
// again with the problem (RETRY).
func (e *Engine) handleQATestsCheck(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	if st.unit == nil {
		return e.requireRecovery(ctx, st, step, "no active task", nil)
	}
	key := wbs.UnitKey(st.unit)
	sent, err := e.unitSteps(ctx, st, "QA_TESTS")
	if err != nil {
		return execution.StepResult{}, err
	}
	if len(sent) == 0 {
		return e.requireRecovery(ctx, st, step, "no QA test command to check for task "+key, nil)
	}
	qa := sent[len(sent)-1]
	if _, ok := qa.Detail[detailReuseTests]; ok {
		b, err := e.testsBaseline(ctx, st)
		if err != nil {
			return execution.StepResult{}, err
		}
		if b == nil {
			return e.requireRecovery(ctx, st, step, "task "+key+"'s locked tests are missing", nil)
		}
		if stop, err := e.openSandbox(ctx, st, step, b.Commit); stop != nil || err != nil {
			return derefStop(stop, err)
		}
		return e.advance(ctx, st, step, "READY", advanceOpts{Outcome: "READY", Refs: persistence.StepRefs{Detail: map[string]any{
			detailTestsMD5: b.MD5, detailTestsCommit: b.Commit, detailTestFiles: b.Files, detailReuseTests: b.Commit}}})
	}
	if qa.DispatchID == nil {
		return e.requireRecovery(ctx, st, step, "no QA test dispatch to check for task "+key, nil)
	}
	refs := persistence.StepRefs{DispatchID: qa.DispatchID}
	if stop, err := e.commandFailed(ctx, st, step, *qa.DispatchID, "the QA test command", refs); stop != nil || err != nil {
		return derefStop(stop, err)
	}
	rep, err := e.mcpReport(ctx, *qa.DispatchID, persistence.ReportImplement)
	if err != nil {
		return execution.StepResult{}, err
	}
	if rep == nil {
		return e.resultMissing(ctx, st, step, persistence.ReportImplement, refs)
	}
	switch rep.Status {
	case persistence.ReportComplete:
	case persistence.ReportBlocked:
		return e.requireRecovery(ctx, st, step, "the tester is blocked on task "+key+": "+rep.Summary, map[string]any{"report_id": rep.ID.String()})
	default:
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("the tests for task %s were not finished (%s: %s); write them and submit status COMPLETE",
			key, rep.Status, rep.Summary), refs)
	}

	if err := e.guardRepository(ctx, st, "test commit"); err != nil {
		return execution.StepResult{}, err
	}
	cur, stop, err := e.gitFlowReady(ctx, st, step)
	if stop != nil || err != nil {
		return derefStop(stop, err)
	}
	dir := st.cwd()
	if cur != taskBranch(st.unit) && cur != sandboxBranch(st.unit) {
		return e.requireRecovery(ctx, st, step, fmt.Sprintf("the repository is on branch %q, not %s, after the QA tests of task %s", cur, taskBranch(st.unit), key),
			map[string]any{"branch": cur})
	}
	ckey := execution.CheckpointKey(st.rec.ID, execution.RepoCheckpointTests, st.unit.ID, int(qa.Seq))
	found, err := e.d.Checkpointer.FindAllByKey(ctx, dir, ckey)
	if err != nil {
		return execution.StepResult{}, err
	}
	commit := ""
	if len(found) > 0 {
		commit = found[0].Commit // committed before a crash: adopted
	} else {
		changed, err := e.d.GitFlow.ChangedPaths(ctx, dir, "HEAD")
		if err != nil {
			return execution.StepResult{}, err
		}
		if len(testPaths(changed)) == 0 {
			return e.statefileRetry(ctx, st, step, fmt.Sprintf("no test file was written for task %s: write its specification tests (%s) and submit again",
				key, strings.Join(testintegrity.Patterns, ", ")), refs)
		}
		c, err := e.d.Checkpointer.Create(ctx, dir, execution.CommitMessage("qa: generate specification test suite for "+key, ckey, st.rec.ID))
		if err != nil {
			return execution.StepResult{}, err
		}
		commit = c.Commit
		if err := e.recordHead(ctx, st, cur, commit); err != nil {
			return execution.StepResult{}, err
		}
	}
	fp, err := testintegrity.Compute(dir, e.d.GitFlow.Reserved())
	if err != nil {
		return execution.StepResult{}, err
	}
	if stop, err := e.openSandbox(ctx, st, step, commit); stop != nil || err != nil {
		return derefStop(stop, err)
	}
	refs.Detail = map[string]any{detailTestsMD5: fp.MD5, detailTestsCommit: commit, detailTestFiles: len(fp.Files), "report_id": rep.ID.String()}
	return e.advance(ctx, st, step, "READY", advanceOpts{Outcome: "READY", Refs: refs, Events: []pendingEvent{{Type: execution.EventTestsBaselined,
		Payload: map[string]any{"work_unit_id": st.unit.ID.String(), "commit": commit, "md5": fp.MD5, "test_files": len(fp.Files), "branch": taskBranch(st.unit)}}}})
}

// openSandbox checks out sandbox/<key> at the test commit (idempotent).
func (e *Engine) openSandbox(ctx context.Context, st *engineState, step *persistence.StepRecord, commit string) (*execution.StepResult, error) {
	cur, stop, err := e.gitFlowReady(ctx, st, step)
	if stop != nil || err != nil {
		return stop, err
	}
	dir, sb := st.cwd(), sandboxBranch(st.unit)
	if cur == sb {
		if head, err := e.d.GitFlow.Resolve(ctx, dir, "HEAD"); err != nil || head == commit {
			return nil, err
		}
	}
	if err := e.d.GitFlow.StartBranch(ctx, dir, sb, commit); err != nil {
		return nil, err
	}
	return nil, e.recordHead(ctx, st, sb, commit)
}

// prepareImplement runs before every IMPLEMENT command of the git-gated
// workflow: the coder works in its sandbox at the locked tests, always
// with a fresh context.
func (e *Engine) prepareImplement(ctx context.Context, st *engineState, step *persistence.StepRecord) (*execution.StepResult, error) {
	b, err := e.testsBaseline(ctx, st)
	if err != nil {
		return nil, err
	}
	if b == nil {
		r, err := e.requireRecovery(ctx, st, step, "task "+wbs.UnitKey(st.unit)+" has no locked QA tests to implement against", nil)
		return &r, err
	}
	if stop, err := e.openSandbox(ctx, st, step, b.Commit); stop != nil || err != nil {
		return stop, err
	}
	return e.freshSession(ctx, st, step)
}

// checkTestIntegrity runs before the git-gated workflow's validation: the
// sandbox must be on sandbox/<key> at the test commit with the locked
// test fingerprint. Otherwise the sandbox is hard-reset to the test commit
// and the coder starts again (TAMPERED, a fix attempt).
func (e *Engine) checkTestIntegrity(ctx context.Context, st *engineState, step *persistence.StepRecord) (*execution.StepResult, error) {
	key := wbs.UnitKey(st.unit)
	b, err := e.testsBaseline(ctx, st)
	if err != nil {
		return nil, err
	}
	if b == nil {
		r, err := e.requireRecovery(ctx, st, step, "task "+key+" has no locked QA tests to validate against", nil)
		return &r, err
	}
	if err := e.guardRepository(ctx, st, "test integrity check"); err != nil {
		return nil, err
	}
	cur, stop, err := e.gitFlowReady(ctx, st, step)
	if stop != nil || err != nil {
		return stop, err
	}
	dir := st.cwd()
	head, err := e.d.GitFlow.Resolve(ctx, dir, "HEAD")
	if err != nil {
		return nil, err
	}
	fp, err := testintegrity.Compute(dir, e.d.GitFlow.Reserved())
	if err != nil {
		return nil, err
	}
	var why string
	var changed []string
	switch {
	case cur != sandboxBranch(st.unit):
		why = fmt.Sprintf("the repository was switched to branch %q", cur)
	case head != b.Commit:
		why = fmt.Sprintf("HEAD moved to %s (the coder committed); the sandbox must stay at the test commit %s", short(head), short(b.Commit))
	case fp.MD5 != b.MD5:
		paths, err := e.d.GitFlow.ChangedPaths(ctx, dir, b.Commit)
		if err != nil {
			return nil, err
		}
		changed = testPaths(paths)
		why = "the locked test suite changed: " + capList(changed, 20)
	default:
		return nil, nil
	}
	if cur != sandboxBranch(st.unit) {
		if err := e.d.GitFlow.Discard(ctx, dir, "HEAD"); err != nil {
			return nil, err
		}
	}
	if err := e.discardSandbox(ctx, st, b.Commit); err != nil {
		return nil, err
	}
	ev := map[string]any{"work_unit_id": st.unit.ID.String(), "reason": why, "changed_tests": changed, "md5": fp.MD5, "baseline_md5": b.MD5}
	r, err := e.advance(ctx, st, step, "TAMPERED", advanceOpts{Outcome: "TAMPERED", Refs: persistence.StepRefs{Detail: map[string]any{
		detailIntegrity: "TAMPERED", detailErrorLog: integrityErrorLog + " " + why + "."}}, Events: []pendingEvent{{Type: execution.EventTestsTampered, Payload: ev}}})
	return &r, err
}

// discardSandbox hard-resets sandbox/<key> to the test commit: the next
// attempt starts from a clean checkout of the locked tests.
func (e *Engine) discardSandbox(ctx context.Context, st *engineState, commit string) error {
	dir, sb := st.cwd(), sandboxBranch(st.unit)
	if err := e.d.GitFlow.StartBranch(ctx, dir, sb, commit); err != nil {
		// A dirty tree can block the switch: reset in place, then switch.
		if derr := e.d.GitFlow.Discard(ctx, dir, "HEAD"); derr != nil {
			return derr
		}
		if err := e.d.GitFlow.StartBranch(ctx, dir, sb, commit); err != nil {
			return err
		}
	}
	if err := e.d.GitFlow.Discard(ctx, dir, commit); err != nil {
		return err
	}
	return e.recordHead(ctx, st, sb, commit)
}

// failedAttempt discards a failed attempt's sandbox (git-gated workflow)
// and returns the error log the next attempt is given.
func (e *Engine) failedAttempt(ctx context.Context, st *engineState, run *persistence.ValidationRunRecord) (string, error) {
	b, err := e.testsBaseline(ctx, st)
	if err != nil || b == nil {
		return "", err
	}
	if err := e.discardSandbox(ctx, st, b.Commit); err != nil {
		return "", err
	}
	log := validationErrorLog(run)
	return log, e.event(ctx, nil, st, execution.EventSandboxReset, map[string]any{"work_unit_id": st.unit.ID.String(), "run_id": run.ID.String(),
		"commit": b.Commit})
}

// validationErrorLog renders a run's failing commands and their capped
// output, and its unmet acceptance criteria.
func validationErrorLog(run *persistence.ValidationRunRecord) string {
	var b strings.Builder
	for _, r := range run.Records {
		if r.Outcome == execution.ValidationPass {
			continue
		}
		fmt.Fprintf(&b, "$ %s", strings.TrimSpace(r.Program+" "+strings.Join(r.Args, " ")))
		if r.ExitCode != nil {
			fmt.Fprintf(&b, " (exit %d)", *r.ExitCode)
		}
		if r.TimedOut {
			b.WriteString(" (timed out)")
		}
		b.WriteString("\n" + tail(r.Stdout+"\n"+r.Stderr, 4000) + "\n")
	}
	if run.Acceptance != nil {
		for _, v := range run.Acceptance.Verdicts {
			if v.Verdict != execution.VerdictPass {
				fmt.Fprintf(&b, "acceptance check not met: %s (%s)\n", v.Text, v.Verdict)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// promoteTask fast-forwards main to the task's validated checkpoint
// commit and deletes task/<key> and sandbox/<key>.
func (e *Engine) promoteTask(ctx context.Context, st *engineState, u *wbs.WorkUnit, commit string) (string, error) {
	if e.d.GitFlow == nil {
		return "the git-gated workflow needs git, but no git operations are configured", nil
	}
	err := e.d.GitFlow.Promote(ctx, st.cwd(), gitMainBranch, commit, taskBranch(u), sandboxBranch(u))
	if errors.Is(err, gitcheckpoint.ErrNotFastForward) {
		return fmt.Sprintf("%s moved since task %s branched from it: its validated commit %s cannot be fast-forwarded (%v)",
			gitMainBranch, wbs.UnitKey(u), short(commit), err), nil
	}
	if err != nil {
		return "", err
	}
	if err := e.recordHead(ctx, st, gitMainBranch, commit); err != nil {
		return "", err
	}
	return "", e.event(ctx, nil, st, execution.EventTaskPromoted, map[string]any{"work_unit_id": u.ID.String(), "commit": commit, "branch": gitMainBranch})
}

// implementRules is appended to the git-gated workflow's IMPLEMENT
// command: the locked tests, the sandbox rules and, after a discarded
// attempt, why it was discarded.
func (e *Engine) implementRules(ctx context.Context, st *engineState) (string, error) {
	if st.unit == nil {
		return "", nil
	}
	b, err := e.testsBaseline(ctx, st)
	if err != nil || b == nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "\n\nYou work in the sandbox branch %s, at the QA role's test commit %s (%d test files). The test suite is locked: "+
		"do not create, edit, rename or delete any test file (%s), do not commit, and do not switch branches. Make the locked tests and the "+
		"task's validation pass by changing the code only. The overseer verifies the test suite before validating; a changed suite, a "+
		"commit or a failed validation discards every change in the sandbox and you start again from the test commit.",
		sandboxBranch(st.unit), short(b.Commit), b.Files, strings.Join(testintegrity.Patterns, ", "))
	vals, err := e.unitSteps(ctx, st, "VALIDATE")
	if err != nil {
		return "", err
	}
	if n := len(vals); n > 0 {
		if log, _ := vals[n-1].Detail[detailErrorLog].(string); log != "" {
			out.WriteString("\n\nThe previous attempt was discarded:\n\n```text\n" + feedback(log) + "\n```")
		}
	}
	return out.String(), nil
}

// testPaths keeps the test files of paths, sorted.
func testPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if testintegrity.IsTest(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func capList(items []string, n int) string {
	if len(items) == 0 {
		return "(a test file was added, removed or changed outside the tracked paths)"
	}
	if len(items) > n {
		return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
	}
	return strings.Join(items, ", ")
}

// derefStop turns a (stop, err) pair into a handler result.
func derefStop(stop *execution.StepResult, err error) (execution.StepResult, error) {
	if err != nil {
		return execution.StepResult{}, err
	}
	return *stop, nil
}
