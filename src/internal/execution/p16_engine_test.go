package execution

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/phase"
	"overseer/internal/state"
	"overseer/internal/wbs"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// P16-01 (16.2, 16.6, 16.29, 16.159).
func TestP1601WaitingAndSchedulable(t *testing.T) {
	if s, err := ParseExecutionStatus("WAITING"); err != nil || s != ExecutionWaiting {
		t.Fatalf("WAITING: %v", err)
	}
	if !ExecutionWaiting.IsActive() || ExecutionWaiting.IsTerminal() {
		t.Fatal("WAITING must be active, not terminal")
	}
	for _, e := range [][2]ExecutionStatus{{ExecutionRunning, ExecutionWaiting}, {ExecutionWaiting, ExecutionRunning},
		{ExecutionWaiting, ExecutionPaused}, {ExecutionWaiting, ExecutionRecovery}, {ExecutionWaiting, ExecutionCancelled}, {ExecutionWaiting, ExecutionFailed}} {
		if err := ValidateTransition(e[0], e[1]); err != nil {
			t.Fatalf("%v rejected", e)
		}
	}
	for _, e := range [][2]ExecutionStatus{{ExecutionWaiting, ExecutionCompleted}, {ExecutionInitializing, ExecutionWaiting}, {ExecutionReady, ExecutionWaiting}, {ExecutionPaused, ExecutionWaiting}} {
		if !errors.Is(ValidateTransition(e[0], e[1]), ErrIllegalTransition) {
			t.Fatalf("%v allowed", e)
		}
	}
	for _, s := range AllStatuses() {
		want := s == ExecutionRunning || s == ExecutionWaiting
		if s.IsSchedulable() != want {
			t.Fatalf("schedulable(%s) = %v", s, s.IsSchedulable())
		}
	}
}

// P16-02.
func TestP1602EngineSettings(t *testing.T) {
	d := DefaultEngineSettings()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.MaxFixAttempts != 3 || d.MaxPlanAttempts != 3 || d.CheckpointPolicy != CheckpointDisabled || d.StepBudget != 8 ||
		d.AtomicPolicy != wbs.DefaultAtomicPolicy() || len(d.FinalValidation) != 2 {
		t.Fatalf("defaults: %+v", d)
	}
	// Git is opt-in: "use git" maps to REQUIRED, off to DISABLED.
	if CheckpointPolicyFor(true) != CheckpointRequired || CheckpointPolicyFor(false) != CheckpointDisabled ||
		!CheckpointBestEffort.UsesGit() || CheckpointDisabled.UsesGit() {
		t.Fatal("use git mapping")
	}
	bad := EngineSettings{CheckpointPolicy: "MAYBE", UnitValidation: []wbs.ValidationSpec{{Program: "sh -c x"}}}
	err := bad.Validate()
	for _, want := range []string{"max_fix_attempts", "max_plan_attempts", "checkpoint_policy", "unit_validation[0]", "validation_timeout", "output_cap_bytes", "step_budget", "driver_lease"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %s in %v", want, err)
		}
	}
	for _, p := range []string{"REQUIRED", "BEST_EFFORT", "DISABLED"} {
		if _, err := ParseCheckpointPolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ParseCheckpointPolicy("required"); err == nil {
		t.Fatal("lowercase accepted")
	}
}

