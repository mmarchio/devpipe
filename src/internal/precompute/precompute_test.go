package precompute

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func implGraph() Graph {
	st := func(k, tmpl string) StateNode { return StateNode{Key: k, Enabled: true, TemplateKey: tmpl} }
	return Graph{MachineVersion: 1, States: map[string]StateNode{
		"IMPLEMENT": st("IMPLEMENT", "IMPLEMENT_WORK_UNIT"), "VALIDATE": st("VALIDATE", "VALIDATE_WORK_UNIT"),
		"CONTINUE": st("CONTINUE", "CONTINUE_WORK_UNIT"), "FIX": st("FIX", "FIX_WORK_UNIT"),
		"BLOCKED": st("BLOCKED", ""), "DONE": {Key: "DONE", Enabled: true, Terminal: true},
		"ARCHIVE": {Key: "ARCHIVE", Enabled: false},
	}, Edges: []Edge{
		{From: "IMPLEMENT", To: "VALIDATE", Outcome: "COMPLETE", Enabled: true},
		{From: "IMPLEMENT", To: "CONTINUE", Outcome: "INCOMPLETE", Enabled: true},
		{From: "IMPLEMENT", To: "BLOCKED", Outcome: "BLOCKED", Enabled: true},
		{From: "IMPLEMENT", To: "FIX", Outcome: "FAILED", Enabled: false},
		{From: "IMPLEMENT", To: "ARCHIVE", Outcome: "ARCHIVE", Enabled: true},
		{From: "VALIDATE", To: "DONE", Outcome: "PASS", Enabled: true},
		{From: "VALIDATE", To: "FIX", Outcome: "FAIL", Enabled: true},
		{From: "FIX", To: "VALIDATE", Outcome: "COMPLETE", Enabled: true},
		{From: "CONTINUE", To: "IMPLEMENT", Outcome: "RESUME", Enabled: true},
	}}
}

func keys(rs []Reach) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.StateKey)
	}
	return strings.Join(s, ",")
}

