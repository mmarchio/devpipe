package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/wbs"
)

// p16AcceptanceModule is the repository of the acceptance project: DUP-1
// (project / phase / role normalizeName — SAFE, consolidated into the
// existing stringutil) and DUP-2 (a.slug / c.slug — consolidating would
// create a → stringutil → b → a, SKIPPED).
func p16AcceptanceModule() map[string]string {
	files := p16DupModule()
	slug := "func slug(s string) string {\n\ts = strings.TrimSpace(s)\n\ts = strings.ToUpper(s)\n\treturn strings.ReplaceAll(s, \" \", \".\")\n}\n"
	files["a/a.go"] = "package a\n\nimport \"strings\"\n\nconst Tag = \"!\"\n\n" + slug + "\nfunc A(s string) string { return slug(s) }\n"
	files["c/c.go"] = "package c\n\nimport \"strings\"\n\n" + slug + "\nfunc C(s string) string { return slug(s) }\n"
	files["b/b.go"] = "package b\n\nimport \"ex/a\"\n\nfunc Mark() string { return a.Tag }\n"
	files["stringutil/s.go"] = "package stringutil\n\nimport \"ex/b\"\n\n// Exclaim marks s.\nfunc Exclaim(s string) string { return s + b.Mark() }\n"
	return files
}

// p16AcceptanceRig builds the three-phase acceptance project.
func p16AcceptanceRig(t *testing.T, phases int, cfg func(*config.Config)) *p16Rig {
	t.Helper()
	ps := []p15Phase{{1, "Phase 1", "PENDING", 1}, {2, "Phase 2", "PENDING", 1}, {3, "Phase 3", "PENDING", 1}}[:phases]
	r := p16NewRigWith(t, p16Options{phases: ps, noBootstrap: true, config: cfg, engine: func(s *execution.EngineSettings, h *hardening.Settings) {
		*h = hardening.DefaultSettings()
		h.GroupCheckpointPolicy = string(execution.CheckpointRequired)
		h.MinStatements = 2
		s.FinalValidation = []wbs.ValidationSpec{{Name: "go vet", Program: "go", Args: []string{"vet", "./..."}},
			{Name: "go test", Program: "go", Args: []string{"test", "-count=1", "./..."}}}
	}})
	for p, c := range p16AcceptanceModule() {
		full := filepath.Join(r.dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p15Git(t, r.dir, "add", "-A")
	p15Git(t, r.dir, "commit", "-q", "-m", "module")
	r.boot(t)
	return r
}

// p16Scenario queues the worker replies and installs the deterministic
// "repairs" of the acceptance scenario (WBS 16.181):
//
//	Phase 1: planning, atomic work, validation, checkpoint, DONE.
//	Phase 2: planning, INCOMPLETE, same-session continuation, INCOMPLETE
//	         above the threshold, context reset, COMPLETE, validation
//	         failure, FIX, PASS, DONE.
//	Phase 3: planning, dependency graph, phase validation failure, plan
//	         amendment, additional units, PASS, DONE.
func p16Scenario(t *testing.T, r *p16Rig, phases int) func() {
	r.queuePlan(r.planJSON(p16Unit{key: "p1a"}, p16Unit{key: "p1b"}))
	r.queue("complete")
	r.queue("complete")
	if phases >= 2 {
		r.queuePlan(r.planJSON(p16Unit{key: "p2a"}, p16Unit{key: "p2b", kind: "COMPOSITION", deps: []string{"p2a"}}))
		r.queueRaw(p16Reply("incomplete", "Half way.", ""), 1000)      // INCOMPLETE → same session
		r.queueRaw(p16Reply("incomplete", "Almost there.", ""), 60000) // continuation INCOMPLETE above threshold → reset
		r.queue("complete")                                            // reset continuation completes
		r.queue("complete")                                            // p2b first attempt (validation fails)
		r.queue("complete")                                            // FIX
		r.setCheck(t, "p2b", 1)
	}
	if phases >= 3 {
		r.queuePlan(r.planJSON(p16Unit{key: "p3a"}, p16Unit{key: "p3b"}, p16Unit{key: "p3c", kind: "INTEGRATION", deps: []string{"p3a", "p3b"}},
			p16Unit{key: "p3v", kind: "VALIDATION", deps: []string{"p3c"}}))
		for i := 0; i < 4; i++ {
			r.queue("complete")
		}
		r.queuePlan(r.planJSON(p16Unit{key: "p3r", kind: "INTEGRATION", deps: []string{"p3v"}}))
		r.queue("complete")
	}
	phaseFailed := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		switch {
		case a == execution.ActionDispatch && stage == "dispatch" && r.state(t) == "FIX":
			r.setCheck(t, "p2b", 0) // the worker's fix works
		case a == execution.ActionPhaseValidation && stage == "intent" && phases >= 3 && r.phaseStatus(t, 2) == "DONE" && !phaseFailed:
			phaseFailed = true
			r.setCheck(t, "phase", 1) // phase 3 validation fails once
		case a == execution.ActionSelectUnit && stage == "intent" && strings.Contains(r.unitStatuses(t, 3), "p3r:PENDING"):
			r.setCheck(t, "phase", 0) // the repair unit makes it pass
		}
		return nil
	})
	return func() { r.eng().SetStepHook(nil) }
}