// P16-03 (16.43): resolution ignores worker claims; dedup keeps order.
func TestP1603ResolveValidationCommands(t *testing.T) {
	st := DefaultEngineSettings()
	st.UnitValidation = []wbs.ValidationSpec{{Program: "go", Args: []string{"vet", "./..."}}, {Program: "go", Args: []string{"test", "./..."}}}
	spec := &wbs.WorkUnitSpecification{
		Validation: []wbs.ValidationSpec{{Program: "go", Args: []string{"test", "./..."}}},
		AcceptanceChecks: []wbs.AcceptanceCheckSpec{
			{Text: "lint", Verification: wbs.VerifyCommand, Command: &wbs.ValidationSpec{Program: "golint"}},
			{Text: "m", Verification: wbs.VerifyManual},
		},
	}
	got := ResolveValidationCommands(spec, st, ScopeUnit)
	var keys []string
	for _, c := range got {
		keys = append(keys, c.Program+" "+strings.Join(c.Args, " "))
	}
	if strings.Join(keys, "|") != "go test ./...|golint |go vet ./..." {
		t.Fatalf("resolved %v", keys)
	}
	if len(ResolveValidationCommands(nil, DefaultEngineSettings(), ScopeUnit)) != 0 {
		t.Fatal("unit default should be empty → NOT_CONFIGURED")
	}
	if len(ResolveValidationCommands(nil, st, ScopePhase)) != 1 || len(ResolveValidationCommands(nil, st, ScopeFinal)) != 2 {
		t.Fatal("phase/final policy")
	}
}

// P16-04 (16.45–16.47).
func TestP1604AggregateValidation(t *testing.T) {
	r := func(o ValidationOutcome) ValidationRecord { return ValidationRecord{Outcome: o} }
	cases := []struct {
		in   []ValidationRecord
		want ValidationOutcome
	}{
		{nil, ValidationNotConfigured},
		{[]ValidationRecord{r(ValidationPass), r(ValidationPass)}, ValidationPass},
		{[]ValidationRecord{r(ValidationPass), r(ValidationFail)}, ValidationFail},
		{[]ValidationRecord{r(ValidationFail), r(ValidationInfraError)}, ValidationInfraError},
	}
	for i, c := range cases {
		if got := AggregateValidation(c.in); got != c.want {
			t.Fatalf("case %d: %s want %s", i, got, c.want)
		}
	}
}

// P16-05 (16.48): unverified manual criterion is not PASS.
func TestP1605EvaluateAcceptance(t *testing.T) {
	cmd := &wbs.ValidationSpec{Program: "go", Args: []string{"test", "./x"}}
	checks := []wbs.AcceptanceCheckSpec{
		{Text: "cmd", Verification: wbs.VerifyCommand, Command: cmd},
		{Text: "tests", Verification: wbs.VerifyTest, Tests: []string{"TestX"}},
		{Text: "docs reviewed", Verification: wbs.VerifyManual},
	}
	pass := []ValidationRecord{{Program: "go", Args: []string{"test", "./x"}, Outcome: ValidationPass}}
	res := EvaluateAcceptance(checks, pass, nil)
	if res.Outcome != AcceptanceReviewRequired || res.Verdicts[2].Verdict != VerdictUnverified || res.Verdicts[0].Verdict != VerdictPass {
		t.Fatalf("manual: %+v", res)
	}
	if res := EvaluateAcceptance(checks, pass, map[int]bool{2: true}); res.Outcome != AcceptancePass {
		t.Fatalf("approved: %+v", res)
	}
	fail := []ValidationRecord{{Program: "go", Args: []string{"test", "./x"}, Outcome: ValidationFail}}
	if res := EvaluateAcceptance(checks, fail, map[int]bool{2: true}); res.Outcome != AcceptanceFail {
		t.Fatalf("fail: %+v", res)
	}
	if res := EvaluateAcceptance(nil, nil, nil); res.Outcome != AcceptancePass {
		t.Fatal("no checks")
	}
}

// P16-06 (16.1, 16.83).
func TestP1606StepTypes(t *testing.T) {
	for _, a := range AllStepActions() {
		if got, err := ParseStepAction(string(a)); err != nil || got != a {
			t.Fatalf("%s", a)
		}
	}
	if _, err := ParseStepAction("NOPE"); err == nil {
		t.Fatal("bad action")
	}
	for _, o := range []StepOutcome{StepWaiting, StepPaused, StepRecoveryRequired, StepExecutionFailed, StepCancelled, StepCompleted, StepNoop} {
		if !o.StopsRun() {
			t.Fatalf("%s must stop Run", o)
		}
	}
	if StepProgressed.StopsRun() {
		t.Fatal("PROGRESSED stops")
	}
	id := uuid.New()
	if IdempotencyKey(id, 4, ActionDispatch, 1) != IdempotencyKey(id, 4, ActionDispatch, 1) ||
		IdempotencyKey(id, 4, ActionDispatch, 1) == IdempotencyKey(id, 5, ActionDispatch, 1) {
		t.Fatal("idempotency key")
	}
}

