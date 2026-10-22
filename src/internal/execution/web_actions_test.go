package execution

import (
	"reflect"
	"testing"
)

// P17-14 (WBS 17.164 / 17.96 / 17.172) — allowed controls come from the
// persisted statuses; recovery blocks Start / Resume / Step.
func TestP1714AllowedActions(t *testing.T) {
	cases := []struct {
		f    ControlFacts
		want []ControlAction
	}{
		{ControlFacts{ProjectStatus: "READY", ReadinessOK: true}, []ControlAction{ControlStart, ControlCancel}},
		{ControlFacts{ProjectStatus: "READY", ReadinessOK: false}, []ControlAction{ControlCancel}},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionRunning}, []ControlAction{ControlPause, ControlStep, ControlCancel}},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionWaiting}, []ControlAction{ControlPause, ControlStep, ControlCancel}},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionRecovery}, []ControlAction{ControlCancel, ControlResolveRecovery}},
		{ControlFacts{ProjectStatus: "PAUSED", HasExecution: true, ExecutionStatus: ExecutionPaused}, []ControlAction{ControlResume, ControlCancel}},
		{ControlFacts{ProjectStatus: "PAUSED", HasExecution: true, ExecutionStatus: ExecutionRecovery}, []ControlAction{ControlCancel, ControlResolveRecovery}},
		{ControlFacts{ProjectStatus: "COMPLETED", HasExecution: true, ExecutionStatus: ExecutionCompleted}, nil},
		{ControlFacts{ProjectStatus: "CANCELLED", HasExecution: true, ExecutionStatus: ExecutionCancelled}, nil},
		{ControlFacts{ProjectStatus: "FAILED"}, nil},
		{ControlFacts{ProjectStatus: "DRAFT"}, []ControlAction{ControlCancel}},
		// Pause after task: statefile executions only, between Pause and Cancel.
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionRunning, Statefile: true},
			[]ControlAction{ControlPause, ControlPauseAfterTask, ControlStep, ControlCancel}},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionRunning, Statefile: true, PauseAfterTask: true},
			[]ControlAction{ControlPause, ControlCancelPauseAfterTask, ControlStep, ControlCancel}},
		{ControlFacts{ProjectStatus: "PAUSED", HasExecution: true, ExecutionStatus: ExecutionPaused, Statefile: true},
			[]ControlAction{ControlResume, ControlCancel}},
		{ControlFacts{ProjectStatus: "PAUSED", HasExecution: true, ExecutionStatus: ExecutionPaused, Statefile: true, PauseAfterTask: true},
			[]ControlAction{ControlResume, ControlCancelPauseAfterTask, ControlCancel}},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionRecovery, Statefile: true},
			[]ControlAction{ControlCancel, ControlResolveRecovery}},
	}
	for _, c := range cases {
		if got := AllowedActions(c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v: got %v want %v", c.f, got, c.want)
		}
	}
	// Start is never offered while an execution is active.
	for _, st := range ActiveStatuses() {
		for _, a := range AllowedActions(ControlFacts{ProjectStatus: "READY", ReadinessOK: true, HasExecution: true, ExecutionStatus: st}) {
			if a == ControlStart {
				t.Fatalf("start offered with active execution %s", st)
			}
		}
	}
}

// P17-15 (WBS 17.98 / 17.99 / 17.100) — typed recovery actions; unknown
// delivery never yields a dispatching action.
func TestP1715RecoveryActions(t *testing.T) {
	kinds := func(as []RecoveryAction) []RecoveryActionKind {
		var out []RecoveryActionKind
		for _, a := range as {
			out = append(out, a.Kind)
		}
		return out
	}
	du := RecoveryActions(RecoveryFactsFrom("worker delivery unknown: the request is not resent", map[string]any{"action": "EVALUATE_RESULT", "token": "DELIVERY_UNKNOWN"}))
	// Phase 18 (Decision 8): the reviewed operator resolution replaces the
	// self-looping MANUAL_RESUME; RESOLVE_DELIVERY is high impact.
	if !reflect.DeepEqual(kinds(du), []RecoveryActionKind{RecoveryReconcileDispatch, RecoveryValidateRepositoryState, RecoveryResolveDelivery}) || !du[2].HighImpact {
		t.Fatalf("delivery unknown: %v", du)
	}
	for _, a := range du {
		if a.Kind == RecoveryRunValidation || a.Kind == RecoveryRetryCheckpoint || a.Kind == RecoveryApproveCriteria {
			t.Fatalf("unexpected action for unknown delivery: %v", a)
		}
	}
	cases := map[string]RecoveryActionKind{
		"acceptance criteria require manual review":  RecoveryApproveCriteria,
		"required checkpoint failed":                 RecoveryRetryCheckpoint,
		"final hardening baseline checkpoint failed": RecoveryRetryCheckpoint,
		"validation could not run (infrastructure)":  RecoveryRunValidation,
	}
	for reason, want := range cases {
		got := kinds(RecoveryActions(RecoveryFacts{Reason: reason}))
		if len(got) != 2 || got[0] != want || got[1] != RecoveryManualResume {
			t.Fatalf("%s: %v", reason, got)
		}
	}
	if got := kinds(RecoveryActions(RecoveryFacts{Reason: "whatever", StepAction: ActionPhaseValidation})); got[0] != RecoveryRunValidation {
		t.Fatalf("phase validation step: %v", got)
	}
	if got := kinds(RecoveryActions(RecoveryFacts{Reason: "dependency deadlock"})); !reflect.DeepEqual(got, []RecoveryActionKind{RecoveryManualResume}) {
		t.Fatalf("generic: %v", got)
	}
	for _, a := range RecoveryActions(RecoveryFacts{Reason: "acceptance criteria require manual review"}) {
		if a.Label == "" || (a.HighImpact != (a.Kind == RecoveryApproveCriteria || a.Kind == RecoveryManualResume)) {
			t.Fatalf("labels / impact: %+v", a)
		}
	}
}

// P17-16 (WBS 17.81) — every event type has exactly one category; the
// category filter partitions the type set.
func TestP1716EventCategories(t *testing.T) {
	seen := map[EventType]int{}
	for _, c := range EventCategories() {
		for _, typ := range TypesInCategories([]EventCategory{c}) {
			seen[typ]++
		}
		if _, ok := ParseEventCategory(string(c)); !ok {
			t.Fatal(c)
		}
	}
	for _, typ := range AllEventTypes() {
		if seen[typ] != 1 {
			t.Fatalf("%s categorized %d times", typ, seen[typ])
		}
	}
	if CategoryOf(EventPlanActivated) != EventCatPlanning || CategoryOf(EventDispatchIntent) != EventCatWorker ||
		CategoryOf(EventValidationFailed) != EventCatValidation || CategoryOf(EventCheckpointCreated) != EventCatCheckpoint ||
		CategoryOf(EventPhaseDone) != EventCatPhase || CategoryOf(EventRecoveryRequired) != EventCatRecovery ||
		CategoryOf(EventStarted) != EventCatSystem {
		t.Fatal("category mapping")
	}
	if _, ok := ParseEventCategory("bogus"); ok {
		t.Fatal("bogus category")
	}
}
