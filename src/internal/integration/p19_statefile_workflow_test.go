package integration

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
)

// sfTeam simulates the Hermes team of the statefile workflow: it acts on
// each command like the roles would, submits every result through the
// overseer MCP server, and records the commands it received.
type sfTeam struct {
	t   *testing.T
	r   *p16Rig
	mcp *mcpClient
	mu  sync.Mutex
	// keys numbers the idempotency keys of the team's reporting calls.
	keys int
	cmds []string
	// sessions is the worker session of each command.
	sessions []string
	tasks    []sfTask
	// failFirstTest reports the first test of this task with an unmet
	// criterion.
	failFirstTest string
	tested        map[string]int
	// badFirstPlan submits a first plan whose COMMAND criterion has no
	// command; the retry must carry the rejection.
	badFirstPlan bool
	planned      int
	// badFirstTest submits the first test result with too few criteria
	// (the MCP server refuses it; refusals collects the errors).
	badFirstTest bool
	refusals     []string
	// rejectAcceptance never accepts a finished task.
	rejectAcceptance bool
	accepted         int
	// resends are the commands resent with a rejected result's problem.
	resends []string
	// alwaysFail reports every test of this task with an unmet criterion.
	alwaysFail string
	// skills are the output-format skill commands received.
	skills []string
	// replans are the replan commands received; replanCheck is the check
	// the architect gives a replanned task; replanNoopFirst answers the
	// first replan without changing anything.
	replans         []string
	replanCheck     string
	replanNoopFirst bool
	// silentTests answers this many test commands without a result (the
	// result given only in the reply).
	silentTests int
}

// newSFTeam connects the team to the rig's MCP server.
func newSFTeam(t *testing.T, r *p16Rig) *sfTeam {
	t.Helper()
	c, err := p19mcpConnect(t, r.h.App.MCPServer().Addr(), p19mcpSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &sfTeam{t: t, r: r, mcp: c, tested: map[string]int{}}
}

// commandLines lists the commands received by their first line, the
// output-format skill command (named after the project, with the skill
// appended) as "create the output-format skill".
func (s *sfTeam) commandLines() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.cmds))
	for _, c := range s.cmds {
		first, _, _ := strings.Cut(c, "\n")
		if strings.HasPrefix(first, "create the ") && strings.HasSuffix(first, "-output-format skill") {
			first = "create the output-format skill"
		}
		out = append(out, first)
	}
	return strings.Join(out, " | ")
}

// sfTask is a task as the team planned it; status is TODO, COMPLETE
// (tested) or DONE (accepted).
type sfTask struct {
	key, deps string
	status    string
	// check is the validation check the task runs (default: its key).
	check string
}

func (t sfTask) checkName() string {
	if t.check != "" {
		return t.check
	}
	return t.key
}

// unit renders a task as a plan unit: validated by its check, with a
// COMMAND and a MANUAL acceptance criterion.
func (s *sfTeam) unit(t sfTask) map[string]any {
	cmd := map[string]any{"program": s.r.script, "args": []string{t.checkName()}}
	deps := []string{}
	if t.deps != "" {
		deps = []string{t.deps}
	}
	return map[string]any{"key": t.key, "name": "Task " + t.key, "kind": "ATOMIC", "objective": "Deliver " + t.key + ".",
		"acceptance_checks": []any{
			map[string]any{"text": t.key + " behaves", "verification": "COMMAND", "command": cmd},
			map[string]any{"text": t.key + " reviewed", "verification": "MANUAL"},
		},
		"validation": []any{cmd}, "depends_on": deps, "single_invocation": true}
}

func (s *sfTeam) plan(tasks ...sfTask) map[string]any {
	units := make([]any, 0, len(tasks))
	for _, t := range tasks {
		units = append(units, s.unit(t))
	}
	return map[string]any{"summary": "planned", "units": units}
}

func (s *sfTeam) find(key string) *sfTask {
	for i := range s.tasks {
		if s.tasks[i].key == key {
			return &s.tasks[i]
		}
	}
	return nil
}

// submit calls submit_result for the command carrying tok (the caller
// holds s.mu).
func (s *sfTeam) submit(tok string, args map[string]any) {
	s.keys++
	args["token"], args["idempotency_key"] = tok, "sf"+strconv.Itoa(s.keys)
	s.mcp.must("submit_result", args)
}

// submitFor submits a result for the command m (for responders that
// answer a command themselves).
func (s *sfTeam) submitFor(m hermestest.Message, args map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submit(tokenRE.FindString(m.Content), args)
}