// TestP16180FullAutonomousProject — WBS 16.180: the scheduler alone drives a
// multi-phase project (atomic / composition / integration / validation
// units, dependency graphs, continuations, a context reset, a validation
// failure + fix, a plan amendment) through final hardening to COMPLETED,
// with no manual workflow mutation.
func TestP16180FullAutonomousProject(t *testing.T) {
	r := p16AcceptanceRig(t, 3, func(c *config.Config) {
		c.Engine.SchedulerDisabled = false
		c.Engine.Interval = 20 * time.Millisecond
		c.Engine.Workers = 2
	})
	stop := p16Scenario(t, r, 3)
	defer stop()
	r.exec(t, "UPDATE project_settings SET auto_continue = true WHERE "+r.where())
	r.eng().Kick()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if s := r.execStatus(t); s == "COMPLETED" || s == "RECOVERY_REQUIRED" || s == "FAILED" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r.execStatus(t) != "COMPLETED" {
		t.Fatalf("execution %s (recovery %q) at %s\nhistory %s", r.execStatus(t), r.recoveryReason(t), r.state(t), r.statesSeq(t))
	}
	p16AssertAcceptance(t, r, 3)
}

// TestP16181Acceptance — WBS 16.181: the Phase 16 final acceptance scenario
// with every "verify throughout" item asserted.
func TestP16181Acceptance(t *testing.T) {
	r := p16AcceptanceRig(t, 3, nil)
	stop := p16Scenario(t, r, 3)
	defer stop()
	r.runUntil(t, execution.StepCompleted, 1000)
	p16AssertAcceptance(t, r, 3)
}

