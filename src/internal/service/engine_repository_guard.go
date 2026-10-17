package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"syscall"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/invariants"
	"overseer/internal/persistence"
	"overseer/internal/recovery"
	"overseer/internal/resilience"
)

// repositoryGuardError stops a step before a repository-dependent side
// effect (WBS 18.48 / 18.49 / 18.53): the step dispatcher turns it into a
// RECOVERY_REQUIRED condition; nothing was sent, run or committed.
type repositoryGuardError struct {
	condition recovery.Condition
	reason    string
	evidence  map[string]any
}

func (g *repositoryGuardError) Error() string { return g.reason }

// guardRepository verifies, before op (dispatch / validation /
// checkpoint), that the working directory exists, is the repository the
// execution was bootstrapped in (root commit, top level), is on the
// expected branch, and is writable. An execution bootstrapped before
// identities were recorded has its identity recorded at the first check.
func (e *Engine) guardRepository(ctx context.Context, st *engineState, op string) error {
	if e.d.Identifier == nil || e.d.Identities == nil {
		return nil
	}
	cwd := st.cwd()
	stop := func(c recovery.Condition, reason string, ev map[string]any) error {
		if ev == nil {
			ev = map[string]any{}
		}
		ev["condition"], ev["operation"], ev["working_directory"] = string(c), op, cwd
		return &repositoryGuardError{condition: c, reason: reason, evidence: ev}
	}
	got, err := e.d.Identifier.Identify(ctx, cwd)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return stop(recovery.RepositoryMissing, fmt.Sprintf("repository missing: working directory %s does not exist; %s not performed", cwd, op),
			map[string]any{"error": err.Error()})
	case errors.Is(err, fs.ErrPermission):
		return stop(recovery.RepositoryPermission, fmt.Sprintf("repository not accessible: %s; %s not performed", err, op), nil)
	case err != nil:
		return err
	}
	// The working directory must still resolve (symlinks followed) inside
	// the execution's allowed roots: a directory swapped for a symlink to
	// elsewhere is never worked in (WBS 18.85 / 18.86).
	if roots := st.content.Worker.AllowedRoots; len(roots) > 0 {
		canon, cerr := execution.CanonicalizeWorkingDirectory(cwd)
		if cerr != nil || !execution.WithinAllowedRoots(canon, roots) {
			return stop(recovery.RepositoryPermission, fmt.Sprintf("working directory %s resolves outside the allowed roots; %s not performed", cwd, op),
				map[string]any{"resolved": canon, "allowed_roots": roots})
		}
	}
	rec, err := e.d.Identities.Get(ctx, st.rec.ID)
	if errors.Is(err, persistence.ErrNotFound) {
		rec, _, err = e.d.Identities.Record(ctx, &persistence.RepositoryIdentityRecord{ExecutionID: st.rec.ID, ProjectID: st.rec.ProjectID,
			Identity: got, Source: persistence.IdentityFromFirstCheck, RecordedAt: e.d.Now().UTC()})
	}
	if err != nil {
		return err
	}
	want := rec.Identity
	ev := map[string]any{"expected": want, "observed": got}
	if want.IsGit && !got.IsGit {
		return stop(recovery.RepositoryMissing, fmt.Sprintf("repository missing: %s is no longer a git repository; %s not performed", cwd, op), ev)
	}
	// The expected HEAD / branch: the latest bootstrap, checkpoint or
	// operator-accepted position (WBS 18.50 / 18.51).
	exp, err := e.d.Identities.LatestExpectedHead(ctx, st.rec.ID)
	if errors.Is(err, persistence.ErrNotFound) {
		exp, err = e.d.Identities.AppendExpectedHead(ctx, &persistence.ExpectedHeadRecord{ExecutionID: st.rec.ID, ProjectID: st.rec.ProjectID,
			Head: got.Head, Branch: got.Branch, Source: persistence.HeadFromFirstCheck, RecordedAt: e.d.Now().UTC()})
	}
	if err != nil {
		return err
	}
	want.Branch = exp.Branch
	ev["expected_head"], ev["expected_head_source"], ev["expected_branch"] = exp.Head, exp.Source, exp.Branch
	identity, branch := execution.IdentityMismatch(want, got)
	if identity != "" {
		return stop(recovery.RepositoryIdentityMismatch, "repository identity mismatch: "+identity+"; "+op+" not performed", ev)
	}
	if branch != "" {
		return stop(recovery.RepositoryHeadChanged, "repository "+branch+"; "+op+" not performed", ev)
	}
	if want.IsGit && got.Head != exp.Head && !e.headFromPendingCheckpoint(ctx, st, got.Head) {
		return stop(recovery.RepositoryHeadChanged, fmt.Sprintf("repository head changed (%s → %s) without an overseer checkpoint or operator acceptance; %s not performed",
			short(exp.Head), short(got.Head), op), ev)
	}
	if err := e.d.Identifier.Writable(ctx, cwd); err != nil {
		ev["error"] = err.Error()
		if errors.Is(err, syscall.ENOSPC) {
			return stop(recovery.DiskFull, fmt.Sprintf("disk full: the repository cannot be written; %s not performed", op), ev)
		}
		return stop(recovery.RepositoryPermission, fmt.Sprintf("repository not writable: %s; %s not performed", err, op), ev)
	}
	return nil
}