func (s *sfTeam) respond(m hermestest.Message) *hermestest.Behavior {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Dir != s.r.dir {
		s.t.Errorf("command %q does not start with \"change working directory to %s\" (dir %q)", m.Content, s.r.dir, m.Dir)
	}
	s.cmds = append(s.cmds, m.Content)
	s.sessions = append(s.sessions, m.SessionID)
	tok := tokenRE.FindString(m.Content)
	if tok == "" {
		s.t.Errorf("command without a token: %q", m.Content)
	}
	cmd, _, _ := strings.Cut(m.Content, "\n")
	if strings.Contains(m.Content, "The result of this command was rejected: ") {
		s.resends = append(s.resends, m.Content)
	}
	switch {
	case strings.HasPrefix(cmd, "create the ") && strings.HasSuffix(cmd, "-output-format skill"):
		s.skills = append(s.skills, m.Content)
	case strings.HasPrefix(cmd, "have the architect replan task "):
		s.replans = append(s.replans, m.Content)
		key := strings.TrimPrefix(cmd, "have the architect replan task ")
		t := s.find(key)
		if !s.replanNoopFirst || len(s.replans) > 1 {
			t.check = s.replanCheck
		}
		s.submit(tok, map[string]any{"status": "COMPLETE", "summary": "replanned " + key, "plan": s.plan(*t)})
	case strings.HasPrefix(cmd, "plan phase 2"):
		// The plan repeats phase 1's tasks; only phase 2's are its work.
		s.tasks = append(s.tasks, sfTask{key: "C", status: "TODO"}, sfTask{key: "D", deps: "C", status: "TODO"})
		s.submit(tok, map[string]any{"status": "COMPLETE", "summary": "planned phase 2", "plan": s.plan(s.tasks...)})
	case strings.HasPrefix(cmd, "plan phase 1"):
		s.planned++
		rejected := strings.Contains(m.Content, "The previous plan was rejected:")
		if s.badFirstPlan && s.planned == 2 {
			if !rejected || !strings.Contains(m.Content, "- A INVALID_ACCEPTANCE_CHECK: acceptance check 1: COMMAND verification requires a command") ||
				!strings.Contains(m.Content, "submit the corrected plan") {
				s.t.Errorf("rejected plan retry: %q", m.Content)
			}
		} else if rejected {
			s.t.Errorf("plan command %q carries a rejection before any", m.Content)
		}
		ctxv := s.mcp.must("get_context", map[string]any{"token": tok})
		if w, _ := ctxv["wbs"].(string); !strings.HasPrefix(w, "# WBS phase 1") {
			s.t.Errorf("get_context wbs %q", w)
		}
		s.tasks = []sfTask{{key: "A", status: "TODO"}, {key: "B", deps: "A", status: "TODO"}}
		plan := s.plan(s.tasks...)
		if s.badFirstPlan && s.planned == 1 {
			a := plan["units"].([]any)[0].(map[string]any)
			delete(a["acceptance_checks"].([]any)[0].(map[string]any), "command")
		}
		s.submit(tok, map[string]any{"status": "COMPLETE", "summary": "planned phase 1", "plan": plan})
	case cmd == "have the project manager perform acceptance tasks":
		s.accepted++
		finished := false
		for i := range s.tasks {
			finished = finished || s.tasks[i].status == "COMPLETE"
		}
		if finished && s.rejectAcceptance {
			s.submit(tok, map[string]any{"status": "INCOMPLETE", "summary": "not reviewed"})
			break
		}
		for i := range s.tasks {
			if s.tasks[i].status == "COMPLETE" {
				s.tasks[i].status = "DONE"
			}
		}
		s.submit(tok, map[string]any{"status": "COMPLETE", "summary": "accepted"})
	case strings.HasPrefix(cmd, "implement task "):
		key := strings.TrimPrefix(cmd, "implement task ")
		if err := os.WriteFile(filepath.Join(s.r.dir, "src", key+".txt"), []byte("implemented "+key+"\n"), 0o644); err != nil {
			s.t.Error(err)
		}
		s.submit(tok, map[string]any{"status": "COMPLETE", "summary": "implemented " + key})
	case cmd == "continue":
		s.submit(tok, map[string]any{"status": "COMPLETE", "summary": "continued"})
	case strings.HasPrefix(cmd, "test task "):
		key := strings.TrimPrefix(cmd, "test task ")
		s.tested[key]++
		if s.silentTests > 0 {
			s.silentTests--
			break
		}
		task := s.mcp.must("get_task", map[string]any{"token": tok})
		checks, _ := task["acceptance_checks"].([]any)
		met := make([]bool, len(checks))
		for i := range met {
			met[i] = true
		}
		status := "COMPLETE"
		if (key == s.failFirstTest && s.tested[key] == 1) || key == s.alwaysFail {
			met[0], status = false, "INCOMPLETE"
		}
		if s.badFirstTest {
			// A malformed result is refused by the server; the team
			// replies without retrying.
			s.badFirstTest = false
			s.keys++
			text, isErr, err := s.mcp.call("submit_result", map[string]any{"token": tok, "idempotency_key": "sf" + strconv.Itoa(s.keys),
				"status": status, "summary": "tested " + key, "criteria_met": met[:1]})
			if err != nil || !isErr {
				s.t.Errorf("malformed test result accepted: %s %v", text, err)
			}
			s.refusals = append(s.refusals, text)
			break
		}
		s.submit(tok, map[string]any{"status": status, "summary": "tested " + key, "criteria_met": met})
		if t := s.find(key); t != nil && status == "COMPLETE" && len(met) == len(checks) {
			t.status = "COMPLETE"
		}
	default:
		s.t.Errorf("unexpected command %q", m.Content)
	}
	reply := "done: " + m.Content
	return &hermestest.Behavior{Content: &reply}
}

