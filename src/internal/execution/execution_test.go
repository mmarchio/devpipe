package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/phase"
	"overseer/internal/role"
	"overseer/internal/state"
	"overseer/internal/wbs"
)

// ---- P15-01 / P15-02 ----

func TestP1501StatusParseAndSets(t *testing.T) {
	for _, s := range AllStatuses() {
		got, err := ParseExecutionStatus(s.String())
		if err != nil || got != s {
			t.Fatalf("parse %s: %v %v", s, got, err)
		}
		if s.IsActive() && s.IsTerminal() {
			t.Fatalf("%s both active and terminal", s)
		}
	}
	for _, bad := range []string{"", "running", "UNKNOWN", " READY"} {
		if _, err := ParseExecutionStatus(bad); !errors.Is(err, ErrInvalidStatus) {
			t.Fatalf("%q accepted", bad)
		}
	}
	active := map[ExecutionStatus]bool{ExecutionInitializing: true, ExecutionReady: true, ExecutionRunning: true, ExecutionPaused: true, ExecutionRecovery: true, ExecutionWaiting: true}
	term := map[ExecutionStatus]bool{ExecutionCompleted: true, ExecutionFailed: true, ExecutionCancelled: true}
	for _, s := range AllStatuses() {
		if s.IsActive() != active[s] || s.IsTerminal() != term[s] {
			t.Fatalf("classification of %s wrong", s)
		}
	}
	if len(ActiveStatuses()) != 6 {
		t.Fatal("active set")
	}
}

func TestP1502Transitions(t *testing.T) {
	ok := [][2]ExecutionStatus{
		{ExecutionInitializing, ExecutionReady}, {ExecutionReady, ExecutionRunning},
		{ExecutionInitializing, ExecutionFailed}, {ExecutionRunning, ExecutionPaused},
		{ExecutionPaused, ExecutionRunning}, {ExecutionRecovery, ExecutionRunning},
		{ExecutionRunning, ExecutionCompleted},
	}
	for _, e := range ok {
		if err := ValidateTransition(e[0], e[1]); err != nil {
			t.Fatalf("%v rejected", e)
		}
	}
	bad := [][2]ExecutionStatus{
		{ExecutionInitializing, ExecutionRunning}, {ExecutionReady, ExecutionInitializing},
		{ExecutionRunning, ExecutionReady}, {ExecutionInitializing, ExecutionPaused},
	}
	for _, e := range bad {
		if !errors.Is(ValidateTransition(e[0], e[1]), ErrIllegalTransition) {
			t.Fatalf("%v allowed", e)
		}
	}
	for _, term := range []ExecutionStatus{ExecutionCompleted, ExecutionFailed, ExecutionCancelled} {
		for _, to := range AllStatuses() {
			if CanTransition(term, to) {
				t.Fatalf("terminal %s -> %s", term, to)
			}
		}
	}
}

// ---- P15-03 ----

func validInput() NewExecutionInput {
	return NewExecutionInput{ProjectID: uuid.New(), Number: 1, StateMachineID: uuid.New(), StateMachineVersion: 2, StateMachineHash: "h", InitialPhaseID: uuid.New()}
}

