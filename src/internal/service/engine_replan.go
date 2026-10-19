package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
	"overseer/internal/worker"
)

// ---------------------------------------------------------------------------
// Replanning a task the overseer's validation keeps failing the same way.
// Repeating the implementation cannot help when the failure does not
// change (a test that depends on the environment, an impossible
// criterion): after maxIdenticalFailures identical failed validations the
// ARCHITECT replans the task with the failure in hand, the overseer
// validates the rewritten task like a plan and replaces the stored
// definition, and the ENGINEER implements the replanned task.

// maxIdenticalFailures is how many consecutive identical failed
// validations of a task trigger its replan.
const maxIdenticalFailures = 3

// reasonIdenticalFailuresPrefix starts the recovery reason when the
// workflow has no replan state.
const reasonIdenticalFailuresPrefix = "the same validation failure repeated"

// failureLineRE marks the lines that identify a failure: failed tests,
// test assertions, compiler errors, panics, errors.
var failureLineRE = regexp.MustCompile(`--- FAIL|^FAIL\b|_test\.go:\d+:|\.go:\d+:\d+:|^panic:|[Ee]rror`)

// noiseRE strips what varies between identical failures: durations,
// cache markers, addresses, temporary paths.
var noiseRE = regexp.MustCompile(`\(\d+(\.\d+)?m?s\)|\b\d+(\.\d+)?m?s\b|\(cached\)|0x[0-9a-fA-F]+|/tmp/[^\s:]+|goroutine \d+`)

// failureLines returns a failed command's identifying lines (the last
// non-blank lines when none matches).
func failureLines(r execution.ValidationRecord) []string {
	var all, out []string
	for _, l := range strings.Split(r.Stdout+"\n"+r.Stderr, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		all = append(all, l)
		if failureLineRE.MatchString(strings.TrimSpace(l)) {
			out = append(out, l)
		}
	}
	if len(out) == 0 && len(all) > 0 {
		out = all[max(0, len(all)-10):]
	}
	return out
}