// The statefile workflow end to end: commands only, every transition
// decided from the submitted results, the overseer's validation before
// acceptance, a checkpoint per accepted task, then project completion.
func TestP19StatefileWorkflow(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.failFirstTest = "A"
	r.g.Responder = team.respond
	ctx := context.Background()

	// Bootstrap wrote the team files: one per workflow role and HERMES.md.
	for _, k := range []string{"ARCHITECT", "PLANNER", "PROJECT_MANAGER", "ENGINEER", "TESTER"} {
		b, err := os.ReadFile(filepath.Join(r.dir, "statefiles", k+".md"))
		if err != nil || !strings.HasPrefix(string(b), "# "+k+"\n") || !strings.Contains(string(b), "## ROLE") {
			t.Fatalf("role file %s: %q %v", k, b, err)
		}
	}
	guide, err := os.ReadFile(filepath.Join(r.dir, "statefiles", "HERMES.md"))
	if err != nil || !strings.Contains(string(guide), "| `implement task <KEY>` / `continue` | ENGINEER |") ||
		!strings.Contains(string(guide), "| `plan phase <N>` | ARCHITECT, then PLANNER |") || !strings.Contains(string(guide), "## MCP tools") ||
		!strings.Contains(string(guide), "| `create the <project>-output-format skill` | PROJECT_MANAGER |") {
		t.Fatalf("HERMES.md: %s %v", guide, err)
	}

	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	cmds := team.commandLines()
	want := strings.Join([]string{
		"create the output-format skill", // every execution starts with it
		"plan phase 1",
		"have the project manager perform acceptance tasks", // loads A
		"implement task A", "test task A",                   // a criterion unmet
		"implement task A", "test task A", // COMPLETE → overseer validation
		"have the project manager perform acceptance tasks", // archives A, loads B
		"implement task B", "test task B",
		"have the project manager perform acceptance tasks", // archives B, no task left
	}, " | ")
	if cmds != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", cmds, want, r.recoveryReason(t), p16Trace(res))
	}
	// The skill command carries the project's skill: its name and the
	// rule that results are submitted, not replied.
	var projectName string
	if err := r.h.DBScan(ctx, "SELECT name FROM projects WHERE id = '"+r.pid.String()+"'", &projectName); err != nil {
		t.Fatal(err)
	}
	skill := service.OutputFormatSkillName(projectName)
	team.mu.Lock()
	skills := append([]string(nil), team.skills...)
	team.mu.Unlock()
	if len(skills) != 1 || !strings.HasPrefix(skills[0], "create the "+skill+" skill\n\nCreate this Hermes skill now") ||
		!strings.Contains(skills[0], "name: "+skill+"\n") || !strings.Contains(skills[0], "submit_result") || strings.Contains(skills[0], "BACKLOG.md") {
		t.Fatalf("skill commands for %s: %q", skill, skills)
	}
	// The context is cleared (new session, then "cd <working directory>")
	// before the phase is planned and before each new task — not for the
	// task's repeat implementation, its tests or its acceptance (the
	// skill is saved in the execution's first session).
	team.mu.Lock()
	sess := append([]string(nil), team.sessions...)
	team.mu.Unlock()
	group := map[string]int{}
	var pattern []string
	for _, id := range sess {
		if _, ok := group[id]; !ok {
			group[id] = len(group) + 1
		}
		pattern = append(pattern, itoa(group[id]))
	}
	if got := strings.Join(pattern, ","); got != "1,2,2,3,3,3,3,3,4,4,4" {
		t.Fatalf("sessions per command %s, want 1,2,2,3,3,3,3,3,4,4,4", got)
	}
	// Every command is preceded by "change working directory to <project
	// working directory>" in its session.
	if cds := r.g.CDs(); len(cds) != len(sess) {
		t.Fatalf("%d change-directory prompts for %d commands", len(cds), len(sess))
	}
	for _, cd := range r.g.CDs() {
		if cd != r.dir {
			t.Fatalf("changed to %s, want %s", cd, r.dir)
		}
	}
	prev := ""
	for i, op := range r.g.Ops() {
		if op == "msg" && prev != "cd" {
			t.Fatalf("operation %d: a command was sent without a change-directory prompt first: %v", i, r.g.Ops())
		}
		if op != "new" {
			prev = op
		}
	}
	// Each worker session is its own Hermes session; a replaced session is
	// completed and deleted from Hermes, so only the active one remains.
	var order []string
	for _, id := range sess {
		if len(order) == 0 || order[len(order)-1] != id {
			order = append(order, id)
		}
	}
	if del, left := r.g.DeletedSessions(), r.g.HermesSessions(); strings.Join(del, ",") != strings.Join(order[:len(order)-1], ",") ||
		len(left) != 1 || left[0] != order[len(order)-1] {
		t.Fatalf("Hermes sessions deleted %q, remaining %q; sessions in order %q", del, left, order)
	}
	for _, id := range []string{sess[0], sess[1], sess[3], sess[8]} {
		if cwd := r.g.SessionCWD(id); cwd == nil || *cwd != r.dir {
			t.Fatalf("session %s working directory %v", id, cwd)
		}
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	// Commands were sent verbatim: every dispatch's prompt is a COMMAND
	// worker prompt.
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches d JOIN worker_prompts wp ON wp.id = d.worker_prompt_id WHERE wp.source <> 'COMMAND' AND d.project_id = '"+r.pid.String()+"'"); n != 0 {
		t.Fatalf("%d dispatches sent a composed prompt", n)
	}
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id WHERE w.status = 'DONE' AND w.archived_at IS NOT NULL AND p.phase_id = '"+r.phases[1].String()+"'"); n != 2 {
		t.Fatalf("%d units DONE and archived, want 2", n)
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE kind = 'UNIT' AND status = 'CREATED' AND "+r.where()); n != 2 {
		t.Fatalf("%d unit checkpoints, want 2", n)
	}
	// The tester's MANUAL checks were recorded as approvals.
	if n := r.count(t, "SELECT count(*) FROM acceptance_approvals WHERE approved_by = 'TESTER'"); n < 2 {
		t.Fatalf("%d tester approvals", n)
	}
	assertNoStatefiles(t, r.dir)
}

