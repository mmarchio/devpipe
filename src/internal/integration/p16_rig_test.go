package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/config"
	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/hardening"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

// Phase 16 rig (P16-141): the p15 rig (default workflow, roles, templates,
// pinned response protocol, fake compression, hermestest fake gateway, temp
// git repository) bootstrapped to a RUNNING execution, with deterministic
// validation commands — a check script outside the repository whose exit
// code per check name is controlled by the test — and scripted worker
// replies. The scheduler is disabled: tests drive Step / Run.

type p16Rig struct {
	*p15Rig
	execID   uuid.UUID
	checks   string // directory of check exit-code files
	script   string // absolute path of the check script
	settings execution.EngineSettings
	hard     hardening.Settings
}

// p16Options tune the rig before bootstrap (captured in the snapshot).
type p16Options struct {
	phases []p15Phase
	engine func(*execution.EngineSettings, *hardening.Settings)
	config func(*config.Config)
	// noBootstrap leaves the project READY (tests that bootstrap later).
	noBootstrap bool
	// statefile runs the default statefile workflow instead of the
	// composed-prompt workflow (with the MCP server the team reports
	// through enabled, unless noMCP).
	statefile bool
	noMCP     bool
}

func p16NewRig(t *testing.T) *p16Rig { return p16NewRigWith(t, p16Options{}) }

func p16NewRigWith(t *testing.T, o p16Options) *p16Rig {
	t.Helper()
	phases := o.phases
	if phases == nil {
		phases = []p15Phase{{1, "Foundation", "PENDING", 1}}
	}
	cfg := o.config
	if o.statefile && !o.noMCP {
		mcp := p19mcpConfig(t)
		cfg = func(c *config.Config) {
			if o.config != nil {
				o.config(c)
			}
			mcp(c)
		}
	}
	h, g, srv := p15Harness(t, cfg)
	base := p15BuildWorkflow(t, h, g, srv, phases, o.statefile)
	r := &p16Rig{p15Rig: base}
	p16Configure(t, r, o)
	return r
}

