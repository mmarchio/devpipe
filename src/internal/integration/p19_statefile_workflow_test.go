package integration

import (
	"context"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/service"
	"overseer/internal/wbs"
	"overseer/internal/worker"
)

// sfTeam simulates the Hermes team of the statefile workflow: it acts on
// each command by editing <working directory>/statefiles like the roles
// would, and records the commands it received.
type sfTeam struct {
	t      *testing.T
	dir    string
	script string
	mu     sync.Mutex
	cmds   []string
	// sessions is the worker session of each command.
	sessions []string
	tasks    []sfTask
	// failFirstTest leaves the first test of this task with an unmet
	// criterion.
	failFirstTest string
	tested        map[string]int
	// badFirstPlan writes a first backlog whose COMMAND criterion has
	// text after the command; the retry must name PLAN_REJECTED.md.
	badFirstPlan bool
	planned      int
	// priorWork plans a backlog that already records task A as DONE
	// (archived) and B as ACTIVE, like a recreated project.
	priorWork bool
	// badFirstActive makes the first acceptance write ACTIVE_TASK.md with
	// the task heading as its title (unparsable); the resend must carry
	// the parse error.
	badFirstActive bool
	accepted       int
	resends        []string
	// keepBadActive writes every active task unparsable; fixActive
	// rewrites it correctly at the next acceptance.
	keepBadActive, fixActive bool
	// alwaysFail leaves every test of this task with an unmet criterion.
	alwaysFail string
	// skills are the output-format skill commands received.
	skills []string
	// replans are the replan commands received; replanCheck is the check
	// the architect gives a replanned task; replanNoopFirst answers the
	// first replan without changing anything.
	replans         []string
	replanCheck     string
	replanNoopFirst bool
	// silentTests answers this many test commands without touching
	// ACTIVE_TASK.md (the result reported only in the reply).
	silentTests int
	// kv writes every task file and the plan in the key:value format.
	kv bool
}