func TestP1503NewExecution(t *testing.T) {
	e, err := NewExecution(validInput())
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != ExecutionInitializing || e.Version != 1 || e.Generation != 1 || e.Number != 1 || e.CurrentSessionID != nil || e.ID == uuid.Nil || e.RecoveryRequired() {
		t.Fatalf("bad execution %+v", e)
	}
	cases := map[string]func(*NewExecutionInput){
		"project": func(i *NewExecutionInput) { i.ProjectID = uuid.Nil },
		"machine": func(i *NewExecutionInput) { i.StateMachineID = uuid.Nil },
		"version": func(i *NewExecutionInput) { i.StateMachineVersion = 0 },
		"hash":    func(i *NewExecutionInput) { i.StateMachineHash = " " },
		"phase":   func(i *NewExecutionInput) { i.InitialPhaseID = uuid.Nil },
		"number":  func(i *NewExecutionInput) { i.Number = 0 },
	}
	want := map[string]error{"project": ErrProjectRequired, "machine": ErrStateMachineRequired, "version": ErrStateMachineRequired, "hash": ErrStateMachineRequired, "phase": ErrInitialPhaseRequired, "number": ErrInvalidNumber}
	for name, mut := range cases {
		in := validInput()
		mut(&in)
		if _, err := NewExecution(in); !errors.Is(err, want[name]) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// ---- P15-04 / P15-05 ----

func TestP1504Stages(t *testing.T) {
	path := []BootstrapStage{StageValidating, StageSnapshotting, StageInitializingFiles, StageInitializingRuntime, StagePreparingContext, StageFinalizing, StageComplete}
	for i := 0; i+1 < len(path); i++ {
		if !CanAdvanceStage(path[i], path[i+1], "") {
			t.Fatalf("%s->%s", path[i], path[i+1])
		}
		if CanAdvanceStage(path[i+1], path[i], "") {
			t.Fatal("backward allowed")
		}
		if !CanAdvanceStage(path[i], StageFailed, "") {
			t.Fatal("fail not allowed")
		}
	}
	if CanAdvanceStage(StageValidating, StageInitializingFiles, "") {
		t.Fatal("skip allowed")
	}
	if CanAdvanceStage(StageComplete, StageFailed, "") {
		t.Fatal("complete not terminal")
	}
	if !CanAdvanceStage(StageFailed, StageInitializingFiles, StageInitializingFiles) || CanAdvanceStage(StageFailed, StageFinalizing, StageInitializingFiles) {
		t.Fatal("resume edge")
	}
	for _, s := range AllStages() {
		if p, err := ParseStage(string(s)); err != nil || p != s {
			t.Fatal(s)
		}
	}
	if _, err := ParseStage("x"); err == nil {
		t.Fatal("bad stage")
	}
}

func TestP1505Codes(t *testing.T) {
	if len(AllCodes()) != 14 {
		t.Fatal("14 codes")
	}
	want := map[BootstrapErrorCode]FailureClass{
		CodeRoleInvalid: ClassConfiguration, CodeWorkdirInvalid: ClassConfiguration,
		CodeMaterializationFailed: ClassRecoverable, CodeCompressionFailed: ClassRecoverable,
		CodePersistenceFailed: ClassInfrastructure, CodeConcurrentBootstrap: ClassRecoverable,
	}
	for c, cl := range want {
		if DefaultClass(c) != cl {
			t.Fatalf("%s -> %s", c, DefaultClass(c))
		}
	}
	err := error(NewError(CodeMaterializationFailed, StageInitializingFiles, "x", os.ErrPermission))
	be, ok := AsBootstrapError(fmtWrap(err))
	if !ok || be.Code != CodeMaterializationFailed || !be.Class.Retryable() || !errors.Is(err, os.ErrPermission) {
		t.Fatal("as error")
	}
	if ClassConfiguration.Retryable() {
		t.Fatal("configuration retryable")
	}
}

func fmtWrap(err error) error { return errors.Join(errors.New("ctx"), err) }

// ---- P15-06 ----

func TestP1506ReadinessReport(t *testing.T) {
	r := &ExecutionReadinessReport{}
	r.Add(CategoryTemplates, CodePromptInvalid, "CONTINUE_WORK_UNIT", "template missing")
	r.Add(CategoryWorkingDir, CodeWorkdirInvalid, "/x", "missing")
	r.Add(CategoryRoles, CodeRoleInvalid, "ENGINEER", "disabled")
	r.Add(CategoryRoles, CodeRoleInvalid, "ENGINEER", "disabled")
	r.Warn(CategoryRepository, CodeRepositoryInvalid, "/x", "dirty")
	r.Finalize()
	if r.Ready || len(r.Errors) != 3 || len(r.Warnings) != 1 {
		t.Fatalf("%+v", r)
	}
	if r.Errors[0].Category != CategoryWorkingDir || r.Errors[1].Category != CategoryRoles || r.Errors[2].Category != CategoryTemplates {
		t.Fatalf("order %+v", r.Errors)
	}
	ok := (&ExecutionReadinessReport{}).Finalize()
	ok.Warn(CategoryRepository, CodeRepositoryInvalid, "", "dirty")
	ok.Finalize()
	if !ok.Ready {
		t.Fatal("warnings block")
	}
	ce := (&ExecutionReadinessReport{CompletionEligible: true}).Finalize()
	if ce.Ready || ce.FirstCode() != CodeNoPhase {
		t.Fatal("completion eligible")
	}
}

// ---- P15-07 ----

func TestP1507Settings(t *testing.T) {
	d := DefaultSettings()
	if d.DirtyPolicy != DirtyWarn || d.MaxBootstrapControlTransitions != 100 || d.RequireGitRepository || d.MaterializeStateFiles || d.Validate() != nil {
		t.Fatalf("%+v", d)
	}
	bad := Settings{DirtyPolicy: "x", MaxBootstrapControlTransitions: 0}
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "dirty_policy") || !strings.Contains(err.Error(), "max_bootstrap") || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("%v", err)
	}
	for _, p := range []string{"ALLOW", "WARN", "REJECT"} {
		if _, err := ParseDirtyPolicy(p); err != nil {
			t.Fatal(p)
		}
	}
}