// P16-07 (16.1): every default key bound; kind fallback.
func TestP1607ResolveAction(t *testing.T) {
	want := map[string]StepAction{"PHASE_PLAN": ActionPlanDispatch, "PLAN_VALIDATE": ActionPlanValidate, "WORK_UNIT_PREPARE": ActionSelectUnit,
		"IMPLEMENT": ActionDispatch, "FIX": ActionDispatch, "CONTINUE": ActionDispatch, "RESPONSE_EVALUATE": ActionEvaluateResult,
		"CONTINUE_DECISION": ActionContinuation, "VALIDATE": ActionRunUnitValidation, "UNIT_DONE": ActionUnitGates, "MORE_WORK": ActionMoreWork,
		"PHASE_VALIDATE": ActionPhaseValidation, "MORE_PHASES": ActionMorePhases, "FINAL_HARDENING": ActionFinalHardening,
		"HARDENING_FIX": ActionHardeningFix, "FINAL_VALIDATE": ActionFinalValidation, "FINAL_CHECKPOINT": ActionFinalCheckpoint,
		"PROJECT_INIT": ActionControl, "PASS": ActionControl, "NEXT_UNIT": ActionControl}
	for k, a := range want {
		if got := ResolveAction(state.StateSnapshot{Key: k, Kind: string(state.KindControl)}); got != a {
			t.Fatalf("%s → %s want %s", k, got, a)
		}
	}
	for kind, a := range map[state.StateKind]StepAction{state.KindAction: ActionDispatch, state.KindValidation: ActionRunUnitValidation,
		state.KindDecision: ActionControl, state.KindControl: ActionControl, state.KindTerminal: ActionComplete} {
		if got := ResolveAction(state.StateSnapshot{Key: "CUSTOM", Kind: string(kind)}); got != a {
			t.Fatalf("kind %s → %s", kind, got)
		}
	}
	if ResolveAction(state.StateSnapshot{Key: "X", Kind: "CONTROL", Terminal: true}) != ActionComplete {
		t.Fatal("terminal flag")
	}
}

// P16-08 (16.31–16.42, 16.75).
func TestP1608MapDispatchOutcome(t *testing.T) {
	cases := []struct {
		in   DispatchOutcome
		want ResultToken
	}{
		{DispatchOutcome{Status: workerresult.StatusComplete}, TokenComplete},
		{DispatchOutcome{Status: workerresult.StatusIncomplete}, TokenIncomplete},
		{DispatchOutcome{Status: workerresult.StatusBlocked}, TokenBlocked},
		{DispatchOutcome{Status: workerresult.StatusFailed}, TokenFailed},
		{DispatchOutcome{ProtocolFailure: true, Status: workerresult.StatusFailed}, TokenProtocolError},
		{DispatchOutcome{Error: &worker.WorkerError{Code: worker.CodeContextLimit, Delivery: worker.DeliveryReceived}}, TokenContextLimit},
		{DispatchOutcome{Error: &worker.WorkerError{Code: worker.CodeUnavailable, Delivery: worker.DeliveryUnknown}}, TokenDeliveryUnknown},
		{DispatchOutcome{Error: &worker.WorkerError{Code: worker.CodeUnavailable, Delivery: worker.DeliveryNotSent}}, TokenTransportFailed},
	}
	for i, c := range cases {
		if got := MapDispatchOutcome(c.in); got != c.want {
			t.Fatalf("case %d: %s want %s", i, got, c.want)
		}
	}
}