// doc renders a statefile with its title in the team's format.
func (s *sfTeam) doc(title, body string) string {
	if s.kv {
		return "title: " + title + "\n\n" + body
	}
	return "# " + title + "\n\n" + body
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

// badActive renders an active task with its heading as the document
// title (unparsable).
func badActive(section string) string { return strings.Replace(section, "## ", "# ", 1) }

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

func (s *sfTeam) file(rel string) string { return filepath.Join(s.dir, rel) }

func (s *sfTeam) write(rel, content string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(s.file(rel)), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(s.file(rel), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// section renders one task in the task file format.
func (s *sfTeam) section(t sfTask, met bool) string {
	box := "[ ]"
	if met {
		box = "[x]"
	}
	deps := t.deps
	if deps == "" {
		deps = "none"
	}
	if s.kv {
		return fmt.Sprintf("task: %s — Task %s\nstatus: %s\nkind: ATOMIC\ndepends_on: %s\nsingle_invocation: yes\nobjective: Deliver %s.\n"+
			"criterion: %s [COMMAND] %s behaves `%s %s`\ncriterion: %s [MANUAL] %s reviewed\nvalidation: `%s %s`\n\n",
			t.key, t.key, t.status, deps, t.key, box, t.key, s.script, t.checkName(), box, t.key, s.script, t.checkName())
	}
	return fmt.Sprintf("## %s — Task %s\nStatus: %s\nKind: ATOMIC\nDepends on: %s\nSingle invocation: yes\n\n### Objective\nDeliver %s.\n\n"+
		"### Acceptance criteria\n- %s [COMMAND] %s behaves `%s %s`\n- %s [MANUAL] %s reviewed\n\n### Validation\n- `%s %s`\n\n",
		t.key, t.key, t.status, deps, t.key, box, t.key, s.script, t.checkName(), box, t.key, s.script, t.checkName())
}

func (s *sfTeam) backlog() {
	var b strings.Builder
	b.WriteString(s.doc("Backlog — phase 1", ""))
	for _, t := range s.tasks {
		b.WriteString(s.section(t, t.status == wbs.TaskStatusDone))
	}
	s.write(wbs.BacklogFile, b.String())
}

func (s *sfTeam) find(key string) *sfTask {
	for i := range s.tasks {
		if s.tasks[i].key == key {
			return &s.tasks[i]
		}
	}
	return nil
}

func (s *sfTeam) respond(m hermestest.Message) *hermestest.Behavior {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Dir != s.dir {
		s.t.Errorf("command %q does not start with \"change working directory to %s\" (dir %q)", m.Content, s.dir, m.Dir)
	}
	s.cmds = append(s.cmds, m.Content)
	s.sessions = append(s.sessions, m.SessionID)
	// A resent command carries the rejected statefile problem after its
	// first line.
	cmd, problem, _ := strings.Cut(m.Content, "\n")
	skill := strings.HasPrefix(cmd, "create the ") && strings.HasSuffix(cmd, "-output-format skill")
	if problem != "" && !strings.HasPrefix(cmd, "plan phase") && !skill && !strings.HasPrefix(cmd, "have the architect replan task ") {
		s.resends = append(s.resends, m.Content)
	}
	switch {
	case skill:
		s.skills = append(s.skills, m.Content)
	case strings.HasPrefix(cmd, "have the architect replan task "):
		s.replans = append(s.replans, m.Content)
		if s.replanNoopFirst && len(s.replans) == 1 {
			break
		}
		key := strings.TrimPrefix(cmd, "have the architect replan task ")
		t := s.find(key)
		t.check, t.status = s.replanCheck, "ACTIVE"
		s.write(wbs.ActiveTaskFile, s.doc("Active task", "")+s.section(*t, false))
		s.backlog()
	case strings.HasPrefix(m.Content, "plan phase 2"):
		// The backlog keeps phase 1's DONE tasks and appends phase 2's.
		s.write(wbs.PlanFile, "# Plan — phase 2\n\nFrom the WBS.\n")
		s.tasks = append(s.tasks, sfTask{key: "C", status: "TODO"}, sfTask{key: "D", deps: "C", status: "TODO"})
		s.backlog()
	case strings.HasPrefix(m.Content, "plan phase 1"):
		s.planned++
		_, rejErr := os.Stat(s.file(wbs.PlanRejectedFile))
		notice := strings.Contains(m.Content, wbs.PlanRejectedFile)
		if retry := s.badFirstPlan && s.planned == 2; retry {
			rej, _ := os.ReadFile(s.file(wbs.PlanRejectedFile))
			if !notice || !strings.Contains(string(rej), "`A` INVALID_ACCEPTANCE_CHECK") ||
				!strings.Contains(m.Content, "The previous plan was rejected:\n- A INVALID_ACCEPTANCE_CHECK: acceptance check 1: COMMAND verification requires a command") ||
				!strings.Contains(m.Content, "Expected format:\n\n") || !strings.Contains(m.Content, "A `[COMMAND]` criterion must end with exactly one command in backticks") {
				s.t.Errorf("rejected plan retry: command %q, %s %q", m.Content, wbs.PlanRejectedFile, rej)
			}
		} else if notice || rejErr == nil {
			s.t.Errorf("plan command %q names %s before any rejection", m.Content, wbs.PlanRejectedFile)
		}
		wbsFile, err := os.ReadFile(s.file(wbs.WBSFile(1)))
		if err != nil {
			s.t.Errorf("the WBS was not loaded before planning: %v", err)
		}
		if s.kv {
			s.write(wbs.PlanFile, "title: Plan — phase 1\nstep: deliver the phase WBS\n")
		} else {
			s.write(wbs.PlanFile, "# Plan — phase 1\n\nFrom the WBS:\n"+string(wbsFile))
		}
		s.tasks = []sfTask{{key: "A", status: "TODO"}, {key: "B", deps: "A", status: "TODO"}}
		if s.priorWork {
			s.tasks[0].status, s.tasks[1].status = wbs.TaskStatusDone, "ACTIVE"
			s.write(wbs.ArchiveFile, s.doc("Task archive", "")+s.section(s.tasks[0], true))
			s.write(wbs.ActiveTaskFile, s.doc("Active task", "")+s.section(s.tasks[1], false))
		}
		s.backlog()
		if s.badFirstPlan && s.planned == 1 {
			b, _ := os.ReadFile(s.file(wbs.BacklogFile))
			bad := strings.Replace(string(b), fmt.Sprintf("[COMMAND] A behaves `%s A`", s.script), fmt.Sprintf("[COMMAND] `%s A` behaves", s.script), 1)
			s.write(wbs.BacklogFile, bad)
		}
	case cmd == "have the project manager perform acceptance tasks":
		s.accepted++
		active, _ := os.ReadFile(s.file(wbs.ActiveTaskFile))
		if d, err := wbs.ParseTasks(string(active)); err == nil && len(d.Tasks) == 0 {
			active = nil // a file with no task (e.g. cleared by a requeue)
		}
		archive, _ := os.ReadFile(s.file(wbs.ArchiveFile))
		for i := range s.tasks {
			t := &s.tasks[i]
			if t.status == wbs.TaskStatusComplete {
				t.status = wbs.TaskStatusDone
				if len(archive) == 0 {
					archive = []byte(s.doc("Task archive", ""))
				}
				archive = append(archive, s.section(*t, true)...)
				s.write(wbs.ArchiveFile, string(archive))
				active = nil
			}
		}
		if len(active) == 0 {
			next := "# Active task\n\nNo active task.\n"
			if s.kv {
				next = "title: Active task\n"
			}
			for i := range s.tasks {
				t := &s.tasks[i]
				if t.status == "TODO" && (t.deps == "" || s.find(t.deps).status == wbs.TaskStatusDone) {
					t.status = "ACTIVE"
					next = s.doc("Active task", "") + s.section(*t, false)
					break
				}
			}
			if (s.badFirstActive && s.accepted == 1) || s.keepBadActive {
				// The task heading written as the document title.
				next = badActive(strings.TrimPrefix(next, "# Active task\n\n"))
			}
			s.write(wbs.ActiveTaskFile, next)
		} else if (s.badFirstActive && s.accepted == 2) || s.keepBadActive || s.fixActive {
			// Resent with the problem: rewrite the active task (correctly
			// unless it stays bad).
			for _, t := range s.tasks {
				if t.status == "ACTIVE" {
					content := s.doc("Active task", "") + s.section(t, false)
					if s.keepBadActive {
						content = badActive(s.section(t, false))
					}
					s.write(wbs.ActiveTaskFile, content)
				}
			}
		}
		s.backlog()
	case strings.HasPrefix(cmd, "implement task "):
		key := strings.TrimPrefix(cmd, "implement task ")
		s.write("src/"+key+".txt", "implemented "+key+"\n")
	case strings.HasPrefix(cmd, "test task "):
		key := strings.TrimPrefix(cmd, "test task ")
		t := s.find(key)
		s.tested[key]++
		if s.silentTests > 0 {
			s.silentTests--
			break
		}
		if (key == s.failFirstTest && s.tested[key] == 1) || key == s.alwaysFail {
			// A failing test is noted under ### Instructions (the result is
			// recorded even though the status stays).
			note := fmt.Sprintf("### Instructions\nTest round %d: a criterion is not met.\n\n### Acceptance criteria", s.tested[key])
			anchor := "### Acceptance criteria"
			if s.kv {
				note = fmt.Sprintf("instructions: Test round %d: a criterion is not met.\ncriterion:", s.tested[key])
				anchor = "criterion:"
			}
			s.write(wbs.ActiveTaskFile, s.doc("Active task", "")+strings.Replace(s.section(*t, false), anchor, note, 1))
			break
		}
		t.status = wbs.TaskStatusComplete
		s.write(wbs.ActiveTaskFile, s.doc("Active task", "")+s.section(*t, true))
		s.backlog()
	default:
		s.t.Errorf("unexpected command %q", m.Content)
	}
	reply := "done: " + m.Content
	return &hermestest.Behavior{Content: &reply}
}

// The statefile workflow end to end: commands only, every transition
// decided from the statefiles, the overseer's validation before
// acceptance, a checkpoint per accepted task, then the next phase /
// project completion.
func TestP19StatefileWorkflow(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, failFirstTest: "A", tested: map[string]int{}}
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
	if err != nil || !strings.Contains(string(guide), "| `implement task <KEY>` | ENGINEER |") ||
		!strings.Contains(string(guide), "| `plan phase <N>` | ARCHITECT, then PLANNER |") || !strings.Contains(string(guide), "## Return format") ||
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
	// full task file format.
	var projectName string
	if err := r.h.DBScan(ctx, "SELECT name FROM projects WHERE id = '"+r.pid.String()+"'", &projectName); err != nil {
		t.Fatal(err)
	}
	skill := service.OutputFormatSkillName(projectName)
	team.mu.Lock()
	skills := append([]string(nil), team.skills...)
	team.mu.Unlock()
	if len(skills) != 1 || !strings.HasPrefix(skills[0], "create the "+skill+" skill\n\nCreate this Hermes skill now") ||
		!strings.Contains(skills[0], "name: "+skill+"\n") || !strings.Contains(skills[0], service.TaskFileFormat(worker.FormatMarkdown)) {
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
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id WHERE w.status = 'DONE' AND p.phase_id = '"+r.phases[1].String()+"'"); n != 2 {
		t.Fatalf("%d units DONE, want 2", n)
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE kind = 'UNIT' AND status = 'CREATED' AND "+r.where()); n != 2 {
		t.Fatalf("%d unit checkpoints, want 2", n)
	}
	// The tester's MANUAL checks were recorded as approvals.
	if n := r.count(t, "SELECT count(*) FROM acceptance_approvals WHERE approved_by = 'TESTER'"); n < 2 {
		t.Fatalf("%d tester approvals", n)
	}
	if b, err := os.ReadFile(filepath.Join(r.dir, wbs.WBSFile(1))); err != nil || !strings.HasPrefix(string(b), "# WBS phase 1") {
		t.Fatalf("WBS file: %q %v", b, err)
	}
	// Every statefile the overseer verified has its last verified MD5
	// stored; nothing rewrote them after the final verification.
	for _, rel := range []string{wbs.PlanFile, wbs.BacklogFile, wbs.ActiveTaskFile, wbs.ArchiveFile} {
		b, err := os.ReadFile(filepath.Join(r.dir, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		sum := fmt.Sprintf("%x", md5.Sum(b))
		if n := r.count(t, "SELECT count(*) FROM statefile_hashes WHERE execution_id = '"+r.execID.String()+"' AND path = '"+rel+"' AND md5 = '"+sum+"'"); n != 1 {
			t.Fatalf("%s: stored hash does not match %s", rel, sum)
		}
	}
}

// A test command that does not reach the worker is resent, never routed
// to the engineer; the acceptance check refuses a task that skipped the
// archive: the acceptance command is resent with the problem (bounded),
// then recovery.
func TestP19StatefileWorkflowRefusesUnarchivedTask(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}}
	archiveSkipped := false
	r.g.Responder = func(m hermestest.Message) *hermestest.Behavior {
		b := team.respond(m)
		if m.Content == "have the project manager perform acceptance tasks" && !archiveSkipped && team.find("A").status == wbs.TaskStatusDone {
			// The project manager "forgets" the archive.
			archiveSkipped = true
			_ = os.Remove(filepath.Join(r.dir, wbs.ArchiveFile))
		}
		return b
	}
	res := r.runUntil(t, execution.StepRecoveryRequired, 200)
	if reason := r.recoveryReason(t); !strings.Contains(reason, "did not archive task A") || !strings.Contains(reason, "still wrong after 3 resends") {
		t.Fatalf("recovery %q\ntrace: %s", reason, p16Trace(res))
	}
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id WHERE w.status = 'DONE' AND p.phase_id = '"+r.phases[1].String()+"'"); n != 0 {
		t.Fatalf("a unit was marked DONE without its archive (%d)", n)
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
	resent := "have the project manager perform acceptance tasks\n\nThe statefiles you wrote for this command were rejected: the project manager did not archive task A"
	pc, _ := prompt["content"].(string)
	rc, _ := resp["content"].(string)
	if ev["last_dispatch_id"] == nil || !strings.HasPrefix(rc, "done: "+resent) || !strings.HasPrefix(pc, resent) || prompt["source"] != "COMMAND" {
		t.Fatalf("recovery evidence without the prompt and the model reply: %s", raw)
	}
}

// A rejected plan is explained to the planner: the problems are written
// to PLAN_REJECTED.md, the retried PLAN command points at it, and the file
// is removed once a plan is accepted.
func TestP19StatefileWorkflowExplainsRejectedPlan(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, badFirstPlan: true}
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
	if _, err := os.Stat(filepath.Join(r.dir, wbs.PlanRejectedFile)); !os.IsNotExist(err) {
		t.Fatalf("%s still exists after the plan was accepted: %v", wbs.PlanRejectedFile, err)
	}
}

// A recreated project planned from a backlog with prior progress: the task
// the statefiles record as DONE is validated by the overseer and adopted,
// never re-implemented; work continues with the active task.
func TestP19StatefileWorkflowAdoptsPriorWork(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, priorWork: true}
	r.g.Responder = team.respond
	res, err := r.eng().Run(context.Background(), r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	cmds := team.commandLines()
	want := strings.Join([]string{
		"create the output-format skill",
		"plan phase 1",
		"have the project manager perform acceptance tasks", // adopts A → overseer validation
		"have the project manager perform acceptance tasks", // A DONE; B is active
		"implement task B", "test task B",
		"have the project manager perform acceptance tasks", // archives B
	}, " | ")
	if cmds != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", cmds, want, r.recoveryReason(t), p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q", st, r.recoveryReason(t))
	}
	// A was marked DONE on a passing overseer validation of its own.
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN validation_runs v ON v.id = w.done_validation_run_id WHERE w.unit_key = 'A' AND w.status = 'DONE' AND v.status = 'PASS' AND v.execution_id = '"+r.execID.String()+"'"); n != 1 {
		t.Fatalf("task A adopted without its own passing validation (%d)", n)
	}
}