// ---- P15-08 ----

func TestP1508WorkingDirectory(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	_ = os.Mkdir(proj, 0o755)
	file := filepath.Join(root, "f.txt")
	_ = os.WriteFile(file, []byte("x"), 0o644)
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	_ = os.Symlink(outside, link)

	if _, errs := CheckWorkingDirectory(filepath.Join(root, "missing"), nil); len(errs) != 1 || errs[0].Code != CodeWorkdirInvalid {
		t.Fatal("missing accepted")
	}
	if _, errs := CheckWorkingDirectory(file, nil); len(errs) != 1 || !strings.Contains(errs[0].Message, "not a directory") {
		t.Fatalf("file accepted %v", errs)
	}
	a, errs := CheckWorkingDirectory(proj, nil)
	b, errs2 := CheckWorkingDirectory(filepath.Join(root, "proj", "..", "proj", "."), nil)
	if len(errs)+len(errs2) != 0 || a != b {
		t.Fatalf("equivalent paths %q %q", a, b)
	}
	if _, errs := CheckWorkingDirectory(proj, []string{root}); len(errs) != 0 {
		t.Fatal("inside root rejected")
	}
	if _, errs := CheckWorkingDirectory(link, []string{root}); len(errs) != 1 {
		t.Fatal("symlink escape accepted")
	}
	if _, errs := CheckWorkingDirectory(filepath.Join(proj, "..", ".."), []string{root}); len(errs) != 1 {
		t.Fatal("traversal accepted")
	}
	if WithinAllowedRoots(root+"-sibling", []string{root}) {
		t.Fatal("string prefix match")
	}
	if _, errs := CheckWorkingDirectory("", nil); len(errs) != 1 {
		t.Fatal("empty accepted")
	}
}

// ---- P15-09 ----

func TestP1509RepositoryPolicy(t *testing.T) {
	notGit := RepositorySnapshot{WorkingDirectory: "/w"}
	if e, _ := EvaluateRepositoryPolicy(notGit, Settings{RequireGitRepository: true, DirtyPolicy: DirtyWarn}); len(e) != 1 {
		t.Fatal("git required")
	}
	if e, w := EvaluateRepositoryPolicy(notGit, DefaultSettings()); len(e)+len(w) != 0 {
		t.Fatal("non-git not required")
	}
	dirty := RepositorySnapshot{IsGitRepository: true, Dirty: true}
	for pol, want := range map[DirtyPolicy][2]int{DirtyAllow: {0, 0}, DirtyWarn: {0, 1}, DirtyReject: {1, 0}} {
		e, w := EvaluateRepositoryPolicy(dirty, Settings{DirtyPolicy: pol})
		if len(e) != want[0] || len(w) != want[1] {
			t.Fatalf("%s: %d %d", pol, len(e), len(w))
		}
	}
	h := "abc"
	s1 := RepositorySnapshot{IsGitRepository: true, HeadCommit: &h}
	s2 := s1
	s2.Dirty = true
	if s1.Hash() == s2.Hash() {
		t.Fatal("hash")
	}
}

var _ RepositoryInspector = fakeInspector{}

type fakeInspector struct{}

func (fakeInspector) Inspect(context.Context, string) (RepositorySnapshot, error) {
	return RepositorySnapshot{}, nil
}

// ---- P15-11 ----