// headFromPendingCheckpoint reports whether head is the commit of one of
// the execution's own checkpoints whose database record is still pending
// (a crash between the commit and its record, WBS 18.58): the checkpoint
// step reconciles and records it, so the change is explained.
func (e *Engine) headFromPendingCheckpoint(ctx context.Context, st *engineState, head string) bool {
	if head == "" || e.d.Checkpointer == nil {
		return false
	}
	list, err := e.d.Repo.ListRepoCheckpoints(ctx, st.rec.ID)
	if err != nil {
		return false
	}
	for _, c := range list {
		if c.Status != persistence.CheckpointPending {
			continue
		}
		if found, err := e.d.Checkpointer.FindByKey(ctx, st.cwd(), c.Key); err == nil && found != nil && found.Commit == head {
			return true
		}
	}
	return false
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "<none>"
	}
	return s
}

// checkpointFailure classifies a failed checkpoint commit (WBS 18.52–
// 18.54): a full disk, or a repository that is now missing / unwritable,
// is named in the reason and recorded as the recovery condition; anything
// else stays a rejected checkpoint. The reason keeps its "checkpoint"
// wording, so RETRY_CHECKPOINT remains the offered operator action.
func (e *Engine) checkpointFailure(ctx context.Context, st *engineState, reason, cpErr string, evidence map[string]any) (string, map[string]any) {
	if evidence == nil {
		evidence = map[string]any{}
	}
	if resilience.MentionsDiskFull(cpErr) {
		evidence["condition"] = string(recovery.DiskFull)
		return reason + ": disk full (no space left on device)", evidence
	}
	var guard *repositoryGuardError
	if err := e.guardRepository(ctx, st, "checkpoint"); errors.As(err, &guard) {
		switch guard.condition {
		case recovery.RepositoryMissing, recovery.RepositoryPermission, recovery.DiskFull:
			evidence["condition"] = string(guard.condition)
			return reason + ": " + guard.reason, evidence
		}
	}
	return reason, evidence
}

// InvariantScanner is the database invariant scan (dbrepo.IntegrityScanner).
type InvariantScanner interface {
	Scan(ctx context.Context, scope dbrepo.IntegrityScope) ([]invariants.ID, []invariants.Violation, error)
}

// durableInvariants checks the execution's critical durable invariants
// (WBS 18.178 / 18.179): before a side effect, corruption prevents it;
// after one, an unexpected state enters recovery. Both stop with
// INVARIANT_VIOLATION; nothing is repaired (WBS 18.181).
func (e *Engine) durableInvariants(ctx context.Context, st *engineState, when, op string) error {
	if e.d.Invariants == nil {
		return nil
	}
	pid, eid := st.rec.ProjectID, st.rec.ID
	_, vs, err := e.d.Invariants.Scan(ctx, dbrepo.IntegrityScope{ProjectID: &pid, ExecutionID: &eid})
	if err != nil {
		return err
	}
	var critical []invariants.Violation
	for _, v := range vs {
		if d, ok := invariants.Lookup(v.Invariant); ok && d.Severity == invariants.Critical {
			critical = append(critical, v)
		}
	}
	if len(critical) == 0 {
		return nil
	}
	first := critical[0]
	return &repositoryGuardError{condition: recovery.InvariantViolation,
		reason:   fmt.Sprintf("execution invariant violated %s %s: %s (%s); nothing further is performed", when, op, first.Invariant, first.Detail),
		evidence: map[string]any{"condition": string(recovery.InvariantViolation), "operation": op, "when": when, "violations": critical}}
}

// CheckInvariants runs the durable invariant scan of one execution
// outside a step (the periodic integrity scan, WBS 18.180). It returns
// the number of critical violations. A schedulable execution with
// violations enters INVARIANT_VIOLATION recovery under its own lease —
// only that execution; an execution another driver holds is left to that
// driver's own pre-side-effect check (ErrExecutionBusy).
func (e *Engine) CheckInvariants(ctx context.Context, executionID uuid.UUID) (int, error) {
	if e.d.Invariants == nil {
		return 0, nil
	}
	rec, err := e.d.Execs.GetByID(ctx, nil, executionID)
	if err != nil {
		return 0, translate(err)
	}
	pid := rec.ProjectID
	_, vs, err := e.d.Invariants.Scan(ctx, dbrepo.IntegrityScope{ProjectID: &pid, ExecutionID: &executionID})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, v := range vs {
		if d, ok := invariants.Lookup(v.Invariant); ok && d.Severity == invariants.Critical {
			n++
		}
	}
	if n == 0 || !rec.Status.IsSchedulable() {
		return n, nil
	}
	m := e.lock(executionID)
	m.Lock()
	defer m.Unlock()
	d, err := e.acquire(ctx, executionID, e.recordLeaseTTL(ctx, rec))
	if err != nil {
		return n, err
	}
	defer e.release(ctx, d)
	st, err := e.load(ctx, executionID)
	if err != nil {
		return n, translate(err)
	}
	st.drv = d
	if !st.rec.Status.IsSchedulable() {
		return n, nil
	}
	var guard *repositoryGuardError
	if err := e.durableInvariants(ctx, st, "during", "periodic integrity scan"); errors.As(err, &guard) {
		_, err := e.requireRecovery(ctx, st, nil, guard.reason, guard.evidence)
		return n, err
	} else if err != nil {
		return n, err
	}
	return n, nil
}