// A phase is planned from its own tasks only: the backlog still holds the
// earlier phase's DONE tasks, but they stay in that phase's plan and are
// never carried into the next phase's plan.
func TestP19StatefileWorkflowPlansOnlyThePhase(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true, phases: []p15Phase{{1, "Foundation", "PENDING", 1}, {2, "Second", "PENDING", 1}}})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}}
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

// An adopted task that fails the overseer's validation stops for recovery
// with the reset instructions; it is never sent to the engineer.
func TestP19StatefileWorkflowAdoptedTaskFails(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, priorWork: true}
	r.g.Responder = team.respond
	r.setCheck(t, "A", 1)
	res, _ := r.eng().Run(context.Background(), r.execID, 200)
	if reason := r.recoveryReason(t); !strings.Contains(reason, "task A is DONE in statefiles/BACKLOG.md but failed the overseer's validation") {
		t.Fatalf("recovery %q\ntrace: %s", reason, p16Trace(res))
	}
	team.mu.Lock()
	defer team.mu.Unlock()
	for _, c := range team.cmds {
		if c == "implement task A" {
			t.Fatalf("the adopted task was sent to the engineer: %q", team.cmds)
		}
	}
}

// A statefile the team wrote wrongly is not a stop: the command is resent
// with the parse error appended, and the corrected file lets work go on.
func TestP19StatefileWorkflowResendsWithErrors(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true})
	team := &sfTeam{t: t, dir: r.dir, script: r.script, tested: map[string]int{}, badFirstActive: true}
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
	if len(team.resends) != 1 || !strings.HasPrefix(team.resends[0], "have the project manager perform acceptance tasks\n") ||
		!strings.Contains(team.resends[0], "statefiles/ACTIVE_TASK.md") || !strings.Contains(team.resends[0], "outside a task") ||
		!strings.Contains(team.resends[0], "statefiles/HERMES.md") || !strings.Contains(team.resends[0], "Expected format:\n\n") ||
		!strings.Contains(team.resends[0], "## <KEY> — <task name>\nStatus: TODO | ACTIVE | COMPLETE | DONE") {
		t.Fatalf("resends %q", team.resends)
	}
}