func ph(order int, st phase.PhaseStatus) *phase.Phase {
	return &phase.Phase{ID: uuid.New(), Order: order, Status: st, Name: "P"}
}

func unit(plan uuid.UUID, seq int, st wbs.WorkUnitStatus) *wbs.WorkUnit {
	return &wbs.WorkUnit{ID: uuid.New(), ExecutionPlanID: plan, Sequence: seq, Status: st}
}

func TestP1511Routing(t *testing.T) {
	p1, p2, p3, p4 := ph(1, phase.StatusDONE), ph(2, phase.StatusDONE), ph(3, phase.StatusPENDING), ph(4, phase.StatusPENDING)
	f := ResolveRouting(RoutingInput{Phases: []*phase.Phase{p4, p2, p3, p1}, CurrentWBSVersion: 1})
	if f.Phase != p3 || f.PlanState != PlanMissing {
		t.Fatal("phase 3 / missing")
	}
	if pref := f.OutcomePreference(); pref[0] != OutcomePlanRequired || pref[len(pref)-1] != OutcomeProceed {
		t.Fatal(pref)
	}
	if f := ResolveRouting(RoutingInput{Phases: []*phase.Phase{p1, p2}}); !f.CompletionEligible {
		t.Fatal("completion eligible")
	}
	// appended phase
	if f := ResolveRouting(RoutingInput{Phases: []*phase.Phase{p1, p2, ph(3, phase.StatusDONE), p4}}); f.Phase != p4 {
		t.Fatal("appended")
	}
	plan := &wbs.ExecutionPlan{ID: uuid.New(), PhaseID: p3.ID, WBSVersion: 7, Version: 1, Status: wbs.StatusREADY}
	if f := ResolveRouting(RoutingInput{Phases: []*phase.Phase{p3}, CurrentWBSVersion: 8, Plans: []*wbs.ExecutionPlan{plan}}); f.PlanState != PlanStale {
		t.Fatal("stale")
	}
	gen := *plan
	gen.Status = wbs.PlanStatusGENERATING
	gen.WBSVersion = 8
	if f := ResolveRouting(RoutingInput{Phases: []*phase.Phase{p3}, CurrentWBSVersion: 8, Plans: []*wbs.ExecutionPlan{&gen}}); f.PlanState != PlanInvalid {
		t.Fatal("invalid")
	}
	a, b, c, d := unit(plan.ID, 1, wbs.WorkUnitStatusDONE), unit(plan.ID, 2, wbs.WorkUnitStatusDONE), unit(plan.ID, 3, wbs.WorkUnitStatusREADY), unit(plan.ID, 4, wbs.WorkUnitStatusPENDING)
	in := RoutingInput{Phases: []*phase.Phase{p3}, CurrentWBSVersion: 7, Plans: []*wbs.ExecutionPlan{plan}, Units: []*wbs.WorkUnit{d, c, b, a}, EligibleUnits: []*wbs.WorkUnit{c}}
	f = ResolveRouting(in)
	if f.PlanState != PlanCurrent || f.NextUnit != c || f.DoneUnits != 2 || f.TotalUnits != 4 {
		t.Fatalf("partial %+v", f)
	}
	if pref := f.OutcomePreference(); strings.Join(pref, ",") != "PLAN_READY,READY,PROCEED" {
		t.Fatal(pref)
	}
	// blocked dependency: nothing eligible
	in.EligibleUnits = nil
	if f := ResolveRouting(in); !f.NoExecutableUnit || f.NextUnit != nil {
		t.Fatal("blocked dependency selected a unit")
	}
	// in-progress wins
	d.Status = wbs.WorkUnitStatusINCOMPLETE
	if f := ResolveRouting(in); f.NextUnit != d {
		t.Fatal("in-progress precedence")
	}
	// all done
	for _, u := range []*wbs.WorkUnit{c, d} {
		u.Status = wbs.WorkUnitStatusDONE
	}
	f = ResolveRouting(in)
	if !f.AllUnitsDone || f.OutcomePreference()[0] != OutcomeWorkComplete {
		t.Fatal("all done")
	}
}

// ---- P15-12 / P15-13 ----

func machine(states []state.StateSnapshot, ts []state.TransitionSnapshot, initial string) *state.StateMachineSnapshot {
	m := &state.StateMachineSnapshot{MachineID: uuid.New(), Version: 1, Hash: "h", States: states, Transitions: ts}
	for _, s := range states {
		if s.Key == initial {
			m.InitialState = s
		}
	}
	return m
}

