package integration

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// e2eRig bundles a harness with the repositories and the DoD-question
// helpers used by the WBS 2.28 end-to-end acceptance test.
type e2eRig struct {
	h      *Harness
	proj   *dbrepo.ProjectRepository
	phases *dbrepo.PhaseRepository
	docs   *dbrepo.DocumentRepository
	plans  *dbrepo.PlanRepository
	units  *dbrepo.WorkUnitRepository
}

func newE2ERig(t *testing.T, h *Harness) *e2eRig {
	t.Helper()
	raw := h.App.DB().Raw()
	return &e2eRig{
		h:      h,
		proj:   dbrepo.NewProjectRepository(raw),
		phases: dbrepo.NewPhaseRepository(raw),
		docs:   dbrepo.NewDocumentRepository(raw),
		plans:  dbrepo.NewPlanRepository(raw),
		units:  dbrepo.NewWorkUnitRepository(raw),
	}
}

func (r *e2eRig) newUnit(t *testing.T, planID uuid.UUID, seq int, name string) *wbs.WorkUnit {
	t.Helper()
	ctx := context.Background()
	wu, err := wbs.NewWorkUnit(planID, seq, name, wbs.WorkUnitKindATOMIC)
	if err != nil {
		t.Fatalf("new unit: %v", err)
	}
	spec, err := wbs.NewWorkUnitSpecification(wbs.WorkUnitSpecification{
		Objective:          "implement " + name,
		AcceptanceCriteria: []string{"accepts valid input"},
		RequiredTests:      []string{"valid case"},
		Constraints:        []string{"touch only the target code"},
	})
	if err != nil {
		t.Fatalf("new spec: %v", err)
	}
	wu.Specification = spec
	if err := r.units.Create(ctx, wu); err != nil {
		t.Fatalf("create unit %s: %v", name, err)
	}
	return wu
}

// e2eAnswers is the complete set of DoD (Phase 2) answers captured from one
// application instance; a restarted instance must reproduce it exactly.
type e2eAnswers struct {
	projectName     string
	projectStatus   string
	settingsWorkDir string

	phaseNames  []string
	phaseOrders []int
	phaseStatus []string

	currentPhaseName  string
	wbsVersion        int
	wbsContent        string
	activePlanVersion int
	wbsVersionOfPlan  int

	unitNames     []string
	unitStatuses  []string
	unitDeps      map[string][]string // unit name -> sorted dependency names
	eligibleNames []string
	doneNames     []string

	progCompleted int
	progTotal     int
	progPercent   float64

	nextPhaseName string
}

// captureAnswers answers every DoD question (WBS 2.28 / Phase 2 DoD) against
// the rig's repositories.
func (r *e2eRig) captureAnswers(t *testing.T) *e2eAnswers {
	t.Helper()
	ctx := context.Background()
	ans := &e2eAnswers{}

	// What project is this?
	list, err := r.proj.List(ctx, persistence.ProjectListFilter{})
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("projects = %d, want 1", len(list))
	}
	ans.projectName = list[0].Name
	ans.projectStatus = list[0].Status.String()

	// Settings.
	var wd string
	if err := r.h.DBScan(ctx, "SELECT working_directory FROM project_settings", &wd); err != nil {
		t.Fatalf("settings row: %v", err)
	}
	ans.settingsWorkDir = wd

	// What phases exist? What order are they in?
	phases, err := r.phases.ListByProject(ctx, list[0].ID)
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	for _, p := range phases {
		ans.phaseNames = append(ans.phaseNames, p.Name)
		ans.phaseOrders = append(ans.phaseOrders, p.Order)
		ans.phaseStatus = append(ans.phaseStatus, p.Status.String())
	}

	// What is the current phase?
	cur, err := r.phases.GetNextIncomplete(ctx, list[0].ID)
	if err != nil {
		t.Fatalf("next incomplete: %v", err)
	}
	if cur == nil {
		t.Fatal("current phase = nil")
	}
	ans.currentPhaseName = cur.Name

	// What WBS defines this phase?
	doc, err := r.docs.GetByPhase(ctx, cur.ID.String())
	if err != nil {
		t.Fatalf("wbs doc: %v", err)
	}
	ans.wbsVersion = doc.Version
	ans.wbsContent = doc.Content

	// Which execution plan belongs to that WBS version?
	plans, err := r.plans.ListByPhase(ctx, cur.ID)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	ans.activePlanVersion = plans[0].Version
	ans.wbsVersionOfPlan = plans[0].WBSVersion

	// What work units exist? What does each unit depend upon?
	units, err := r.units.ListByPlan(ctx, plans[0].ID)
	if err != nil {
		t.Fatalf("list units: %v", err)
	}
	ans.unitDeps = map[string][]string{}
	for _, u := range units {
		ans.unitNames = append(ans.unitNames, u.Name)
		ans.unitStatuses = append(ans.unitStatuses, u.Status.String())
		deps, derr := r.units.GetDependencies(ctx, u.ID)
		if derr != nil {
			t.Fatalf("deps of %s: %v", u.Name, derr)
		}
		var depNames []string
		for _, d := range deps {
			depNames = append(depNames, d.Name)
		}
		ans.unitDeps[u.Name] = depNames
	}

	// Which units are structurally eligible? Which are DONE?
	elig, err := r.units.ListEligible(ctx, plans[0].ID)
	if err != nil {
		t.Fatalf("eligible: %v", err)
	}
	for _, u := range elig {
		ans.eligibleNames = append(ans.eligibleNames, u.Name)
	}
	for _, u := range units {
		if u.Status == wbs.WorkUnitStatusDONE {
			ans.doneNames = append(ans.doneNames, u.Name)
		}
	}

	// How far through the current WBS are we?
	prog := wbs.CalculateWorkProgress(units)
	ans.progCompleted = prog.Completed
	ans.progTotal = prog.Total
	ans.progPercent = prog.Percentage

	// What phase comes next (after the current one completes)?
	next, err := r.phases.GetNextIncomplete(ctx, list[0].ID)
	if err != nil {
		t.Fatalf("next phase: %v", err)
	}
	if next == nil {
		t.Fatal("next phase = nil")
	}
	ans.nextPhaseName = next.Name
	return ans
}