// A finished task the project manager does not accept: the acceptance
// command is resent with the problem (bounded), then recovery; the task
// is never marked DONE.
func TestP19StatefileWorkflowRefusesUnacceptedTask(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.rejectAcceptance = true
	r.g.Responder = team.respond
	res := r.runUntil(t, execution.StepRecoveryRequired, 200)
	if reason := r.recoveryReason(t); !strings.Contains(reason, "task A was not accepted (INCOMPLETE: not reviewed)") || !strings.Contains(reason, "still wrong after 3 resends") {
		t.Fatalf("recovery %q\ntrace: %s", reason, p16Trace(res))
	}
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id WHERE w.status = 'DONE' AND p.phase_id = '"+r.phases[1].String()+"'"); n != 0 {
		t.Fatalf("a unit was marked DONE without its acceptance (%d)", n)
	}
	// The recovery evidence carries the model's reply to the last command:
	// the last resend, which names the problem.
	code, raw, body := p9HTTP(t, "GET", r.h.BaseURL+"/api/v1/projects/"+r.pid.String()+"/executions/"+r.execID.String()+"/recovery", "")
	if code != 200 {
		t.Fatalf("recovery %d %s", code, raw)
	}
	ev, _ := body["evidence"].(map[string]any)
	resp, _ := body["response"].(map[string]any)
	prompt, _ := body["prompt"].(map[string]any)
	resent := "have the project manager perform acceptance tasks\n\nThe result of this command was rejected: task A was not accepted"
	pc, _ := prompt["content"].(string)
	rc, _ := resp["content"].(string)
	if ev["last_dispatch_id"] == nil || !strings.HasPrefix(rc, "done: "+resent) || !strings.HasPrefix(pc, resent) || prompt["source"] != "COMMAND" {
		t.Fatalf("recovery evidence without the prompt and the model reply: %s", raw)
	}
}