func p16AssertAcceptance(t *testing.T, r *p16Rig, phases int) {
	t.Helper()
	ctx := context.Background()
	hist := r.statesSeq(t)
	ex := r.execID.String()
	q := func(sql string) int { return r.count(t, sql) }
	// Phase 2: INCOMPLETE → same session → INCOMPLETE → reset → COMPLETE →
	// validation FAIL → FIX → PASS.
	for _, w := range []string{"CONTINUE_DECISION→SAME_SESSION→CONTINUE", "CONTINUE_DECISION→RESET_SESSION→CONTINUE",
		"VALIDATION_RESULT→FAIL→FIX", "PHASE_VALIDATE→PHASE_PLAN", "MORE_PHASES→FINAL_HARDENING", "FINAL_VALIDATE→FINAL_CHECKPOINT→PROJECT_DONE"} {
		if !strings.Contains(hist, w) {
			t.Fatalf("history lacks %s:\n%s", w, hist)
		}
	}
	// Worker COMPLETE never equals WorkUnit DONE: every DONE unit has a
	// passing validation run and a unit checkpoint.
	if n := q("SELECT count(*) FROM work_units w WHERE w." + r.where() + " AND w.status = 'DONE' AND w.claimed_execution_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM validation_runs v WHERE v.id = w.done_validation_run_id AND v.status = 'PASS')"); n != 0 {
		t.Fatalf("%d DONE units without passing validation", n)
	}
	// Worker-reported tests never replace deterministic validation: every
	// unit validation run executed commands.
	if n := q("SELECT count(*) FROM validation_runs v WHERE v.execution_id = '" + ex + "' AND v.status IN ('PASS','FAIL') AND NOT EXISTS (SELECT 1 FROM validation_command_results c WHERE c.run_id = v.id)"); n != 0 {
		t.Fatal("validation without executed commands")
	}
	// INCOMPLETE uses Phase 14 continuation policy (one same-session, one
	// fresh-session continuation); a reset created a new session.
	if q("SELECT count(*) FROM continuations WHERE "+r.where()+" AND strategy = 'same_session'") < 1 ||
		q("SELECT count(*) FROM continuations WHERE "+r.where()+" AND strategy = 'fresh_session'") < 1 || len(r.g.Sessions()) < 2 {
		t.Fatal("continuations / context reset")
	}
	// No worker dispatch without durable intent: every engine dispatch is
	// referenced by a step and preceded by a WORKER_DISPATCH_INTENT event.
	if n := q("SELECT count(*) FROM worker_dispatches d WHERE d." + r.where() + " AND d.continuation_id IS NULL AND NOT EXISTS (SELECT 1 FROM execution_steps s WHERE s.dispatch_id = d.id)"); n != 0 {
		t.Fatalf("%d dispatches without a step", n)
	}
	evs := r.events(t)
	if countOf(evs, "WORKER_DISPATCH_INTENT") < q("SELECT count(*) FROM worker_dispatches WHERE "+r.where()+" AND continuation_id IS NULL") {
		t.Fatal("dispatch without intent event")
	}
	// No DELIVERY_UNKNOWN request resent; no duplicate dispatch per step.
	if n := q("SELECT count(*) FROM (SELECT dispatch_id FROM execution_steps WHERE execution_id = '" + ex + "' AND dispatch_id IS NOT NULL AND action IN ('DISPATCH','PLAN_DISPATCH','HARDENING_FIX') AND detail->>'adopted' IS NULL GROUP BY dispatch_id HAVING count(*) > 1) x"); n != 0 {
		t.Fatal("dispatch recorded by two steps")
	}
	// Dependency-blocked units cannot execute; phases complete only with
	// all current-plan units DONE; plan amendment preserved history.
	if q("SELECT count(*) FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p."+r.where()+" AND ep.kind = 'AMENDMENT'") != 1 ||
		q("SELECT count(*) FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p."+r.where()+" AND ep.status = 'SUPERSEDED'") != 1 {
		t.Fatal("plan amendment")
	}
	for o := 1; o <= phases; o++ {
		if r.phaseStatus(t, o) != "DONE" || strings.Contains(r.unitStatuses(t, o), "PENDING") {
			t.Fatalf("phase %d %s units %s", o, r.phaseStatus(t, o), r.unitStatuses(t, o))
		}
	}
	// Final user phase does not directly complete the project; final
	// hardening (with a baseline) executes before final validation.
	iHard, iFinal := strings.Index(hist, "FINAL_HARDENING"), strings.Index(hist, "FINAL_VALIDATE")
	if iHard < 0 || iFinal < iHard {
		t.Fatal("final hardening not before final validation")
	}
	if q("SELECT count(*) FROM repository_checkpoints WHERE kind = 'HARDENING_BASELINE' AND status = 'CREATED' AND execution_id = '"+ex+"'") != 1 {
		t.Fatal("hardening baseline")
	}
	// DUP-1 consolidated (copy → tests → migrate → remove → validate →
	// checkpoint), DUP-2 skipped with its reason; skipped groups do not
	// appear consolidated.
	var groups string
	if err := r.h.DBScan(ctx, `SELECT string_agg(g.classification || ':' || g.status, ',' ORDER BY g.classification) FROM hardening_groups g
		JOIN hardening_passes p ON p.id = g.pass_id WHERE p.execution_id = '`+ex+`' AND g.round = 1`, &groups); err != nil {
		t.Fatal(err)
	}
	if groups != "SAFE:CONSOLIDATED,UNSAFE_IMPORT_CYCLE:SKIPPED" {
		t.Fatalf("groups %s", groups)
	}
	for _, f := range []string{"project/project.go", "phase/phase.go", "role/role.go"} {
		if strings.Contains(r.read(t, f), "func normalizeName") {
			t.Fatalf("%s keeps the duplicate", f)
		}
	}
	if !strings.Contains(r.read(t, "a/a.go"), "func slug") || !strings.Contains(r.read(t, "c/c.go"), "func slug") {
		t.Fatal("skipped DUP-2 modified")
	}
	if !strings.Contains(r.read(t, "stringutil/s.go"), "func Exclaim") || strings.Count(r.read(t, "stringutil/normalize_name.go"), "func NormalizeName") != 1 {
		t.Fatal("existing stringutil not reused / duplicate shared function")
	}
	if !strings.Contains(r.read(t, "project/project_test.go"), "TestProjectCreationNormalizesName") {
		t.Fatal("caller-specific test lost")
	}
	rep := r.report(t)
	for _, w := range []string{`"consolidated_groups":1`, `"skipped_groups":1`, `"import_cycle_failures":1`, `"termination":"FIXED_POINT"`, `"validation_result":"PASS"`} {
		if !strings.Contains(rep, w) {
			t.Fatalf("report lacks %s: %s", w, rep)
		}
	}
	// Hardening progress is separate from WBS progress; final validation
	// ran against the hardened repository; final checkpoint; COMPLETED
	// only after every gate; artifacts immutable.
	if countOf(evs, "FINAL_VALIDATION_PASSED") < 1 || countOf(evs, "PROJECT_COMPLETED") != 1 {
		t.Fatal("finalization events")
	}
	if q("SELECT count(*) FROM repository_checkpoints WHERE kind = 'FINAL' AND status = 'CREATED' AND execution_id = '"+ex+"'") != 1 {
		t.Fatal("final checkpoint")
	}
	if r.projectStatus(t) != "COMPLETED" || r.execStatus(t) != "COMPLETED" {
		t.Fatal("completion")
	}
	if _, err := r.h.DBExec(ctx, "UPDATE executions SET status = 'RUNNING' WHERE id = '"+ex+"'"); err == nil {
		t.Fatal("completed execution mutable")
	}
	// No browser is required: everything above ran through the engine.
}