// failureSignature identifies a failed validation run: its failed
// commands and their failure lines, with the varying noise removed.
func failureSignature(run *persistence.ValidationRunRecord) string {
	var parts []string
	for _, r := range run.Records {
		if r.Outcome == execution.ValidationPass {
			continue
		}
		parts = append(parts, r.Program+" "+strings.Join(r.Args, " ")+" "+string(r.Outcome))
		for _, l := range failureLines(r) {
			parts = append(parts, noiseRE.ReplaceAllString(strings.TrimSpace(l), ""))
		}
	}
	if run.Acceptance != nil {
		for _, v := range run.Acceptance.Verdicts {
			if v.Verdict == execution.VerdictFail {
				parts = append(parts, "criterion "+strconv.Itoa(v.Index)+" "+v.Text)
			}
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// identicalFailures counts the unit's most recent consecutive failed
// validations with the same failure, since its last replan (or a
// resolved identical-failure recovery).
func (e *Engine) identicalFailures(ctx context.Context, st *engineState, uid uuid.UUID) (int, *persistence.ValidationRunRecord, error) {
	runs, err := e.d.Repo.ListValidationRuns(ctx, st.rec.ID)
	if err != nil {
		return 0, nil, err
	}
	reset, err := e.limitResetAt(ctx, st, LimitIdenticalFailures)
	if err != nil {
		return 0, nil, err
	}
	n, sig := 0, ""
	var latest *persistence.ValidationRunRecord
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.Scope != execution.ScopeUnit || r.WorkUnitID == nil || *r.WorkUnitID != uid {
			continue
		}
		if !r.StartedAt.After(reset) || r.Status != string(execution.ValidationFail) {
			break
		}
		s := failureSignature(r)
		if latest == nil {
			latest, sig = r, s
		} else if s != sig {
			break
		}
		n++
	}
	return n, latest, nil
}

// validationUser describes the user the overseer runs validation as.
func validationUser() string {
	uid := os.Geteuid()
	if uid == 0 {
		return "root (uid 0), which ignores file permissions"
	}
	return "uid " + strconv.Itoa(uid)
}

// replanContext is what the replan command carries: the repeated failure,
// where and how the overseer validates, and what the architect may change.
func (e *Engine) replanContext(ctx context.Context, st *engineState, format worker.ResponseFormat) (string, error) {
	if st.unit == nil {
		return "", nil
	}
	n, run, err := e.identicalFailures(ctx, st, st.unit.ID)
	if err != nil {
		return "", err
	}
	key := wbs.UnitKey(st.unit)
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nTask %s failed the overseer's validation the same way %d times; implementing it again cannot pass. ", key, n)
	fmt.Fprintf(&b, "The overseer runs the task's validation commands itself, in %s, as %s.", st.cwd(), validationUser())
	if run != nil {
		b.WriteString("\n\nThe failure:\n")
		size := 0
		for _, r := range run.Records {
			if r.Outcome == execution.ValidationPass {
				continue
			}
			line := fmt.Sprintf("`%s %s` → %s\n", r.Program, strings.Join(r.Args, " "), r.Outcome)
			b.WriteString(line)
			for _, l := range failureLines(r) {
				if size += len(l); size > 6000 {
					b.WriteString("    …\n")
					break
				}
				b.WriteString("    " + l + "\n")
			}
		}
	}
	fmt.Fprintf(&b, "\nReplan task %s so it can succeed under that validation: rewrite its section in %s and %s (Status: ACTIVE, criteria unchecked). "+
		"You may change its objective, instructions, acceptance criteria, required tests and validation commands (for example, a test that relies on "+
		"something the overseer's environment does not provide); keep its key, kind and dependencies. The ENGINEER implements the replanned task next.",
		key, wbs.BacklogFile, wbs.ActiveTaskFile)
	b.WriteString(expectedFormat(format))
	return feedback(b.String()), nil
}

// handleReplanCheck reads the ARCHITECT's replanned task: the same section
// in BACKLOG.md and ACTIVE_TASK.md, the same key, kind and dependencies,
// valid under the plan checks and actually changed. The stored definition
// is replaced, the task's attempt counters reset, and the ENGINEER gets
// the task (REPLANNED → IMPLEMENT). Anything else is resent to the
// architect with the problem.
func (e *Engine) handleReplanCheck(ctx context.Context, st *engineState, step *persistence.StepRecord) (execution.StepResult, error) {
	pend, err := e.pendingDispatch(ctx, st)
	if err != nil {
		return execution.StepResult{}, err
	}
	if pend == nil || st.unit == nil {
		return e.requireRecovery(ctx, st, step, "no replan dispatch to check", nil)
	}
	refs := persistence.StepRefs{DispatchID: pend}
	if stop, err := e.commandFailed(ctx, st, step, *pend, "the replan command", refs); stop != nil || err != nil {
		if err != nil {
			return execution.StepResult{}, err
		}
		return *stop, nil
	}
	key := wbs.UnitKey(st.unit)
	t, problem := e.activeTask(st)
	if problem != "" {
		return e.statefileRetry(ctx, st, step, problem, refs)
	}
	backlog, problem := readTaskFile(st.cwd(), wbs.BacklogFile, false)
	if problem != "" {
		return e.statefileRetry(ctx, st, step, problem, refs)
	}
	bt := backlog.Find(key)
	if bt == nil {
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("task %s is missing from %s", key, wbs.BacklogFile), refs)
	}
	a, _ := json.Marshal(t.Unit)
	bb, _ := json.Marshal(bt.Unit)
	if string(a) != string(bb) {
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("%s and %s must hold the same replanned task %s", wbs.BacklogFile, wbs.ActiveTaskFile, key), refs)
	}
	u := t.Unit
	deps, err := e.d.Repo.UnitDependencies(ctx, nil, st.plan.ID)
	if err != nil {
		return execution.StepResult{}, err
	}
	keyOf := map[uuid.UUID]string{}
	var carried []string
	for _, x := range st.units {
		keyOf[x.ID] = wbs.UnitKey(x)
		if x.ID != st.unit.ID {
			carried = append(carried, wbs.UnitKey(x))
		}
	}
	var stored []string
	for _, d := range deps[st.unit.ID] {
		stored = append(stored, keyOf[d])
	}
	got := append([]string(nil), u.DependsOn...)
	slices.Sort(stored)
	slices.Sort(got)
	if kind, _ := wbs.ParseWorkUnitKind(u.Kind); kind != st.unit.Kind || !slices.Equal(stored, got) {
		return e.statefileRetry(ctx, st, step, fmt.Sprintf("a replanned task keeps its key, kind and dependencies: %s is %s and depends on %s",
			key, st.unit.Kind, depList(stored)), refs)
	}
	pc := &wbs.PlanCandidate{Units: []wbs.CandidateUnit{u}}
	if problems := validateCandidate(pc, st.engine.AtomicPolicy, carried); len(problems) > 0 {
		msgs := make([]string, 0, len(problems))
		for _, p := range problems {
			msgs = append(msgs, p.Code+": "+p.Message)
		}
		return e.statefileRetry(ctx, st, step, "the replanned task "+key+" is invalid: "+strings.Join(msgs, "; "), refs)
	}
	spec := wbs.PlannedUnitsFromCandidate(pc)[0].Spec
	if st.unit.Specification != nil && sameSpec(*st.unit.Specification, spec) {
		return e.statefileRetry(ctx, st, step, "the replanned task "+key+" is unchanged: change what makes it fail under the overseer's validation", refs)
	}
	uid := st.unit.ID
	tx := func(tx persistence.Transaction) error {
		if err := e.d.Repo.ReplanUnit(ctx, tx, uid, &spec); err != nil {
			return err
		}
		err := e.d.Repo.SetUnitStatus(ctx, tx, uid, []wbs.WorkUnitStatus{wbs.WorkUnitStatusVALIDATING, wbs.WorkUnitStatusINCOMPLETE,
			wbs.WorkUnitStatusEXECUTING}, wbs.WorkUnitStatusEXECUTING)
		if err != nil && !errors.Is(err, persistence.ErrConflict) {
			return err
		}
		return nil
	}
	ev := pendingEvent{Type: execution.EventUnitReplanned, Payload: map[string]any{"work_unit_id": uid.String(), "key": key,
		"resets": []string{LimitFixAttempts, LimitStatefileResends, LimitIdenticalFailures}}}
	refs.Detail = map[string]any{"replanned": key}
	return e.advance(ctx, st, step, "REPLANNED", advanceOpts{Refs: refs, Outcome: "REPLANNED", SetUnit: true, UnitID: &uid, Tx: tx,
		Events: []pendingEvent{ev}})
}

func depList(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

// sameSpec reports whether a replan changed nothing that matters. The
// stored and the parsed definitions are compared after dropping empty
// values, so representation differences (an empty list vs none) do not
// count as a change.
func sameSpec(a, b wbs.WorkUnitSpecification) bool {
	pick := func(s wbs.WorkUnitSpecification) string {
		j, _ := json.Marshal([]any{s.Objective, s.ImplementationInstructions, s.RequiredTests, s.Validation, s.AcceptanceChecks, s.Paths})
		var v any
		_ = json.Unmarshal(j, &v)
		n, _ := json.Marshal(dropEmpty(v))
		return string(n)
	}
	return pick(a) == pick(b)
}

// dropEmpty removes nulls, empty strings, empty lists and empty objects
// from a decoded JSON value; inside lists they become null (positions
// are kept).
func dropEmpty(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if e = dropEmpty(e); !isEmptyJSON(e) {
				out[k] = e
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			if e = dropEmpty(e); isEmptyJSON(e) {
				e = nil
			}
			out = append(out, e)
		}
		return out
	}
	return v
}

func isEmptyJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}