// A rejected plan is explained to the planner: the retried PLAN command
// carries every problem.
func TestP19StatefileWorkflowExplainsRejectedPlan(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.badFirstPlan = true
	r.g.Responder = team.respond
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if team.planned != 2 {
		t.Fatalf("planned %d times, want 2\ncommands: %q\nrecovery: %q", team.planned, team.cmds, r.recoveryReason(t))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	assertNoStatefiles(t, r.dir)
}

// A phase is planned from its own tasks only: the submitted plan still
// holds the earlier phase's DONE tasks, but they stay in that phase's plan
// and are never carried into the next phase's plan.
func TestP19StatefileWorkflowPlansOnlyThePhase(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true, phases: []p15Phase{{1, "Foundation", "PENDING", 1}, {2, "Second", "PENDING", 1}}})
	team := newSFTeam(t, r)
	r.g.Responder = team.respond
	res, err := r.eng().Run(context.Background(), r.execID, 400)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	cmds := team.commandLines()
	want := strings.Join([]string{
		"create the output-format skill",
		"plan phase 1",
		"have the project manager perform acceptance tasks",
		"implement task A", "test task A",
		"have the project manager perform acceptance tasks",
		"implement task B", "test task B",
		"have the project manager perform acceptance tasks",
		"plan phase 2",
		"have the project manager perform acceptance tasks",
		"implement task C", "test task C",
		"have the project manager perform acceptance tasks",
		"implement task D", "test task D",
		"have the project manager perform acceptance tasks",
	}, " | ")
	if cmds != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", cmds, want, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q", st, r.recoveryReason(t))
	}
	for order, keys := range map[int]string{1: "A,B", 2: "C,D"} {
		var got string
		if err := r.h.DBScan(context.Background(), "SELECT COALESCE(string_agg(w.unit_key, ',' ORDER BY w.unit_key), '') FROM work_units w "+
			"JOIN execution_plans p ON p.id = w.execution_plan_id WHERE p.phase_id = '"+r.phases[order].String()+"'", &got); err != nil {
			t.Fatal(err)
		}
		if got != keys {
			t.Fatalf("phase %d planned units %q, want %q", order, got, keys)
		}
	}
}

// A malformed result is refused by the MCP server with the problem; a team
// that replies anyway gets the command resent, and the corrected result
// lets work go on.
func TestP19StatefileWorkflowResendsWithErrors(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := newSFTeam(t, r)
	team.badFirstTest = true
	r.g.Responder = team.respond
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ncommands %q\ntrace: %s", st, r.recoveryReason(t), team.cmds, p16Trace(res))
	}
	team.mu.Lock()
	defer team.mu.Unlock()
	if len(team.refusals) != 1 || !strings.HasPrefix(team.refusals[0], "CRITERIA_COUNT: criteria_met has 1 entries; task A has 2 acceptance checks") {
		t.Fatalf("refusals %q", team.refusals)
	}
	if len(team.resends) != 1 || !strings.HasPrefix(team.resends[0], "test task A\n\nThe result of this command was rejected: no TEST result was submitted") {
		t.Fatalf("resends %q", team.resends)
	}
	if team.tested["A"] != 2 {
		t.Fatalf("task A tested %d times, want 2", team.tested["A"])
	}
}

// The statefile workflow is not started without the MCP server its team
// reports through: readiness names the problem, and no execution or
// command is created.
func TestP19StatefileWorkflowNeedsMCP(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true, noMCP: true, noBootstrap: true})
	rep, err := r.svc().Readiness(context.Background(), r.pid)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range rep.Errors {
		found = found || (e.Category == execution.CategoryMCP && strings.Contains(e.Message, "set mcp.enabled"))
	}
	if rep.Ready || !found {
		t.Fatalf("readiness %+v", rep)
	}
	_, be := r.bootstrapErr(t)
	if be.Code != execution.CodeConfigInvalid || be.Stage != execution.StageValidating || !strings.Contains(be.Message, "overseer MCP server") {
		t.Fatalf("bootstrap error %+v", be)
	}
	if n := r.count(t, "SELECT count(*) FROM executions WHERE project_id = '"+r.pid.String()+"'"); n != 0 {
		t.Fatalf("%d executions created", n)
	}
	if n := len(r.g.Ops()); n != 0 {
		t.Fatalf("the worker was contacted: %v", r.g.Ops())
	}
}
