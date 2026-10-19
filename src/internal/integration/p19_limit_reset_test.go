package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
)

// countCmd counts the commands the team received that equal cmd (first
// line).
func (s *sfTeam) countCmd(cmd string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.cmds {
		if first, _, _ := strings.Cut(c, "\n"); first == cmd {
			n++
		}
	}
	return n
}

// Resolving a "fix attempts exhausted" recovery resets the counter: the
// engineer gets the task again instead of an immediate second recovery.
func TestP19FixLimitResetOnRecovery(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, alwaysFail: "A"}
	r.g.Responder = team.respond
	r.runUntil(t, execution.StepRecoveryRequired, 200)
	if reason := r.recoveryReason(t); reason != "fix attempts exhausted" {
		t.Fatalf("recovery %q", reason)
	}
	before := team.countCmd("implement task A")
	if want := r.settings.MaxFixAttempts + 1; before != want {
		t.Fatalf("implement task A sent %d times before the limit, want %d", before, want)
	}
	team.mu.Lock()
	team.alwaysFail = ""
	team.mu.Unlock()
	if _, err := r.eng().ResolveRecovery(context.Background(), r.pid, r.execID, service.RecoveryResolution{Action: service.RecoveryResume}); err != nil {
		t.Fatal(err)
	}
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s after the reset, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if after := team.countCmd("implement task A"); after <= before {
		t.Fatalf("implement task A not sent again after the reset (%d)", after)
	}
}

// Resolving a statefile-resend limit recovery resets the counter: a file
// still wrong after the resolution gets a resend with the problem again,
// not an immediate second recovery.
func TestP19StatefileResendLimitResetOnRecovery(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, keepBadActive: true}
	r.g.Responder = team.respond
	r.runUntil(t, execution.StepRecoveryRequired, 200)
	if reason := r.recoveryReason(t); !strings.Contains(reason, "(still wrong after 3 resends with the problem)") {
		t.Fatalf("recovery %q", reason)
	}
	before := team.countCmd("have the project manager perform acceptance tasks")
	team.mu.Lock()
	team.keepBadActive, team.fixActive = false, true
	team.mu.Unlock()
	if _, err := r.eng().ResolveRecovery(context.Background(), r.pid, r.execID, service.RecoveryResolution{Action: service.RecoveryResume}); err != nil {
		t.Fatal(err)
	}
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s after the reset, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if after := team.countCmd("have the project manager perform acceptance tasks"); after <= before {
		t.Fatalf("acceptance not resent after the reset (%d)", after)
	}
}

// A skill command that does not reach Hermes is resent from ROUTE
// (RETRY → SKILL) before any work is routed; the run then completes.
func TestP19SkillCommandResent(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}}
	failed := false
	r.g.Responder = func(m hermestest.Message) *hermestest.Behavior {
		if first, _, _ := strings.Cut(m.Content, "\n"); !failed && strings.HasSuffix(first, "-output-format skill") {
			failed = true
			return &hermestest.Behavior{Status: 400, ErrorCode: "invalid_request", Message: "rejected"}
		}
		return team.respond(m)
	}
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if n := r.count(t, "SELECT count(*) FROM execution_steps WHERE execution_id = '"+r.execID.String()+"' AND state_key = 'ROUTE' AND outcome = 'RETRY'"); n != 1 {
		t.Fatalf("%d ROUTE retries, want 1\ntrace: %s", n, p16Trace(res))
	}
	team.mu.Lock()
	defer team.mu.Unlock()
	if len(team.skills) != 1 {
		t.Fatalf("%d skill commands reached the team after the resend, want 1", len(team.skills))
	}
}

// A test command answered without recording anything in ACTIVE_TASK.md
// is resent to the tester with the problem; the engineer does not get the
// working task again.
func TestP19UnrecordedTestResent(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, silentTests: 1}
	r.g.Responder = team.respond
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if n := team.countCmd("implement task A"); n != 1 {
		t.Fatalf("implement task A sent %d times, want 1 (the unrecorded test is not a failure)", n)
	}
	if n := team.countCmd("test task A"); n != 2 {
		t.Fatalf("test task A sent %d times, want 2", n)
	}
	team.mu.Lock()
	defer team.mu.Unlock()
	if len(team.resends) != 1 || !strings.HasPrefix(team.resends[0], "test task A\n\nThe statefiles you wrote for this command were rejected: the tester did not record the test result: statefiles/ACTIVE_TASK.md is unchanged") ||
		!strings.Contains(team.resends[0], "Expected format:") {
		t.Fatalf("resends %q", team.resends)
	}
}

// A task the overseer's validation fails the same way three times is
// replanned by the architect with the failure in hand; an unchanged replan
// is resent; the replanned definition replaces the stored one, the engineer
// implements it, and it passes.
func TestP19RepeatedFailureReplansTask(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, replanCheck: "A2", replanNoopFirst: true}
	r.g.Responder = team.respond
	r.setCheck(t, "A", 1) // the original validation can never pass
	res, err := r.eng().Run(context.Background(), r.execID, 300)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ncommands %q\ntrace: %s", st, r.recoveryReason(t), team.commandLines(), p16Trace(res))
	}
	team.mu.Lock()
	replans := append([]string(nil), team.replans...)
	team.mu.Unlock()
	if len(replans) != 2 {
		t.Fatalf("%d replan commands, want 2 (the unchanged first answer is resent)", len(replans))
	}
	first := replans[0]
	for _, want := range []string{"have the architect replan task A\n", "failed the overseer's validation the same way 3 times", "check A failed: expected behaviour missing",
		"Expected format:"} {
		if !strings.Contains(first, want) {
			t.Fatalf("replan command lacks %q:\n%s", want, first)
		}
	}
	if !strings.Contains(replans[1], "is unchanged") {
		t.Fatalf("the unchanged replan was not resent with the problem:\n%s", replans[1])
	}
	if n := team.countCmd("implement task A"); n != 4 {
		t.Fatalf("implement task A sent %d times, want 4 (3 before the replan, 1 after)", n)
	}
	if n := r.count(t, "SELECT count(*) FROM work_units WHERE unit_key = 'A' AND validation_commands::text LIKE '%A2%' AND status = 'DONE'"); n != 1 {
		t.Fatalf("task A's stored validation was not replaced (%d)", n)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'WORK_UNIT_REPLANNED' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatalf("%d replan events, want 1", n)
	}
}