func st(key string, kind state.StateKind) state.StateSnapshot {
	return state.StateSnapshot{Key: key, Kind: string(kind), Enabled: true, Terminal: kind == state.KindTerminal}
}

func tr(from, outcome, to string) state.TransitionSnapshot {
	return state.TransitionSnapshot{From: from, Outcome: outcome, To: to, Enabled: true}
}

func defaultLike() *state.StateMachineSnapshot {
	return machine([]state.StateSnapshot{
		st("PROJECT_INIT", state.KindControl), st("PHASE_PLAN", state.KindAction),
		st("WORK_UNIT_PREPARE", state.KindControl), st("IMPLEMENT", state.KindAction),
		st("PHASE_VALIDATE", state.KindValidation), st("ORPHAN", state.KindAction),
	}, []state.TransitionSnapshot{
		tr("PROJECT_INIT", "PROCEED", "PHASE_PLAN"),
		tr("PROJECT_INIT", "PLAN_REQUIRED", "PHASE_PLAN"),
		tr("PROJECT_INIT", "PLAN_READY", "WORK_UNIT_PREPARE"),
		tr("PROJECT_INIT", "WORK_COMPLETE", "PHASE_VALIDATE"),
		tr("WORK_UNIT_PREPARE", "READY", "IMPLEMENT"),
	}, "PROJECT_INIT")
}

func TestP1512Reachable(t *testing.T) {
	keys := []string{}
	for _, s := range ReachableStates(defaultLike()) {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, ",") != "IMPLEMENT,PHASE_PLAN,PHASE_VALIDATE,PROJECT_INIT,WORK_UNIT_PREPARE" {
		t.Fatal(keys)
	}
}

func TestP1513ControlResolution(t *testing.T) {
	m := defaultLike()
	p, err := ResolveFirstActionable(m, RoutingFacts{PlanState: PlanMissing}, 100)
	if err != nil || p.FirstActionable.Key != "PHASE_PLAN" || p.InitialState.Key != "PROJECT_INIT" || len(p.Transitions) != 1 {
		t.Fatalf("missing plan %v %+v", err, p)
	}
	nu := &wbs.WorkUnit{ID: uuid.New()}
	p, err = ResolveFirstActionable(m, RoutingFacts{PlanState: PlanCurrent, NextUnit: nu}, 100)
	if err != nil || p.FirstActionable.Key != "IMPLEMENT" || len(p.Transitions) != 2 || p.Transitions[0].To != "WORK_UNIT_PREPARE" {
		t.Fatalf("plan ready %v %+v", err, p)
	}
	p, _ = ResolveFirstActionable(m, RoutingFacts{PlanState: PlanCurrent, AllUnitsDone: true}, 100)
	if p.FirstActionable.Key != "PHASE_VALIDATE" {
		t.Fatal("all done")
	}
	// determinism
	p2, _ := ResolveFirstActionable(m, RoutingFacts{PlanState: PlanCurrent, AllUnitsDone: true}, 100)
	if p2.FirstActionable.Key != p.FirstActionable.Key {
		t.Fatal("deterministic")
	}
	// custom machine
	custom := machine([]state.StateSnapshot{st("BOOT", state.KindControl), st("INSPECT", state.KindControl), st("PLAN", state.KindAction)},
		[]state.TransitionSnapshot{tr("BOOT", "PROCEED", "INSPECT"), tr("INSPECT", "PROCEED", "PLAN")}, "BOOT")
	p, err = ResolveFirstActionable(custom, RoutingFacts{PlanState: PlanMissing}, 100)
	if err != nil || p.FirstActionable.Key != "PLAN" || p.InitialState.Key != "BOOT" {
		t.Fatalf("custom %v", err)
	}
	// cycle
	cyc := machine([]state.StateSnapshot{st("A", state.KindControl), st("B", state.KindControl)},
		[]state.TransitionSnapshot{tr("A", "PROCEED", "B"), tr("B", "PROCEED", "A")}, "A")
	if _, err := ResolveFirstActionable(cyc, RoutingFacts{}, 10); !errors.Is(err, ErrControlResolution) || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("cycle %v", err)
	}
	// dead end
	dead := machine([]state.StateSnapshot{st("A", state.KindControl)}, nil, "A")
	if _, err := ResolveFirstActionable(dead, RoutingFacts{}, 10); !errors.Is(err, ErrControlResolution) {
		t.Fatal("dead end")
	}
	// terminal
	term := machine([]state.StateSnapshot{st("A", state.KindControl), st("Z", state.KindTerminal)}, []state.TransitionSnapshot{tr("A", "PROCEED", "Z")}, "A")
	if _, err := ResolveFirstActionable(term, RoutingFacts{}, 10); !errors.Is(err, ErrControlResolution) {
		t.Fatal("terminal")
	}
}

