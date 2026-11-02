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
	team := newSFTeam(t, r)
	team.alwaysFail = "A"
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

// Resolving a result-resend limit recovery resets the counter: a result
// still wrong after the resolution gets a resend with the problem again,
// not an immediate second recovery.
func TestP19StatefileResendLimitResetOnRecovery(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.rejectAcceptance = true
	r.g.Responder = team.respond
	r.runUntil(t, execution.StepRecoveryRequired, 200)
	if reason := r.recoveryReason(t); !strings.Contains(reason, "(still wrong after 3 resends with the problem)") {
		t.Fatalf("recovery %q", reason)
	}
	before := team.countCmd("have the project manager perform acceptance tasks")
	team.mu.Lock()
	team.rejectAcceptance = false
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
	team := newSFTeam(t, r)
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

// A test command answered without submitting its result is resent to the
// tester with the problem; the engineer does not get the working task
// again.
func TestP19UnrecordedTestResent(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.silentTests = 1
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
	if len(team.resends) != 1 || !strings.HasPrefix(team.resends[0], "test task A\n\nThe result of this command was rejected: no TEST result was submitted for the command") {
		t.Fatalf("resends %q", team.resends)
	}
}

// A task the overseer's validation fails the same way three times is
// replanned by the architect with the failure in hand; an unchanged replan
// is resent; the replanned definition replaces the stored one, the engineer
// implements it, and it passes.
func TestP19RepeatedFailureReplansTask(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.replanCheck, team.replanNoopFirst = "A2", true
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
		"keep its key, kind and dependencies"} {
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

// A replan may split the task into sub-tasks: they are inserted before
// the task, run first in the submitted order, and the task (which now
// depends on them) runs after them; the task that depends on it keeps
// waiting for it.
func TestP19ReplanSplitsTask(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.replanCheck, team.replanSplit = "A2", true
	r.g.Responder = team.respond
	r.setCheck(t, "A", 1) // the original validation can never pass
	res, err := r.eng().Run(context.Background(), r.execID, 400)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ncommands %q\ntrace: %s", st, r.recoveryReason(t), team.commandLines(), p16Trace(res))
	}
	team.mu.Lock()
	replans := append([]string(nil), team.replans...)
	team.mu.Unlock()
	if len(replans) != 1 || !strings.Contains(replans[0], "split it") {
		t.Fatalf("replan commands %q", replans)
	}
	cmds := team.commandLines()
	after := cmds[strings.Index(cmds, "have the architect replan task A"):]
	want := strings.Join([]string{"have the architect replan task A",
		"implement task A.1", "test task A.1", "have the project manager perform acceptance tasks",
		"implement task A.2", "test task A.2", "have the project manager perform acceptance tasks",
		"implement task A", "test task A", "have the project manager perform acceptance tasks",
		"implement task B", "test task B", "have the project manager perform acceptance tasks"}, " | ")
	if after != want {
		t.Fatalf("commands after the replan\n got %s\nwant %s", after, want)
	}
	var order string
	if err := r.h.DBScan(context.Background(), "SELECT string_agg(unit_key, ',' ORDER BY sequence) FROM work_units WHERE execution_plan_id = "+
		"(SELECT execution_plan_id FROM work_units WHERE unit_key = 'A' AND project_id = '"+r.pid.String()+"')", &order); err != nil {
		t.Fatal(err)
	}
	if order != "A.1,A.2,A,B" {
		t.Fatalf("plan order %s, want A.1,A.2,A,B", order)
	}
	if n := r.count(t, "SELECT count(*) FROM work_unit_dependencies d JOIN work_units a ON a.id = d.work_unit_id JOIN work_units p ON p.id = d.depends_on_work_unit_id "+
		"WHERE a.unit_key = 'A' AND p.unit_key IN ('A.1', 'A.2') AND a.project_id = '"+r.pid.String()+"'"); n != 2 {
		t.Fatalf("A depends on %d of its parts, want 2", n)
	}
	if n := r.count(t, "SELECT count(*) FROM execution_events WHERE type = 'WORK_UNIT_REPLANNED' AND payload->'split_into' = '[\"A.1\", \"A.2\"]' "+
		"AND execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatalf("%d split replan events, want 1", n)
	}
}

// After a failed phase validation the PLAN command says the plan is
// amended and carries the failure; a plan that only resubmits the current
// tasks is rejected with that explanation, and the repair task it then
// adds runs and makes the phase pass.
func TestP19PhaseAmendmentAddsRepairTasks(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.amendRepair = true
	team.repaired = func() { r.setCheck(t, "phase", 0) }
	r.g.Responder = team.respond
	r.setCheck(t, "phase", 1)
	res, err := r.eng().Run(context.Background(), r.execID, 400)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ncommands %s\ntrace: %s", st, r.recoveryReason(t), team.commandLines(), p16Trace(res))
	}
	team.mu.Lock()
	amendments := append([]string(nil), team.amendments...)
	team.mu.Unlock()
	if len(amendments) != 2 {
		t.Fatalf("%d amendment plan commands, want 2", len(amendments))
	}
	for _, want := range []string{"must not be resubmitted", "The failure:", "phase failed", "only the new repair tasks"} {
		if !strings.Contains(amendments[0], want) {
			t.Fatalf("amendment command lacks %q:\n%s", want, amendments[0])
		}
	}
	if !strings.Contains(amendments[1], "ONLY_EXISTING_TASKS") || !strings.Contains(amendments[1], "all 2 submitted tasks reuse their keys") {
		t.Fatalf("the resubmitted plan was not rejected with the explanation:\n%s", amendments[1])
	}
	if n := team.countCmd("implement task R1"); n != 1 {
		t.Fatalf("implement task R1 sent %d times, want 1", n)
	}
}