// TestP19StatefileWorkflowKeyValue — a key:value project runs the whole
// statefile workflow with every task file and the plan in the key:value
// format: HERMES.md and the skill describe that format, the overseer
// parses the team's key:value files (including a failed test noted in an
// instructions: line) and the execution completes.
func TestP19StatefileWorkflowKeyValue(t *testing.T) {
	r := p16NewRigWith(t, p16Options{statefile: true, noBootstrap: true})
	r.exec(t, "INSERT INTO project_worker_settings (project_id, session_policy, concurrent_send, lease_ms, wait_ms, response_format) "+
		"VALUES ('"+r.pid.String()+"', 'SINGLE_ACTIVE_PER_PROJECT', 'REJECT', 60000, 30000, 'KEY_VALUE')")
	r.boot(t)
	team := &sfTeam{t: t, dir: r.dir, script: r.script, failFirstTest: "A", tested: map[string]int{}, kv: true}
	r.g.Responder = team.respond
	ctx := context.Background()

	guide, err := os.ReadFile(filepath.Join(r.dir, service.GuideFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"task: <KEY> — <task name>\nstatus: TODO | ACTIVE | COMPLETE | DONE", "every task `status: TODO`",
		"check `criterion: [x]` each acceptance criterion", "`step: <planned step>`"} {
		if !strings.Contains(string(guide), want) {
			t.Fatalf("HERMES.md lacks %q:\n%s", want, guide)
		}
	}
	if strings.Contains(string(guide), "### Acceptance criteria") {
		t.Fatalf("HERMES.md still describes the Markdown layout:\n%s", guide)
	}

	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	want := strings.Join([]string{
		"create the output-format skill",
		"plan phase 1",
		"have the project manager perform acceptance tasks",
		"implement task A", "test task A",
		"implement task A", "test task A",
		"have the project manager perform acceptance tasks",
		"implement task B", "test task B",
		"have the project manager perform acceptance tasks",
	}, " | ")
	if cmds := team.commandLines(); cmds != want {
		t.Fatalf("commands\n got %s\nwant %s\nrecovery: %q\ntrace: %s", cmds, want, r.recoveryReason(t), p16Trace(res))
	}
	team.mu.Lock()
	skills := append([]string(nil), team.skills...)
	team.mu.Unlock()
	if len(skills) != 1 || !strings.Contains(skills[0], service.TaskFileFormat(worker.FormatKeyValue)) {
		t.Fatalf("skill command lacks the key:value task format: %q", skills)
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if n := r.count(t, "SELECT count(*) FROM work_units w JOIN execution_plans p ON p.id = w.execution_plan_id WHERE w.status = 'DONE' AND p.phase_id = '"+r.phases[1].String()+"'"); n != 2 {
		t.Fatalf("%d units DONE, want 2", n)
	}
	for _, rel := range []string{wbs.BacklogFile, wbs.ArchiveFile, wbs.ActiveTaskFile} {
		b, err := os.ReadFile(filepath.Join(r.dir, rel))
		if err != nil || !wbs.IsKeyValueTasks(string(b)) {
			t.Fatalf("%s is not key:value: %q %v", rel, b, err)
		}
	}
}