// ---- P15-14 ----

func content() ConfigurationContent {
	m := *defaultLike()
	m.MachineID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	return ConfigurationContent{
		ProjectVersion: 3, WorkingDirectory: "/repo",
		WBS:          WBSSnapshot{PhaseID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Version: 4, Hash: "w"},
		StateMachine: m,
		Roles: []role.RoleSnapshot{
			{RoleKey: "PLANNER", RoleVersion: 1, SystemPrompt: role.PromptSnapshot{Version: 1, Content: "plan", ContentHash: "p"}},
			{RoleKey: "ENGINEER", RoleVersion: 2},
		},
		PromptTemplates: []PromptTemplateSnapshot{{Key: "PLAN_PHASE", Version: 1}, {Key: "IMPLEMENT_WORK_UNIT", Version: 2}},
		Compression:     CompressionConfigurationSnapshot{Provider: "fake", Model: "m1", APIKeyEnv: "COMP_KEY", ConfigHash: "c"},
		Worker:          WorkerConfigurationSnapshot{Provider: "hermes", Model: "w1", Endpoint: StripUserinfo("https://user:s3cret@gw.example/x"), AuthSecretEnv: "HERMES_TOKEN"},
		Continuation:    ContinuationConfigurationSnapshot{ContextThreshold: 0.8, ContextWindow: 64000},
		Execution:       DefaultSettings(),
	}
}

func TestP1514ConfigSnapshot(t *testing.T) {
	c := content()
	b1, err := EncodeContent(c)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodeContent(b1)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := EncodeContent(dec)
	if string(b1) != string(b2) {
		t.Fatal("not byte-stable")
	}
	if dec.Roles[0].RoleKey != "ENGINEER" || dec.StateMachine.InitialState.Key != "PROJECT_INIT" || dec.Execution.BootstrapLease != 60*time.Second {
		t.Fatal("reconstruction")
	}
	h1, _ := ContentHash(c)
	h2, _ := ContentHash(content())
	if h1 != h2 {
		t.Fatal("same config different hash")
	}
	muts := []func(*ConfigurationContent){
		func(c *ConfigurationContent) { c.Roles[0].SystemPrompt.Version = 2 },
		func(c *ConfigurationContent) { c.StateMachine.Version = 3 },
		func(c *ConfigurationContent) { c.Compression.Model = "m2" },
		func(c *ConfigurationContent) { c.Continuation.ContextThreshold = 0.9 },
		func(c *ConfigurationContent) { c.PromptTemplates[0].Version = 9 },
		func(c *ConfigurationContent) { c.WBS.Version = 5 },
	}
	for i, m := range muts {
		x := content()
		m(&x)
		if h, _ := ContentHash(x); h == h1 {
			t.Fatalf("mutation %d same hash", i)
		}
	}
	if strings.Contains(string(b1), "s3cret") || !strings.Contains(string(b1), "HERMES_TOKEN") {
		t.Fatal("secret handling")
	}
}

// ---- P15-15 ----