func TestT112Request(t *testing.T) {
	r := Request{ProjectID: uuid.New(), RuntimeStateID: uuid.New(), Trigger: TriggerWorkerDispatch, MaxDepth: 2, MaxCandidates: 8}
	if r.Validate() != nil || r.Trigger != TriggerWorkerDispatch {
		t.Fatal("T11.2.1/T11.2.5")
	}
	for name, mut := range map[string]func(*Request){
		"T11.2.2 project": func(r *Request) { r.ProjectID = uuid.Nil },
		"T11.2.3 depth":   func(r *Request) { r.MaxDepth = 9 },
		"T11.2.4 limit":   func(r *Request) { r.MaxCandidates = -1 },
		"trigger":         func(r *Request) { r.Trigger = "SOMETIME" },
	} {
		c := r
		mut(&c)
		if !errors.Is(c.Validate(), ErrInvalidRequest) {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestT113Settings(t *testing.T) {
	d := DefaultSettings()
	if !d.Enabled || d.Depth != 2 || d.MaxCandidates != 8 || d.Concurrency != 2 || !d.CompressionEnabled || !d.StaticFragments || !d.DuringWorkerExecution || d.Validate() != nil {
		t.Fatalf("T11.3.1 %+v", d)
	}
	bad := d
	bad.Depth = 0
	if bad.Validate() == nil {
		t.Fatal("T11.3.3")
	}
	bad = d
	bad.Concurrency = 0
	if bad.Validate() == nil {
		t.Fatal("T11.3.4")
	}
}

func TestT115AndT116Reachability(t *testing.T) {
	g := implGraph()
	if got := keys(g.Reachable("IMPLEMENT", 1)); got != "BLOCKED,VALIDATE,CONTINUE" {
		t.Fatalf("T11.5.1-T11.5.3/T11.6.1 depth 1: %s", got)
	}
	d2 := g.Reachable("IMPLEMENT", 2)
	if got := keys(d2); got != "BLOCKED,VALIDATE,CONTINUE,FIX,DONE" {
		t.Fatalf("T11.6.2 depth 2: %s", got)
	}
	for _, r := range d2 {
		if r.StateKey == "FIX" && (r.Depth != 2 || strings.Join(r.Path, "|") != "COMPLETE>VALIDATE|FAIL>FIX") {
			t.Fatalf("T11.7.2 path %+v", r)
		}
	}
	if got := keys(g.Reachable("IMPLEMENT", 5)); got != "BLOCKED,VALIDATE,CONTINUE,FIX,DONE" {
		t.Fatalf("T11.5.5/T11.6.4 cycles: %s", got)
	}
	if len(g.Reachable("IMPLEMENT", 0)) != 0 {
		t.Fatal("T11.6.3")
	}
	v2 := g
	v2.MachineVersion = 2
	v2.Edges = []Edge{{From: "IMPLEMENT", To: "DONE", Outcome: "COMPLETE", Enabled: true}}
	if keys(v2.Reachable("IMPLEMENT", 2)) != "DONE" {
		t.Fatal("T11.5.4 version graph respected")
	}
	if !g.HasPath("IMPLEMENT", []string{"COMPLETE>VALIDATE", "FAIL>FIX"}) || g.HasPath("IMPLEMENT", []string{"FAILED>FIX"}) {
		t.Fatal("HasPath")
	}
}

func TestT117To119Classification(t *testing.T) {
	reg := DefaultRegistry()
	base := Analysis{HasRole: true, HasTemplate: true, UnitBound: true, HasUnit: true, UnitEligible: true, ActiveTaskExists: true, StaticFilesOK: true}
	impl := base
	impl.TemplateKey, impl.Purpose = "IMPLEMENT_WORK_UNIT", "IMPLEMENTATION"
	if r, known, miss, _, _ := Classify(impl, reg); r != FullyKnown || len(miss) != 0 || len(known) == 0 {
		t.Fatal("T11.9.1")
	}
	val := base
	val.TemplateKey, val.Purpose = "VALIDATE_WORK_UNIT", "VALIDATION"
	r, _, miss, _, _ := Classify(val, reg)
	if r != PartiallyKnown || len(miss) != 2 || miss[0] != InputImplementationResult {
		t.Fatalf("T11.9.2/T11.8.3/T11.25.1 %v %v", r, miss)
	}
	noRole := impl
	noRole.HasRole = false
	if r, _, _, structural, _ := Classify(noRole, reg); r != Unknown || len(structural) == 0 {
		t.Fatal("T11.9.3")
	}
	blocked := impl
	blocked.UnitEligible = false
	if r, _, _, _, _ := Classify(blocked, reg); r != Unknown {
		t.Fatal("T11.31.2 blocked unit")
	}
	opt := impl
	opt.OptionalMissing = []string{"file:TEST_STATE"}
	if r, _, _, _, _ := Classify(opt, reg); r != FullyKnown {
		t.Fatal("T11.8.4 optional dependency demoted")
	}
	ph := impl
	ph.ActiveTaskExists = false
	r, _, miss, _, placeholder := Classify(ph, reg)
	if r != PartiallyKnown || !placeholder || len(miss) != 1 || miss[0] != InputActiveTask {
		t.Fatal("Decision 5 placeholder classification")
	}
	cust := base
	cust.TemplateKey, cust.Purpose = "MY_TEMPLATE", "UNREGISTERED"
	if r, _, miss, _, _ := Classify(cust, reg); r != PartiallyKnown || miss[0] != InputCustom {
		t.Fatal("unknown future input must be dynamic")
	}
	a, _, _, _, _ := Classify(val, reg)
	b, _, _, _, _ := Classify(val, reg)
	if a != b {
		t.Fatal("T11.9.4")
	}
	if _, err := NewCandidate(Candidate{ProjectID: uuid.New(), StateKey: "X"}); err == nil {
		t.Fatal("T11.7.4 readiness required")
	}
	u := uuid.New()
	c, err := NewCandidate(Candidate{ProjectID: uuid.New(), StateKey: "VALIDATE", WorkUnitID: &u, Readiness: PartiallyKnown, Paths: [][]string{{"COMPLETE>VALIDATE"}}})
	if err != nil || c.ID == uuid.Nil || *c.WorkUnitID != u || c.Paths[0][0] != "COMPLETE>VALIDATE" {
		t.Fatal("T11.7.1-T11.7.3")
	}
}

func TestT1110StaticDynamic(t *testing.T) {
	if !IsStatic("role") || !IsStatic("system_prompt") || !IsStatic("work_unit_spec") || !IsStatic("acceptance_criteria") {
		t.Fatal("T11.10.1/T11.10.4")
	}
	for _, d := range []string{"worker_response", "test_output", "generated_plan", "file:ACTIVE_TASK", "anything"} {
		if IsStatic(d) {
			t.Fatalf("T11.10.2/T11.10.3 %s", d)
		}
	}
}

func TestT1115To1116PriorityLimitAndDedup(t *testing.T) {
	pid := uuid.New()
	mk := func(state string, prio, depth int, ident string, miss ...DynamicInputType) Candidate {
		c, _ := NewCandidate(Candidate{ProjectID: pid, StateKey: state, Priority: prio, Depth: depth, Readiness: PartiallyKnown,
			IdentityHash: ident, Missing: miss, Paths: [][]string{{"P>" + state}}})
		return c
	}
	cands := []Candidate{mk("PHASE_VALIDATE", PriorityPhaseBoundry, 1, "a"), mk("IMPLEMENT", PriorityNextUnit, 0, "b"),
		mk("VALIDATE", PriorityImmediate, 1, "c"), mk("FIX", PrioritySameUnit, 2, "d"), mk("FAR", PriorityDistant, 3, "e")}
	r := Rank(cands, 3)
	if len(r) != 3 || r[0].StateKey != "VALIDATE" || r[1].StateKey != "FIX" || r[2].StateKey != "IMPLEMENT" {
		t.Fatalf("T11.15.1-T11.15.3/T11.16.1/T11.16.2 %v", r)
	}
	if !reflect.DeepEqual(Rank(cands, 3), r) {
		t.Fatal("T11.15.3")
	}
	conv := Dedup([]Candidate{mk("C", PriorityImmediate, 1, "same", InputWorkerStatus), mk("C", PriorityDistant, 2, "same", InputWorkerStatus), mk("C", PriorityImmediate, 1, "same", InputTestResults)})
	if len(conv) != 2 || len(conv[0].Paths) != 2 {
		t.Fatalf("T11.44.1-T11.44.3 %+v", conv)
	}
}

func TestT1117NextUnitSelection(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	n := SelectNextUnit([]EligibleUnit{{ID: b, Sequence: 5}, {ID: a, Sequence: 4}})
	if n == nil || n.ID != a {
		t.Fatal("T11.18.1")
	}
	if SelectNextUnit(nil) != nil {
		t.Fatal("no eligible")
	}
	x, y := uuid.MustParse("00000000-0000-0000-0000-000000000001"), uuid.MustParse("00000000-0000-0000-0000-000000000002")
	if SelectNextUnit([]EligibleUnit{{ID: y, Sequence: 1}, {ID: x, Sequence: 1}}).ID != x {
		t.Fatal("T11.18.2 stable tie")
	}
}

func TestT1120To1122Projection(t *testing.T) {
	g := implGraph()
	unit, other := uuid.New(), uuid.New()
	pins := []FilePin{{TypeKey: "ARCHITECTURE", Version: 7, Hash: "r7"}, {TypeKey: "ACTIVE_TASK", Version: 2, Hash: "a2"}}
	p, err := NewProjectedContext(ProjectedContext{BaseSnapshotID: uuid.New(), BaseSemanticHash: "base", ProjectID: uuid.New(),
		ProjectedWorkUnitID: &unit, ProjectedStateKey: "VALIDATE", ProjectedRoleKey: "TESTER", Pins: pins})
	if err != nil || p.ProjectionHash == "" {
		t.Fatal("T11.20.1")
	}
	h := p.ProjectionHash
	if ProjectionHash("base", "VALIDATE", "TESTER", &unit, []FilePin{pins[1], pins[0]}) != h {
		t.Fatal("T11.22.1")
	}
	if ProjectionHash("base", "VALIDATE", "TESTER", &other, pins) == h || ProjectionHash("base", "FIX", "TESTER", &unit, pins) == h ||
		ProjectionHash("base2", "VALIDATE", "TESTER", &unit, pins) == h {
		t.Fatal("T11.22.2-T11.22.4")
	}
	if _, err := NewProjectedContext(ProjectedContext{ProjectID: uuid.New(), ProjectedStateKey: "X"}); err == nil {
		t.Fatal("base snapshot required")
	}
	ok := ValidationInput{Graph: g, StartState: "IMPLEMENT", Path: []string{"COMPLETE>VALIDATE"}, TargetState: "VALIDATE",
		RoleAssigned: true, RoleEnabled: true, UnitBound: true, ProjectedUnit: &unit, UnitIsCurrent: true, StaticFilesOK: true}
	if ValidateProjection(ok) != nil {
		t.Fatal("T11.21.1")
	}
	for name, mut := range map[string]func(*ValidationInput){
		"T11.21.2 impossible transition": func(v *ValidationInput) { v.Path = []string{"FAILED>FIX"}; v.TargetState = "FIX" },
		"T11.20.3 disabled state":        func(v *ValidationInput) { v.TargetState = "ARCHIVE"; v.Path = []string{"ARCHIVE>ARCHIVE"} },
		"T11.21.3 invalid role":          func(v *ValidationInput) { v.RoleEnabled = false },
		"T11.21.4 blocked unit":          func(v *ValidationInput) { v.UnitIsCurrent, v.UnitEligible = false, false },
		"phase change":                   func(v *ValidationInput) { ph := uuid.New(); v.ProjectedPhaseID = &ph },
	} {
		c := ok
		mut(&c)
		if ValidateProjection(c) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestU11PPlaceholder(t *testing.T) {
	u := uuid.New()
	tok := PlaceholderToken(u)
	if tok != PlaceholderToken(u) || tok == PlaceholderToken(uuid.New()) || !ContainsPlaceholder(tok) {
		t.Fatal("U11.P.1")
	}
	text := "header\n" + tok + "\nfooter"
	out, err := ReplacePlaceholder(text, u, "# Active Task\nreal")
	if err != nil || out != "header\n# Active Task\nreal\nfooter" || ContainsPlaceholder(out) {
		t.Fatalf("U11.P.2 %q %v", out, err)
	}
	if _, err := ReplacePlaceholder("no slot", u, "x"); !errors.Is(err, ErrPlaceholderMissing) {
		t.Fatal("U11.P.3 missing")
	}
	if _, err := ReplacePlaceholder(tok+tok, u, "x"); !errors.Is(err, ErrPlaceholderDuplicate) {
		t.Fatal("U11.P.3 duplicate")
	}
	if _, err := ReplacePlaceholder(text, u, "evil "+tok); !errors.Is(err, ErrPlaceholderSpoofed) {
		t.Fatal("U11.P.4")
	}
	if p := PlaceholderPin(u); p.Version != 1 || !p.Placeholder || p.Hash != PlaceholderHash(u) {
		t.Fatal("placeholder pin")
	}
}

func TestT1113Manifest(t *testing.T) {
	m, err := NewFragmentManifest(FragmentManifest{ProjectID: uuid.New(), FragmentID: uuid.New(), SourceSnapshotHash: "h", RequiredDynamicInputs: []DynamicInputType{InputWorkerStatus}})
	if err != nil || m.ID == uuid.Nil || m.RequiredDynamicInputs[0] != InputWorkerStatus || m.SourceSnapshotHash != "h" {
		t.Fatal("T11.13.1/T11.13.2")
	}
	if _, err := NewFragmentManifest(FragmentManifest{ProjectID: uuid.New(), FragmentID: uuid.New(), SourceSnapshotHash: "h", RequiredDynamicInputs: []DynamicInputType{"MAGIC"}}); err == nil {
		t.Fatal("T11.13.4")
	}
}

func TestT1136JobsAndResult(t *testing.T) {
	if !CanTransition(JobPending, JobRunning) || !CanTransition(JobRunning, JobCompleted) || CanTransition(JobCompleted, JobRunning) {
		t.Fatal("T11.36.2/T11.36.3")
	}
	recs := []CandidateRecord{{Result: ResultHit}, {Result: ResultBuilt}, {Result: ResultFragment}, {Result: ResultTemplate}, {Result: ResultSkipped}}
	r := Summarize(uuid.New(), recs)
	if r.Status != JobCompleted || r.Candidates != 5 || r.ExistingHits != 1 || r.FullPrompts != 1 || r.Fragments != 1 || r.Templates != 1 || r.Skipped != 1 {
		t.Fatalf("T11.46.1 %+v", r)
	}
	if Summarize(uuid.New(), nil).Status != JobCompleted {
		t.Fatal("T11.46.2 empty")
	}
	if p := Summarize(uuid.New(), append(recs, CandidateRecord{Result: ResultFailed})); p.Status != JobPartial || p.Failed != 1 {
		t.Fatal("T11.36.4/T11.46.3 partial")
	}
	if Summarize(uuid.New(), []CandidateRecord{{Result: ResultFailed}}).Status != JobFailed {
		t.Fatal("all failed")
	}
}

func TestT1139AndT1140PolicyBudget(t *testing.T) {
	if d, _ := Evaluate(PolicyAlways, true, true, true, true); d != DecisionRun {
		t.Fatal("T11.40.1")
	}
	if d, _ := Evaluate(PolicyIdleOnly, true, false, true, false); d != DecisionSkip {
		t.Fatal("T11.40.2")
	}
	if d, _ := Evaluate(PolicyIdleOnly, false, false, true, false); d != DecisionRun {
		t.Fatal("IDLE_ONLY fragment work")
	}
	if d, _ := Evaluate(PolicyDisabledDuringLocal, false, false, true, true); d != DecisionSkip {
		t.Fatal("T11.40.3")
	}
	if d, _ := Evaluate(PolicyRemoteOnlyWhileActive, true, false, true, false); d != DecisionRun {
		t.Fatal("T11.40.4 remote proceeds")
	}
	if d, _ := Evaluate(PolicyRemoteOnlyWhileActive, true, true, true, false); d != DecisionSkip {
		t.Fatal("local provider while active")
	}
	if !IsLocalURL("http://127.0.0.1:8080/v1") || !IsLocalURL("http://192.168.1.4/v1") || IsLocalURL("https://api.example.com/v1") {
		t.Fatal("IsLocalURL")
	}
	now := time.Now()
	clock := func() time.Time { return now }
	b := NewBudget(2, 100, time.Minute, clock)
	if b.Take(60) != "" || b.Take(60) != BudgetTokens || b.Take(10) != "" || b.Take(0) != BudgetCandidates {
		t.Fatal("T11.39.1/T11.39.2")
	}
	d := NewBudget(0, 0, time.Second, clock)
	now = now.Add(2 * time.Second)
	if d.Take(1) != BudgetDuration {
		t.Fatal("T11.39.3/T11.39.4")
	}
}

func TestT1151To1153Metrics(t *testing.T) {
	usage := []UsageRecord{
		{Depth: 1, Priority: 1, Result: ResultBuilt, Used: true},
		{Depth: 1, Priority: 2, Result: ResultFragment, Used: true},
		{Depth: 2, Priority: 2, Result: ResultTemplate, Used: false},
		{Depth: 2, Priority: 5, Result: ResultBuilt, Used: false},
		{Depth: 1, Priority: 1, Result: ResultHit},
		{Depth: 1, Priority: 1, Result: ResultFailed},
	}
	m := Aggregate(3, usage, []LatencySample{{Source: LatencyOnDemand, Duration: 4 * time.Second}, {Source: LatencyPrecomputed, Duration: 10 * time.Millisecond}, {Source: LatencyPrecomputed, Duration: 30 * time.Millisecond}})
	if m.FullPrompts != 2 || m.Fragments != 1 || m.Templates != 1 || m.Hits != 1 || m.Failed != 1 {
		t.Fatalf("T11.51.1/T11.51.4 %+v", m)
	}
	if m.Overall.Created != 4 || m.Overall.Used != 2 || m.Overall.Utilization != 0.5 || m.Overall.Waste != 0.5 {
		t.Fatalf("T11.51.2/T11.51.3/T11.52.3 %+v", m.Overall)
	}
	if m.ByDepth[1].Utilization != 1 || m.ByDepth[2].Utilization != 0 || m.ByPriority[2].Created != 2 {
		t.Fatalf("T11.77.1/T11.77.2 %+v", m.ByDepth)
	}
	if len(m.Latency) != 2 || m.Latency[1].Source != LatencyPrecomputed || m.Latency[1].Average != 20*time.Millisecond {
		t.Fatalf("T11.53.2 %+v", m.Latency)
	}
}