func p16Machine() *state.StateMachineSnapshot {
	return &state.StateMachineSnapshot{
		InitialState: state.StateSnapshot{Key: "RESPONSE_EVALUATE", Enabled: true},
		States:       []state.StateSnapshot{{Key: "RESPONSE_EVALUATE", Enabled: true}},
		Transitions: []state.TransitionSnapshot{
			{From: "RESPONSE_EVALUATE", To: "VALIDATE", Outcome: "COMPLETE", Enabled: true},
			{From: "RESPONSE_EVALUATE", To: "CONTINUE_DECISION", Outcome: "INCOMPLETE", Enabled: true},
			{From: "RESPONSE_EVALUATE", To: "FIX", Outcome: "FAILED", Enabled: true},
		},
	}
}

// P16-09 (16.40, 16.41).
func TestP1609RouteToken(t *testing.T) {
	m := p16Machine()
	for tok, to := range map[ResultToken]string{TokenComplete: "VALIDATE", TokenIncomplete: "CONTINUE_DECISION", TokenContextLimit: "CONTINUE_DECISION", TokenFailed: "FIX"} {
		d, err := RouteToken(m, "RESPONSE_EVALUATE", tok)
		if err != nil || d.Transition == nil || d.Transition.To != to {
			t.Fatalf("%s: %+v %v", tok, d, err)
		}
	}
	for _, tok := range []ResultToken{TokenBlocked, TokenProtocolError, TokenDeliveryUnknown, TokenTransportFailed} {
		d, err := RouteToken(m, "RESPONSE_EVALUATE", tok)
		if err != nil || d.Transition != nil || d.Recovery == "" {
			t.Fatalf("%s should require recovery: %+v", tok, d)
		}
	}
	m.Transitions = append(m.Transitions, state.TransitionSnapshot{From: "RESPONSE_EVALUATE", To: "X", Outcome: "COMPLETE", Enabled: true})
	if _, err := RouteToken(m, "RESPONSE_EVALUATE", TokenComplete); err == nil {
		t.Fatal("ambiguity not reported")
	}
}

// P16-10 (16.84, 16.85).
func TestP1610Invariants(t *testing.T) {
	cur := &phase.Phase{ID: uuid.New(), Status: phase.StatusEXECUTING}
	plan := &wbs.ExecutionPlan{ID: uuid.New(), WBSVersion: 2}
	unit := &wbs.WorkUnit{ID: uuid.New(), ExecutionPlanID: plan.ID, Status: wbs.WorkUnitStatusEXECUTING}
	ok := InvariantFacts{PositionPhase: cur, CurrentPhase: cur, Plan: plan, CurrentWBSVersion: 2, PositionUnit: unit, PlanUnits: []*wbs.WorkUnit{unit}, UnitAction: true}
	if v := CheckInvariants(ok); len(v) != 0 {
		t.Fatal(v)
	}
	ex := uuid.New()
	other := &phase.Phase{ID: uuid.New(), Status: phase.StatusPENDING}
	bad := ok
	bad.CurrentPhase = other
	bad.CurrentWBSVersion = 3
	bad.PositionUnit = &wbs.WorkUnit{ID: uuid.New(), ExecutionPlanID: uuid.New()}
	bad.OpenDispatches = map[uuid.UUID]int{unit.ID: 2}
	bad.PlanUnits = []*wbs.WorkUnit{{ID: uuid.New(), Status: wbs.WorkUnitStatusDONE, ClaimedExecutionID: &ex}}
	if v := CheckInvariants(bad); len(v) != 5 {
		t.Fatalf("violations: %v", v)
	}
	// Phase DONE requires all current-plan units DONE.
	if PhaseMayComplete(plan, []*wbs.WorkUnit{{Status: wbs.WorkUnitStatusDONE}, {Status: wbs.WorkUnitStatusVALIDATING}}) {
		t.Fatal("phase may complete with open unit")
	}
}