func TestP1515PlanAndSources(t *testing.T) {
	a := SourceVersions{ProjectVersion: 1, WBSVersion: 4, RoleHashes: map[string]string{"ENGINEER": "x"}}
	b := a
	b.RoleHashes = map[string]string{"ENGINEER": "x"}
	if !a.Equal(b) {
		t.Fatal("equal")
	}
	b.WBSVersion = 5
	b.RoleHashes = map[string]string{"ENGINEER": "y"}
	if d := a.Diff(b); strings.Join(d, ",") != "role_hashes,wbs_version" {
		t.Fatal(d)
	}
	if (SourceVersions{RoleHashes: map[string]string{}}).Diff(SourceVersions{}) != nil {
		t.Fatal("nil vs empty map")
	}
	p := BootstrapPlan{WBSVersion: 4, MachineVersion: 3, FirstActionable: "PHASE_PLAN", Sources: a}
	if len(p.Steps()) != 11 || p.Steps()[2] != "Bind WBS v4" || p.Steps()[3] != "Bind workflow v3" {
		t.Fatal(p.Steps())
	}
	q := p
	if p.Hash() != q.Hash() {
		t.Fatal("hash")
	}
	q.Sources.ProjectVersion = 2
	if p.Hash() == q.Hash() {
		t.Fatal("source change kept hash")
	}
}

// ---- P15-16 ----

func TestP1516Renderers(t *testing.T) {
	head, br := "abc", "main"
	ps := RenderProjectState(ProjectStateInput{ProjectName: "Trader", ExecutionNumber: 1, ExecutionStatus: "INITIALIZING", PhaseOrder: 2, PhaseName: "Persistence", WorkflowState: "PHASE_PLAN",
		Repository: RepositorySnapshot{WorkingDirectory: "/r", IsGitRepository: true, HeadCommit: &head, Branch: &br}})
	for _, w := range []string{"Trader", "#1", "2 — Persistence", "PHASE_PLAN", "branch main", "HEAD abc"} {
		if !strings.Contains(ps, w) {
			t.Fatalf("project state missing %q", w)
		}
	}
	if ps != RenderProjectState(ProjectStateInput{ProjectName: "Trader", ExecutionNumber: 1, ExecutionStatus: "INITIALIZING", PhaseOrder: 2, PhaseName: "Persistence", WorkflowState: "PHASE_PLAN",
		Repository: RepositorySnapshot{WorkingDirectory: "/r", IsGitRepository: true, HeadCommit: &head, Branch: &br}}) {
		t.Fatal("deterministic")
	}
	phs := RenderPhaseState(PhaseStateInput{PhaseOrder: 2, PhaseName: "Persistence", WBSVersion: 4, PlanStatus: PlanMissing, DoneUnits: 0, TotalUnits: 0})
	if !strings.Contains(phs, "missing — planning required") || !strings.Contains(phs, "v4") {
		t.Fatal(phs)
	}
	ts := RenderTestState("P", nil)
	if !strings.Contains(ts, "No deterministic validation recorded") || strings.Contains(strings.ToLower(ts), "passed") {
		t.Fatal(ts)
	}
}

// ---- P15-17 / P15-18 ----

func TestP1517Checkpoint(t *testing.T) {
	v1 := uuid.New()
	c := BootstrapCheckpoint{ExecutionID: uuid.New(), ConfigurationHash: "c", StateFiles: []FileRef{{Type: "PROJECT_STATE", VersionID: v1, ContentHash: "a"}}, FirstActionableState: "PHASE_PLAN"}
	if len(VerifyCheckpoint(c, c)) != 0 {
		t.Fatal("agree")
	}
	o := c
	o.StateFiles = []FileRef{{Type: "PROJECT_STATE", VersionID: v1, ContentHash: "b"}}
	o.ConfigurationHash = "d"
	if p := VerifyCheckpoint(c, o); len(p) != 2 {
		t.Fatal(p)
	}
	o.StateFiles = nil
	if p := VerifyCheckpoint(c, o); len(p) != 2 || !strings.Contains(strings.Join(p, ";"), "missing") {
		t.Fatal(p)
	}
	if c.Hash() == o.Hash() {
		t.Fatal("hash")
	}
}

func TestP1518Activation(t *testing.T) {
	good := ActivationInput{ExecutionStatus: ExecutionReady, CheckpointVerified: true, ProjectStatus: "READY", RuntimeExists: true, RuntimeBound: true, FirstActionableKnown: true, ContextSnapshotKnown: true}
	if p, err := ValidateActivation(good); err != nil || len(p) != 0 {
		t.Fatal(p, err)
	}
	bad := good
	bad.CheckpointVerified = false
	if p, _ := ValidateActivation(bad); len(p) != 1 {
		t.Fatal("missing checkpoint")
	}
	all := ActivationInput{ExecutionStatus: ExecutionInitializing, ProjectStatus: "DRAFT"}
	if p, _ := ValidateActivation(all); len(p) != 6 {
		t.Fatal(p)
	}
	if _, err := ValidateActivation(ActivationInput{ExecutionStatus: ExecutionRunning}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("idempotent")
	}
}

