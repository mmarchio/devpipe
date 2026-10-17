package integration

import (
	"reflect"
	"strings"
	"testing"

	"overseer/internal/continuation"
	"overseer/internal/prompt"
	"overseer/internal/worker"
)

var p14FreshSequence = []string{"PENDING", "DECIDING", "RESET_REQUIRED", "CREATING_SESSION", "RESTORING_CONTEXT",
	"RESTORING_CWD", "READY", "DISPATCHING", "WAITING", "COMPLETED"}

// P14-70 — WBS 14.104 + 14.105 + 14.22 + 14.23 (+ 14.128 sequence): at or
// above the threshold a fresh session is created exactly once, the old
// session is reset only after the new one is confirmed, the CWD is
// restored before the continuation, and S2 receives the same work unit and
// captured status with no prior conversation.
func TestP14104FreshSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens int64
		reason continuation.DecisionReason
	}{{"exact_51200", 51200, continuation.ReasonAtThreshold}, {"high_61000", 61000, continuation.ReasonAboveThreshold}} {
		t.Run(tc.name, func(t *testing.T) {
			r := p14NewRig(t)
			s1 := r.activeSession(t)
			before := r.workflowRows(t)
			r.initial(t, p14Report("incomplete", []string{"A", "B"}, []string{"C"}, "Implement C"), tc.tokens)
			r.reply(p14Report("complete", []string{"A", "B", "C"}, nil, "Validate."), 3000)
			from, newBefore := len(r.g.Ops()), r.ops("new")
			r.process(t)

			c1 := r.unitContinuations(t)[0]
			if c1.Strategy != continuation.ContinueFreshSession || c1.Decision.Reason != tc.reason || c1.Status != continuation.StatusCompleted {
				t.Fatalf("decision %+v / %+v", c1, c1.Decision)
			}
			if n := r.ops("new") - newBefore; n != 1 {
				t.Fatalf("14.24 NewSession called %d times", n)
			}
			// T14.23.1/T14.23.2: new → cd → continuation, nothing else.
			if got := r.opLog(from); !reflect.DeepEqual(got, []string{"new", "cd", "msg"}) {
				t.Fatalf("14.22 operation order %v", got)
			}
			if got := r.history(t, c1.ID); !reflect.DeepEqual(got, p14FreshSequence) {
				t.Fatalf("14.128 persisted sequence %v", got)
			}
			s2 := r.activeSession(t)
			if s2.ID == s1.ID || r.session(t, s1.ID).Status != worker.SessionReset {
				t.Fatal("14.104 S1 not reset after S2 confirmation")
			}
			if c1.OldSessionID == nil || *c1.OldSessionID != s1.ID || c1.NewSessionID == nil || *c1.NewSessionID != s2.ID || c1.SessionConfirmedAt == nil {
				t.Fatalf("session identities %+v", c1)
			}
			if cwd := r.g.SessionCWD(s2.ProviderSessionID); cwd == nil || *cwd != "/repos/project-a" {
				t.Fatal("14.104 CWD not restored")
			}
			d2 := r.dispatchOf(t, *c1.DispatchID).Dispatch
			if d2.SessionID != s2.ID || *d2.WorkUnitID != r.pos.UnitID || *d2.PhaseID != r.pos.PhaseID {
				t.Fatalf("14.105 dispatch %+v", d2)
			}
			ms := r.g.MessagesFor(s2.ProviderSessionID)
			if len(ms) != 1 {
				t.Fatalf("14.118 S2 history %d messages", len(ms))
			}
			for _, want := range []string{r.pos.UnitID.String(), prompt.PriorWorkerReportOpen, "completed: A; B", "remaining: C", "AC-1"} {
				if !strings.Contains(ms[0].Content, want) {
					t.Fatalf("14.105 S2 prompt missing %q", want)
				}
			}
			if r.workflowRows(t) != before {
				t.Fatal("14.120 workflow changed")
			}
		})
	}
}
