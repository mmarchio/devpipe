package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/invariants"
)

func p18Violations(rep invariants.Report) []string {
	var out []string
	for _, v := range rep.Violations {
		out = append(out, string(v.Invariant)+": "+v.Detail)
	}
	sort.Strings(out)
	return out
}

func p18HasInvariant(rep invariants.Report, id invariants.ID, projectID string) bool {
	for _, v := range rep.Violations {
		if v.Invariant == id && (projectID == "" || v.ProjectID == projectID) {
			return true
		}
	}
	return false
}

// T18.39 / T18.40 / T18.171 / T18.181 / T18.203 — a healthy completed
// project scans clean; every injected corruption is detected (cross-project
// references and missing checkpoint commits included); the scan alone
// changes nothing; a project-scoped scan reports only that project.
func TestT1839IntegrityScan(t *testing.T) {
	a := p17NewRig(t)
	a.queuePlan(a.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	a.queue("complete")
	a.queue("complete")
	a.runUntil(t, execution.StepCompleted, 200)
	b := &p16Rig{p15Rig: p15Build(t, a.h, a.g, a.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, b, p16Options{})
	b.queuePlan(b.planJSON(p16Unit{key: "x"}))
	b.queue("complete")
	b.runUntil(t, execution.StepCompleted, 100)
	ctx := context.Background()
	svc := a.h.App.Integrity()

	clean, err := svc.Scan(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !clean.OK() || len(clean.Checked) != len(invariants.Catalog()) {
		t.Fatalf("healthy system not clean (checked %d / %d): %v", len(clean.Checked), len(invariants.Catalog()), p18Violations(clean))
	}

	pa, pb := a.pid.String(), b.pid.String()
	ea := a.execID.String()
	id := func(sql string) string { return p18FirstID(t, a.h, sql) }
	unitA := id("SELECT id::text FROM work_units WHERE project_id = '" + pa + "' AND unit_key = 'a'")
	unitB2 := id("SELECT id::text FROM work_units WHERE project_id = '" + pa + "' AND unit_key = 'b'")
	unitX := id("SELECT id::text FROM work_units WHERE project_id = '" + pb + "' LIMIT 1")
	phaseB := id("SELECT id::text FROM phases WHERE project_id = '" + pb + "' LIMIT 1")
	planOther := id("SELECT ep.id::text FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p.project_id = '" + pb + "' LIMIT 1")
	dispA := id("SELECT id::text FROM worker_dispatches WHERE project_id = '" + pa + "' AND work_unit_id IS NOT NULL LIMIT 1")
	stepA := id("SELECT id::text FROM execution_steps WHERE execution_id = '" + ea + "' ORDER BY seq LIMIT 1")
	cpA := id("SELECT id::text FROM repository_checkpoints WHERE execution_id = '" + ea + "' AND kind = 'UNIT' AND target_id = '" + unitB2 + "'")
	before := dbFingerprint(t, a)

	// Inject corruption (as legacy / damaged data: foreign keys and
	// triggers bypassed). Unique indexes cannot be bypassed — ONE_CURRENT_UNIT
	// / ONE_ACTIVE_EXECUTION are enforced by them and still scanned.
	a.exec(t, `SET session_replication_role = replica;
		UPDATE executions SET status = 'RUNNING', current_work_unit_id = '`+unitA+`', current_phase_id = '`+phaseB+`' WHERE id = '`+ea+`';
		UPDATE work_units SET status = 'EXECUTING', claimed_execution_id = '`+ea+`' WHERE id = '`+unitA+`';
		UPDATE validation_runs SET status = 'FAIL' WHERE id = (SELECT done_validation_run_id FROM work_units WHERE id = '`+unitX+`');
		DELETE FROM repository_checkpoints WHERE kind = 'UNIT' AND target_id = '`+unitX+`';
		UPDATE execution_plans SET status = 'SUPERSEDED' WHERE id = (SELECT execution_plan_id FROM work_units WHERE id = '`+unitA+`');
		UPDATE worker_dispatches SET work_unit_id = '`+unitX+`' WHERE id = '`+dispA+`';
		UPDATE execution_steps SET execution_id = '`+uuid.NewString()+`' WHERE id = '`+stepA+`';
		INSERT INTO work_unit_dependencies (work_unit_id, depends_on_work_unit_id) VALUES ('`+unitX+`', '`+unitA+`');
		UPDATE phases SET status = 'EXECUTING' WHERE project_id = '`+pb+`';
		INSERT INTO phases (id, project_id, name, phase_order, status, created_at, updated_at)
			SELECT gen_random_uuid(), project_id, 'Second', 99, 'VALIDATING', now(), now() FROM phases WHERE id = '`+phaseB+`';
		UPDATE repository_checkpoints SET commit_sha = 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef' WHERE id = '`+cpA+`';
		UPDATE work_units SET execution_plan_id = '`+planOther+`' WHERE id = '`+unitB2+`';`)
	if dbFingerprint(t, a) == before {
		t.Fatal("setup: corruption not injected")
	}

	// The scan alone never mutates anything (WBS 18.40 / 18.181).
	pre := dbFingerprint(t, a)
	rep, err := svc.Scan(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dbFingerprint(t, a) != pre {
		t.Fatal("the scan modified the database")
	}
	want := map[invariants.ID]string{
		invariants.DoneRequiresValidation: pb, invariants.DoneRequiresCheckpoint: pb,
		invariants.CurrentUnitOnActivePlan: pa, invariants.RuntimeBindingValid: pa, invariants.NoOrphans: pa,
		invariants.DependencyRefsValid: pb, invariants.OneCurrentPhase: pb, invariants.CheckpointConsistent: pa,
		invariants.OwnershipConsistent: pa,
	}
	for inv, project := range want {
		if !p18HasInvariant(rep, inv, project) {
			t.Errorf("%s not detected for project %s; got %v", inv, project, p18Violations(rep))
		}
	}
	if !strings.Contains(strings.Join(p18Violations(rep), "\n"), "checkpoint commit deadbeef") {
		t.Errorf("missing checkpoint commit (18.203) not reported: %v", p18Violations(rep))
	}

	// Project scope: only project B's findings.
	pidB := b.pid
	scoped, err := svc.Scan(ctx, &pidB, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range scoped.Violations {
		if v.ProjectID != pb {
			t.Fatalf("project-scoped scan reported another project: %+v", v)
		}
	}
	// The same report through the API.
	code, raw := a.b.api(http.MethodGet, "/api/v1/system/integrity?project="+pb, nil)
	var api invariants.Report
	if code != 200 || json.Unmarshal(raw, &api) != nil || len(api.Violations) != len(scoped.Violations) {
		t.Fatalf("API: %d %.300s", code, raw)
	}
}

// p18SecondExecutingPhase corrupts the project: a second phase in
// EXECUTING (ONE_CURRENT_PHASE).
func p18SecondExecutingPhase(t *testing.T, r *p16Rig) {
	t.Helper()
	r.exec(t, "INSERT INTO phases (id, project_id, name, phase_order, status, created_at, updated_at) VALUES (gen_random_uuid(), '"+
		r.pid.String()+"', 'Rogue', 99, 'EXECUTING', now(), now())")
}

// T18.178 — corruption detected before a side effect prevents it: the
// worker request is never sent.
func TestT18178CorruptionPreventsSideEffect(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18BreakRepoAt(t, r, execution.ActionDispatch, func() { p18SecondExecutingPhase(t, r) }, "INVARIANT_VIOLATION", 1)
	if !strings.Contains(r.recoveryReason(t), "before worker dispatch") {
		t.Fatalf("reason %q", r.recoveryReason(t))
	}
}

// T18.179 — an unexpected durable state found right after a side effect
// (the validation run) enters recovery; the unit is not completed.
func TestT18179MismatchAfterSideEffect(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	injected := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionRunUnitValidation && s == "validation" && !injected {
			injected = true
			p18SecondExecutingPhase(t, r)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res, _ := r.eng().Run(context.Background(), r.execID, 100)
	if !injected || p18RecoveryCondition(t, r) != "INVARIANT_VIOLATION" || !strings.Contains(r.recoveryReason(t), "after validation") {
		t.Fatalf("condition %q reason %q; %s", p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	p18NotDone(t, r)
}