// P16-11.
func TestP1611AttemptDecision(t *testing.T) {
	if AttemptDecision(0, 3) != AttemptAllow || AttemptDecision(2, 3) != AttemptAllow || AttemptDecision(3, 3) != AttemptExhausted {
		t.Fatal("attempts")
	}
}

// P16-12 (16.6, 16.7).
func TestP1612SchedulingFairness(t *testing.T) {
	t0 := time.Unix(1000, 0)
	p1, p2, p3 := uuid.New(), uuid.New(), uuid.New()
	at := func(d int) *time.Time { x := t0.Add(time.Duration(d) * time.Second); return &x }
	cands := []SchedulingCandidate{
		{ExecutionID: uuid.New(), ProjectID: p1, LastScheduledAt: at(5)},
		{ExecutionID: uuid.New(), ProjectID: p2, LastScheduledAt: at(1)},
		{ExecutionID: uuid.New(), ProjectID: p3, LastScheduledAt: nil},
		{ExecutionID: uuid.New(), ProjectID: p2, LastScheduledAt: at(0)},
	}
	got := SelectSchedulingBatch(cands, 2)
	if len(got) != 2 || got[0].ProjectID != p3 || got[1].ProjectID != p2 {
		t.Fatalf("batch %+v", got)
	}
	// Rotating: after p3 and p2 ran, p1 gets the next turn.
	now := at(10)
	for i := range cands {
		if cands[i].ProjectID != p1 {
			cands[i].LastScheduledAt = now
		}
	}
	if got := SelectSchedulingBatch(cands, 1); got[0].ProjectID != p1 {
		t.Fatal("p1 starved")
	}
}

// P16-13 (16.54–16.57, 16.80).
func TestP1613Checkpoint(t *testing.T) {
	ex, unit := uuid.New(), uuid.New()
	k := CheckpointKey(ex, RepoCheckpointUnit, unit, 1)
	if k != CheckpointKey(ex, RepoCheckpointUnit, unit, 1) || k == CheckpointKey(ex, RepoCheckpointUnit, unit, 2) {
		t.Fatal("key")
	}
	msg := CheckpointMessage(RepoCheckpointUnit, k, ex, "unit 3")
	if !strings.Contains(msg, TrailerCheckpoint+": "+k) || !strings.Contains(msg, TrailerExecution+": "+ex.String()) {
		t.Fatal(msg)
	}
	if OnCheckpointFailure(CheckpointRequired) != CheckpointBlock || OnCheckpointFailure(CheckpointBestEffort) != CheckpointProceed {
		t.Fatal("policy")
	}
	if !RepoCheckpointFinal.Valid() || RepoCheckpointKind("X").Valid() {
		t.Fatal("kind")
	}
}

// P16-14 (16.81).
func TestP1614EngineEvents(t *testing.T) {
	for _, e := range []EventType{EventStepIntent, EventUnitDone, EventPhaseDone, EventRecoveryRequired, EventHardeningCompleted, EventProjectCompleted} {
		if got, err := ParseEventType(string(e)); err != nil || got != e {
			t.Fatalf("%s", e)
		}
	}
}

// P16-15 (16.161).
func TestP1615Summarize(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := t0.Add(90 * time.Second)
	s := Summarize(SummaryInput{
		Phases:           []*phase.Phase{{Status: phase.StatusDONE}, {Status: phase.StatusEXECUTING}},
		CurrentPlanUnits: []*wbs.WorkUnit{{Status: wbs.WorkUnitStatusDONE}, {Status: wbs.WorkUnitStatusVALIDATING}},
		Dispatches:       4, Continuations: 1, SessionResets: 1, ValidationRuns: 3, FixAttempts: 1, Checkpoints: 1,
		StartedAt: &t0, CompletedAt: &t1, Now: t1.Add(time.Hour),
	})
	if s.PhasesCompleted != 1 || s.PhasesTotal != 2 || s.WorkUnitsCompleted != 1 || s.WorkUnitsTotal != 2 || s.WorkerInvocations != 4 || s.DurationMS != 90000 {
		t.Fatalf("%+v", s)
	}
}
