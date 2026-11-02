package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// The git-gated TDD workflow end to end. Each task starts on task/<key>
// from main with the tester's tests, which the overseer commits and locks;
// the engineer works in sandbox/<key> with a fresh context every attempt.
// Task A's first attempt weakens its locked test: the sandbox is
// hard-reset and the engineer is told why. Task B's first attempt fails
// validation: its sandbox is discarded too. Each validated task is
// committed and fast-forwarded into main, and its branches are deleted.
func TestP20GitGatedWorkflow(t *testing.T) {
	r := p16NewRigWith(t, p16Options{gitGated: true})
	team := newSFTeam(t, r)
	team.noManual = true
	team.tamperFirstImplement = "A"
	team.failFirstImplement = "B"
	r.g.Responder = team.respond
	ctx := context.Background()

	guide, err := os.ReadFile(filepath.Join(r.dir, "statefiles", "HERMES.md"))
	if err != nil || !strings.Contains(string(guide), "write the tests for task <KEY>") || !strings.Contains(string(guide), "`sandbox/<KEY>`") {
		t.Fatalf("HERMES.md: %s %v", guide, err)
	}

	res, err := r.eng().Run(ctx, r.execID, 300)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{
		"create the output-format skill",
		"plan phase 1",
		"write the tests for task A",
		"implement task A", // weakens the locked test → discarded
		"implement task A", // passes → committed, promoted, B loaded
		"write the tests for task B",
		"implement task B", // fails validation → discarded
		"implement task B", // passes → phase complete
	}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", got, want, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if seq := r.statesSeq(t); !strings.Contains(seq, "ACCEPTANCE_CHECK→QA_TESTS→QA_TESTS_CHECK→IMPLEMENT") ||
		!strings.Contains(seq, "VALIDATE→IMPLEMENT") {
		t.Fatalf("states %s", seq)
	}

	team.mu.Lock()
	cmds, sessions, saw := append([]string(nil), team.cmds...), append([]string(nil), team.sessions...), append([]bool(nil), team.sawImplementation...)
	team.mu.Unlock()
	// Every implementation ran with a fresh context, in a clean sandbox
	// (a discarded attempt's file was gone), told about the discard.
	impl := map[string]bool{}
	for i, c := range cmds {
		if strings.HasPrefix(c, "implement task ") {
			if impl[sessions[i]] {
				t.Errorf("implementation %d reused session %s", i, sessions[i])
			}
			impl[sessions[i]] = true
			if !strings.Contains(c, "The test suite is locked") || !strings.Contains(c, "sandbox branch sandbox/") {
				t.Errorf("implementation without the sandbox rules: %q", c)
			}
		}
	}
	for i, ok := range saw {
		if ok {
			t.Errorf("implementation %d found an earlier attempt's file in its sandbox", i)
		}
	}
	retryA, retryB := cmds[4], cmds[7]
	if !strings.Contains(retryA, "The previous attempt was discarded") || !strings.Contains(retryA, "System Integrity Error") ||
		!strings.Contains(retryA, "src/A_test.go") {
		t.Errorf("A's retry does not explain the tampering: %q", retryA)
	}
	if !strings.Contains(retryB, "The previous attempt was discarded") || !strings.Contains(retryB, "exit 1") {
		t.Errorf("B's retry does not carry the failure: %q", retryB)
	}

	// main holds exactly the validated tasks, with the locked tests.
	if b := p15Git(t, r.dir, "branch", "--show-current"); b != "main" {
		t.Fatalf("repository on %q", b)
	}
	if b := p15Git(t, r.dir, "branch", "--list"); strings.TrimSpace(b) != "* main" {
		t.Fatalf("branches left: %q", b)
	}
	log := p15Git(t, r.dir, "log", "--format=%s", "main")
	wantLog := strings.Join([]string{"overseer: final checkpoint: final checkpoint", "feat: completed task B successfully", "qa: generate specification test suite for B",
		"feat: completed task A successfully", "qa: generate specification test suite for A", "initial"}, "\n")
	if log != wantLog {
		t.Fatalf("main log\n%s\nwant\n%s", log, wantLog)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "src", "A_test.go")); string(b) != "package src // spec A\n" {
		t.Fatalf("A's test on main: %q", b)
	}
	for typ, n := range map[string]int{"TESTS_BASELINED": 2, "TESTS_TAMPERED": 1, "SANDBOX_RESET": 1, "TASK_PROMOTED": 2, "FIX_ATTEMPT": 2} {
		if got := r.count(t, "SELECT count(*) FROM execution_events WHERE type = '"+typ+"' AND execution_id = '"+r.execID.String()+"'"); got != n {
			t.Errorf("%d %s events, want %d", got, typ, n)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE kind = 'UNIT' AND status = 'CREATED' AND "+r.where()); n != 2 {
		t.Fatalf("%d unit checkpoints, want 2", n)
	}
}

// A requeued git-gated task starts over: its sandbox and task branch are
// discarded and the tester writes its tests again.
func TestP20GitGatedRequeue(t *testing.T) {
	r := p16NewRigWith(t, p16Options{gitGated: true})
	team := newSFTeam(t, r)
	team.noManual = true
	r.g.Responder = team.respond
	ctx := context.Background()

	for i := 0; i < 60 && r.state(t) != "VALIDATE"; i++ {
		r.run(t, 1)
	}
	if r.state(t) != "VALIDATE" {
		t.Fatalf("never reached VALIDATE: %s (%s)", r.state(t), team.commandLines())
	}
	if b := p15Git(t, r.dir, "branch", "--show-current"); b != "sandbox/A" {
		t.Fatalf("implementation ran on %q", b)
	}
	if _, err := r.eng().Requeue(ctx, r.pid, r.execID, "operator", "start the task over"); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	res, err := r.eng().Run(ctx, r.execID, 300)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{"create the output-format skill", "plan phase 1",
		"write the tests for task A", "implement task A", // requeued before its validation
		"write the tests for task A", "implement task A",
		"write the tests for task B", "implement task B"}, " | ")
	if got := team.commandLines(); got != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery %q\ntrace %s", got, want, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q", st, r.recoveryReason(t))
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'FIX_ATTEMPT' AND execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d fix attempts after a requeue", n)
	}
	// One test commit per task reached main: the discarded one did not.
	if n := strings.Count(p15Git(t, r.dir, "log", "--format=%s", "main"), "qa: generate specification test suite for A"); n != 1 {
		t.Fatalf("%d test commits for A on main", n)
	}
}
