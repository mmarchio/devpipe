package execution

import (
	"slices"
	"testing"
)

// REQUEUE is offered next to CANCEL for a live execution with a task that
// can be requeued, in RECOVERY_REQUIRED too, and never otherwise.
func TestAllowedActionsRequeue(t *testing.T) {
	for _, tc := range []struct {
		facts ControlFacts
		want  bool
	}{
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionRunning, Requeueable: true}, true},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionWaiting, Requeueable: true}, true},
		{ControlFacts{ProjectStatus: "PAUSED", HasExecution: true, ExecutionStatus: ExecutionPaused, Requeueable: true}, true},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionRecovery, Requeueable: true}, true},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionRunning}, false},
		{ControlFacts{ProjectStatus: "RUNNING", HasExecution: true, ExecutionStatus: ExecutionInitializing, Requeueable: true}, false},
		{ControlFacts{ProjectStatus: "READY", HasExecution: true, ExecutionStatus: ExecutionCompleted, Requeueable: true}, false},
		{ControlFacts{ProjectStatus: "COMPLETED", HasExecution: true, ExecutionStatus: ExecutionRunning, Requeueable: true}, false},
	} {
		got := AllowedActions(tc.facts)
		i := slices.Index(got, ControlRequeue)
		if (i >= 0) != tc.want {
			t.Errorf("%+v: actions %v, requeue offered %v, want %v", tc.facts, got, i >= 0, tc.want)
		}
		if i > 0 && got[i-1] != ControlCancel {
			t.Errorf("%+v: requeue not next to cancel: %v", tc.facts, got)
		}
	}
}