// pgxConnectBase connects to the base test database (helper shared by the
// e2e acceptance test's database lifecycle).
func pgxConnectBase(t *testing.T, base string) (*pgx.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return pgx.Connect(ctx, base)
}

func assertAnswersSame(t *testing.T, before, after *e2eAnswers, label string) {
	t.Helper()
	if after.projectName != before.projectName || after.projectStatus != before.projectStatus {
		t.Fatalf("%s: project = %s/%s, want %s/%s", label, after.projectName, after.projectStatus, before.projectName, before.projectStatus)
	}
	if after.settingsWorkDir != before.settingsWorkDir {
		t.Fatalf("%s: settings = %q, want %q", label, after.settingsWorkDir, before.settingsWorkDir)
	}
	if fmt.Sprint(after.phaseNames) != fmt.Sprint(before.phaseNames) ||
		fmt.Sprint(after.phaseOrders) != fmt.Sprint(before.phaseOrders) ||
		fmt.Sprint(after.phaseStatus) != fmt.Sprint(before.phaseStatus) {
		t.Fatalf("%s: phases = %v %v %v, want %v %v %v", label,
			after.phaseNames, after.phaseOrders, after.phaseStatus,
			before.phaseNames, before.phaseOrders, before.phaseStatus)
	}
	if after.currentPhaseName != before.currentPhaseName {
		t.Fatalf("%s: current phase = %q, want %q", label, after.currentPhaseName, before.currentPhaseName)
	}
	if after.wbsVersion != before.wbsVersion || after.wbsContent != before.wbsContent {
		t.Fatalf("%s: wbs = v%d/%q, want v%d/%q", label, after.wbsVersion, after.wbsContent, before.wbsVersion, before.wbsContent)
	}
	if after.activePlanVersion != before.activePlanVersion || after.wbsVersionOfPlan != before.wbsVersionOfPlan {
		t.Fatalf("%s: plan = v%d(wbs %d), want v%d(wbs %d)", label, after.activePlanVersion, after.wbsVersionOfPlan, before.activePlanVersion, before.wbsVersionOfPlan)
	}
	if fmt.Sprint(after.unitNames) != fmt.Sprint(before.unitNames) || fmt.Sprint(after.unitStatuses) != fmt.Sprint(before.unitStatuses) {
		t.Fatalf("%s: units = %v %v, want %v %v", label, after.unitNames, after.unitStatuses, before.unitNames, before.unitStatuses)
	}
	if fmt.Sprint(after.unitDeps) != fmt.Sprint(before.unitDeps) {
		t.Fatalf("%s: deps = %v, want %v", label, after.unitDeps, before.unitDeps)
	}
	if fmt.Sprint(after.eligibleNames) != fmt.Sprint(before.eligibleNames) || fmt.Sprint(after.doneNames) != fmt.Sprint(before.doneNames) {
		t.Fatalf("%s: eligible %v done %v, want eligible %v done %v", label, after.eligibleNames, after.doneNames, before.eligibleNames, before.doneNames)
	}
	if after.progCompleted != before.progCompleted || after.progTotal != before.progTotal || math.Abs(after.progPercent-before.progPercent) > 1e-9 {
		t.Fatalf("%s: progress = %d/%d %v%%, want %d/%d %v%%", label, after.progCompleted, after.progTotal, after.progPercent, before.progCompleted, before.progTotal, before.progPercent)
	}
	if after.nextPhaseName != before.nextPhaseName {
		t.Fatalf("%s: next phase = %q, want %q", label, after.nextPhaseName, before.nextPhaseName)
	}
}