// ---- P15-19 / P15-20 ----

func TestP1519Views(t *testing.T) {
	r := &ExecutionReadinessReport{}
	r.Add(CategoryRoles, CodeRoleInvalid, "ENGINEER", "ENGINEER role has no system prompt.")
	r.Add(CategoryTemplates, CodePromptInvalid, "CONTINUE_WORK_UNIT", "CONTINUE_WORK_UNIT template is missing.")
	r.Finalize()
	v := NewReadinessView(r)
	if v.Ready || v.Headline != "Cannot Start Project — 2 configuration issues" || len(v.Issues) != 2 || len(v.Checks) != len(Categories()) {
		t.Fatalf("%+v", v)
	}
	ok := NewReadinessView((&ExecutionReadinessReport{}).Finalize())
	if ok.Headline != "Ready to Start" {
		t.Fatal(ok.Headline)
	}
	av := NewActiveExecutionView(ActiveExecutionInput{Execution: Execution{Number: 1, Status: ExecutionRunning}, DoneUnits: 8, TotalUnits: 20, PhaseOrder: 7, PhaseName: "Execution Engine", WorkflowState: "PHASE_PLAN", Role: "ARCHITECT"})
	if av.ProgressPercent != 40 || av.ProgressLabel != "8 / 20 tasks complete" || av.Worker != "Not yet dispatched" || av.Title != "Execution #1" || av.Continuations != 0 {
		t.Fatalf("%+v", av)
	}
	pv := NewPreviewView("Trader", "READY", &BootstrapPlan{PhaseOrder: 7, PhaseName: "Execution Engine", WBSVersion: 4, PlanState: PlanMissing, InitialState: "PROJECT_INIT", FirstActionable: "PHASE_PLAN", Role: "ARCHITECT", WorkingDirectory: "/repos/trader"}, (&ExecutionReadinessReport{}).Finalize())
	if pv.ExecutionPlan != "Missing → planning required" || pv.Readiness != "READY" || pv.InitialPhase != "7 — Execution Engine" {
		t.Fatalf("%+v", pv)
	}
}

func TestP1520Events(t *testing.T) {
	// 10 bootstrap types (Phase 15) + 33 engine types (P16-14) + 2
	// repository types (P18-41: HEAD accepted, worker-run evidence) + the
	// task replan + the task requeue (requested, applied) + the task
	// analysis / fix (started, finished each) + the repeated action + pause
	// after task (requested, cancelled, applied).
	if len(AllEventTypes()) != 56 {
		t.Fatalf("%d types", len(AllEventTypes()))
	}
	if _, err := ParseEventType("NOPE"); err == nil {
		t.Fatal("bad type")
	}
	ex := uuid.New()
	t0 := time.Unix(1000, 0)
	ev := []Event{
		{Type: EventBootstrapRequested, Payload: map[string]any{}},
		{Type: EventBootstrapFailed, Payload: map[string]any{"code": "ROLE_INVALID", "stage": "VALIDATING"}},
		{Type: EventBootstrapRequested, Payload: map[string]any{}},
		{Type: EventReadinessValidated, ExecutionID: &ex, CreatedAt: t0},
		{Type: EventBootstrapFailed, ExecutionID: &ex, Payload: map[string]any{"code": "MATERIALIZATION_FAILED", "stage": "INITIALIZING_FILES"}},
		{Type: EventBootstrapRequested, Payload: map[string]any{"retry": true}},
		{Type: EventBootstrapCompleted, ExecutionID: &ex, CreatedAt: t0.Add(2 * time.Second)},
	}
	m := AggregateMetrics(ev)
	if m.Attempts != 3 || m.Successes != 1 || m.Failures != 2 || m.ReadinessFailures != 1 || m.Retries != 1 || m.FailuresByCode["MATERIALIZATION_FAILED"] != 1 || m.DurationTotalMS != 2000 {
		t.Fatalf("%+v", m)
	}
}