// p16Configure installs the check script and engine settings, then
// bootstraps (unless noBootstrap).
func p16Configure(t *testing.T, r *p16Rig, o p16Options) {
	t.Helper()
	r.checks = t.TempDir()
	r.script = filepath.Join(t.TempDir(), "check.sh")
	body := "#!/bin/sh\nf=\"" + r.checks + "/$1\"\nif [ -f \"$f\" ]; then code=$(cat \"$f\"); else code=0; fi\n" +
		"echo \"check $1 exit $code\"\nif [ \"$code\" != 0 ]; then echo \"check $1 failed: expected behaviour missing\" >&2; fi\nexit $code\n"
	if err := os.WriteFile(r.script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	r.settings = execution.DefaultEngineSettings()
	r.settings.PhaseValidation = []wbs.ValidationSpec{r.cmd("phase")}
	r.settings.FinalValidation = []wbs.ValidationSpec{r.cmd("final")}
	r.settings.ValidationTimeout = 30 * time.Second
	// The rig's working directory is a git repository: it uses git.
	r.settings.CheckpointPolicy = execution.CheckpointRequired
	r.hard = hardening.DefaultSettings()
	r.hard.Enabled = false
	r.hard.GroupCheckpointPolicy = string(execution.CheckpointRequired)
	if o.engine != nil {
		o.engine(&r.settings, &r.hard)
	}
	if err := r.h.App.Engine().UpdateEngineSettings(context.Background(), r.pid, r.settings, r.hard); err != nil {
		t.Fatalf("engine settings: %v", err)
	}
	// The engine's planning, implementation, validation and finalization
	// states are assigned to roles like the p15 rig's other states.
	if !o.noBootstrap {
		r.boot(t)
	}
}

func (r *p16Rig) boot(t *testing.T) {
	t.Helper()
	res := r.bootstrap(t)
	r.execID = res.Execution.ID
}

// cmd is a validation command running the check script for name.
func (r *p16Rig) cmd(name string) wbs.ValidationSpec {
	return wbs.ValidationSpec{Name: "check " + name, Program: r.script, Args: []string{name}}
}

// setCheck sets the exit code of a check (0 = pass).
func (r *p16Rig) setCheck(t *testing.T, name string, code int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.checks, name), []byte(fmt.Sprint(code)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *p16Rig) eng() *service.Engine { return r.h.App.Engine() }

// p16Unit is a planner unit.
type p16Unit struct {
	key, kind string
	deps      []string
	manual    bool
}

// planJSON renders a valid atomic plan whose units validate with their
// check (key) and declare it as a COMMAND acceptance check.
func (r *p16Rig) planJSON(units ...p16Unit) string {
	var us []map[string]any
	for _, u := range units {
		kind := u.kind
		if kind == "" {
			kind = "ATOMIC"
		}
		cmd := map[string]any{"program": r.script, "args": []string{u.key}}
		checks := []map[string]any{{"text": u.key + " behaves", "verification": "COMMAND", "command": cmd}}
		if u.manual {
			checks = append(checks, map[string]any{"text": u.key + " reviewed", "verification": "MANUAL"})
		}
		deps := u.deps
		if deps == nil {
			deps = []string{}
		}
		us = append(us, map[string]any{"key": u.key, "name": "Unit " + u.key, "kind": kind, "objective": "Deliver " + u.key,
			"instructions": "Implement " + u.key, "acceptance_checks": checks, "validation": []any{cmd}, "depends_on": deps,
			"paths": []string{"src/" + u.key}, "single_invocation": true})
	}
	b, _ := json.Marshal(map[string]any{"units": us})
	return string(b)
}

func p16Reply(status, summary string, extra string) string {
	rem := "[]"
	na := "Validate."
	if status == "incomplete" {
		rem, na = `["finish the remaining work"]`, "Continue."
	}
	blocking := "[]"
	if status == "blocked" || status == "failed" {
		blocking, na = `["cannot proceed"]`, "Resolve."
	}
	s := `{"protocol_version":"1.0","status":"` + status + `","summary":"` + summary + `","completed":["work"],"remaining":` + rem +
		`,"blocking":` + blocking + `,"next_action":"` + na + `"`
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

// queuePlan queues a planner reply carrying the plan.
func (r *p16Rig) queuePlan(plan string) {
	c := p16Reply("complete", "Planned the phase.", `"plan":`+plan)
	r.g.Script(hermestest.Behavior{Content: &c})
}

// queue queues a worker reply.
func (r *p16Rig) queue(status string) {
	c := p16Reply(status, "Worker reports "+status+".", "")
	r.g.Script(hermestest.Behavior{Content: &c, Usage: p14Usage(1000)})
}

// queueRaw queues raw content.
func (r *p16Rig) queueRaw(content string, usage int64) {
	r.g.Script(hermestest.Behavior{Content: &content, Usage: p14Usage(usage)})
}

// run drives the execution until it stops; fails the test on error.
func (r *p16Rig) run(t *testing.T, budget int) []execution.StepResult {
	t.Helper()
	res, err := r.eng().Run(context.Background(), r.execID, budget)
	if err != nil {
		t.Fatalf("run: %v (results %s)", err, p16Trace(res))
	}
	return res
}

// runUntil drives until the execution stops with the given outcome.
func (r *p16Rig) runUntil(t *testing.T, want execution.StepOutcome, budget int) []execution.StepResult {
	t.Helper()
	res := r.run(t, budget)
	if len(res) == 0 || res[len(res)-1].Outcome != want {
		t.Fatalf("stopped with %s, want %s; recovery=%q\ntrace: %s", last(res), want, r.recoveryReason(t), p16Trace(res))
	}
	return res
}

func last(res []execution.StepResult) execution.StepOutcome {
	if len(res) == 0 {
		return ""
	}
	return res[len(res)-1].Outcome
}

func p16Trace(res []execution.StepResult) string {
	var parts []string
	for _, x := range res {
		parts = append(parts, fmt.Sprintf("%s@%s:%s(%s)", x.Action, x.State, x.Outcome, x.Reason))
	}
	return strings.Join(parts, " → ")
}

func (r *p16Rig) execStatus(t *testing.T) string {
	t.Helper()
	var s string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM executions WHERE id = '"+r.execID.String()+"'", &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (r *p16Rig) recoveryReason(t *testing.T) string {
	t.Helper()
	var s *string
	_ = r.h.DBScan(context.Background(), "SELECT recovery_reason FROM executions WHERE id = '"+r.execID.String()+"'", &s)
	if s == nil {
		return ""
	}
	return *s
}

// state is the runtime's current state key.
func (r *p16Rig) state(t *testing.T) string {
	t.Helper()
	var k string
	if err := r.h.DBScan(context.Background(), "SELECT s.key FROM runtime_state rt JOIN workflow_states s ON s.id = rt.current_state_id WHERE rt.execution_id = '"+
		r.execID.String()+"'", &k); err != nil {
		t.Fatal(err)
	}
	return k
}

// history is the execution's runtime history outcomes, in order.
func (r *p16Rig) history(t *testing.T) []string {
	t.Helper()
	var s *string
	if err := r.h.DBScan(context.Background(), "SELECT string_agg(s.key, ',' ORDER BY h.seq) FROM state_transition_history h JOIN workflow_states s ON s.id = h.to_state_id WHERE h.execution_id = '"+
		r.execID.String()+"'", &s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return nil
	}
	return strings.Split(*s, ",")
}

// events is the execution's timeline types, in order.
func (r *p16Rig) events(t *testing.T) []string {
	t.Helper()
	evs, err := dbrepo.NewEngineRepository(r.h.App.DB().Raw()).ListTimeline(context.Background(), r.execID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, string(e.Type))
	}
	return out
}

func countOf(xs []string, x string) int {
	n := 0
	for _, s := range xs {
		if s == x {
			n++
		}
	}
	return n
}

// unitStatuses returns "seq:key:status" of the phase's current plan units.
func (r *p16Rig) unitStatuses(t *testing.T, order int) string {
	t.Helper()
	var s *string
	if err := r.h.DBScan(context.Background(), `SELECT string_agg(w.unit_key || ':' || w.status, ',' ORDER BY w.sequence) FROM work_units w
		JOIN execution_plans ep ON ep.id = w.execution_plan_id WHERE ep.phase_id = '`+r.phases[order].String()+`' AND ep.status <> 'SUPERSEDED'
		AND ep.version = (SELECT max(version) FROM execution_plans WHERE phase_id = ep.phase_id AND status <> 'SUPERSEDED')`, &s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (r *p16Rig) phaseStatus(t *testing.T, order int) string {
	t.Helper()
	var s string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM phases WHERE id = '"+r.phases[order].String()+"'", &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (r *p16Rig) dispatches(t *testing.T) int {
	return r.count(t, "SELECT count(*) FROM worker_dispatches WHERE "+r.where())
}

func (r *p16Rig) head(t *testing.T) string { return p15Git(t, r.dir, "rev-parse", "HEAD") }

// statesSeq returns the history as a "→"-joined string (for messages).
func (r *p16Rig) statesSeq(t *testing.T) string { return strings.Join(r.history(t), "→") }

func (r *p16Rig) engRepo() *dbrepo.EngineRepository {
	return dbrepo.NewEngineRepository(r.h.App.DB().Raw())
}

func (r *p16Rig) now() time.Time { return time.Now() }

// renameGit hides (or restores) the repository's .git directory so a
// checkpoint commit fails.
func renameGit(dir string, hide bool) error {
	if hide {
		return os.Rename(filepath.Join(dir, ".git"), filepath.Join(dir, ".git-hidden"))
	}
	return os.Rename(filepath.Join(dir, ".git-hidden"), filepath.Join(dir, ".git"))
}

// queueWorkerPromptTamper arranges for the next prepared worker prompt to be
// corrupted after preparation: a trigger appends to its content on insert.
func (r *p16Rig) queueWorkerPromptTamper(t *testing.T) {
	t.Helper()
	r.exec(t, `CREATE OR REPLACE FUNCTION p16_tamper() RETURNS trigger AS $$ BEGIN NEW.content := NEW.content || ' tampered'; RETURN NEW; END; $$ LANGUAGE plpgsql`)
	r.exec(t, `DROP TRIGGER IF EXISTS p16_tamper ON worker_prompts`)
	r.exec(t, `CREATE TRIGGER p16_tamper BEFORE INSERT ON worker_prompts FOR EACH ROW EXECUTE FUNCTION p16_tamper()`)
}