// TestT228Phase2EndToEndAcceptance (WBS 2.28 + Phase 2 DoD) exercises the
// complete persistence hierarchy — project (RUNNING), settings, phases
// (current + pending), WBS, execution plan, work units, dependencies,
// progress — answers every DoD question, then restarts the application
// against the same database and verifies the reconstructed hierarchy
// answers identically.
func TestT228Phase2EndToEndAcceptance(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgxConnectBase(t, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_it_e2e_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		conn.Close(ctx)
		t.Skipf("create temp db: %v", err)
	}
	conn.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	// --- First boot: build the hierarchy entirely through repositories. ---
	h1 := NewHarnessOn(t, base, dbName)
	r := newE2ERig(t, h1)
	cctx := context.Background()

	p, err := project.New("Example", "end-to-end acceptance project")
	if err != nil {
		t.Fatalf("construct project: %v", err)
	}
	if err := r.proj.Create(cctx, p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Project RUNNING (the WBS 2.28 scenario state).
	p.Status = project.StatusRUNNING
	if err := r.proj.Update(cctx, p); err != nil {
		t.Fatalf("set running: %v", err)
	}

	settings, err := project.NewSettings(p.ID, "/wd/e2e", "", "", "", "", 8192, 0.8, false, 0, time.Minute)
	if err != nil {
		t.Fatalf("construct settings: %v", err)
	}
	if _, err := r.h.DBExec(cctx, insertSettingsSQL(p.ID)); err != nil {
		t.Fatalf("settings row: %v", err)
	}
	if settings.ProjectID != p.ID {
		t.Fatalf("settings bound to %v, want %v", settings.ProjectID, p.ID)
	}

	// Phases: 1 EXECUTING (current), 2 PENDING, 3 PENDING.
	p1, err := r.phases.Append(cctx, p.ID, "Phase 1", "")
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := r.phases.MarkStarted(cctx, p1.ID); err != nil {
		t.Fatalf("start 1: %v", err)
	}
	if _, err := r.phases.Append(cctx, p.ID, "Phase 2", ""); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if _, err := r.phases.Append(cctx, p.ID, "Phase 3", ""); err != nil {
		t.Fatalf("append 3: %v", err)
	}

	// WBS v1 for the current phase.
	wbsContent := "# WBS v1\n\nEnd-to-end acceptance scope."
	if _, err := r.docs.Create(cctx, p1.ID.String(), "WBS", wbsContent); err != nil {
		t.Fatalf("wbs doc: %v", err)
	}

	// Execution plan v1 derived from WBS v1.
	plan, err := wbs.NewExecutionPlan(p1.ID, 1)
	if err != nil {
		t.Fatalf("construct plan: %v", err)
	}
	if err := r.plans.Create(cctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// Work units A-D with dependencies A->C, B->C, C->D.
	a := r.newUnit(t, plan.ID, 1, "A")
	b := r.newUnit(t, plan.ID, 2, "B")
	c := r.newUnit(t, plan.ID, 3, "C")
	d := r.newUnit(t, plan.ID, 4, "D")
	if err := r.units.AddDependency(cctx, c.ID, a.ID); err != nil {
		t.Fatalf("dep a->c: %v", err)
	}
	if err := r.units.AddDependency(cctx, c.ID, b.ID); err != nil {
		t.Fatalf("dep b->c: %v", err)
	}
	if err := r.units.AddDependency(cctx, d.ID, c.ID); err != nil {
		t.Fatalf("dep c->d: %v", err)
	}
	// Two of four units DONE.
	if err := r.units.MarkDone(cctx, a.ID); err != nil {
		t.Fatalf("mark a: %v", err)
	}
	if err := r.units.MarkDone(cctx, b.ID); err != nil {
		t.Fatalf("mark b: %v", err)
	}

	// Pre-restart DoD answers.
	before := r.captureAnswers(t)
	if before.projectStatus != "RUNNING" {
		t.Fatalf("pre-restart status = %s, want RUNNING", before.projectStatus)
	}
	if len(before.phaseOrders) != 3 || before.phaseOrders[0] != 1 || before.phaseOrders[2] != 3 {
		t.Fatalf("phase orders = %v", before.phaseOrders)
	}
	if before.currentPhaseName != "Phase 1" || before.wbsVersion != 1 {
		t.Fatalf("current = %s wbs v%d", before.currentPhaseName, before.wbsVersion)
	}
	if before.progCompleted != 2 || before.progTotal != 4 || math.Abs(before.progPercent-50) > 1e-9 {
		t.Fatalf("progress = %d/%d %v%%, want 2/4 50%%", before.progCompleted, before.progTotal, before.progPercent)
	}
	if before.nextPhaseName != "Phase 1" {
		t.Fatalf("next phase = %q, want Phase 1 (current, not done)", before.nextPhaseName)
	}

	// --- Restart: stop the first instance, boot a fresh one on the DB. ---
	h1.Close()
	h2 := NewHarnessOn(t, base, dbName)
	if !h2.App.IsReady() {
		t.Fatal("restarted app not ready")
	}
	after := newE2ERig(t, h2).captureAnswers(t)
	assertAnswersSame(t, before, after, "post-restart")
}
