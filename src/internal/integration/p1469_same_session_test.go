package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/continuation"
	"overseer/internal/prompt"
	"overseer/internal/worker"
)

// P14-69 — WBS 14.103 + 14.18 + 14.31 + 14.119 + 14.123: below-threshold
// INCOMPLETE continues in the SAME session with a CONTINUE_WORK_UNIT prompt
// carrying the captured status.
func TestP14103SameSession(t *testing.T) {
	r := p14NewRig(t)
	ctx := context.Background()
	s1 := r.activeSession(t)
	before := r.workflowRows(t)
	newBefore := r.ops("new")

	d1 := r.initial(t, p14Report("incomplete", []string{"A", "B"}, []string{"C"}, "Implement C"), 32000)
	cs := r.unitContinuations(t)
	if len(cs) != 1 || cs[0].Status != continuation.StatusPending || cs[0].Attempt != 1 || cs[0].SourceDispatchID != d1.ID {
		t.Fatalf("C1 not created: %+v", cs)
	}
	c1 := cs[0]
	r.reply(p14Report("complete", []string{"A", "B", "C"}, nil, "Run deterministic validation."), 40000)
	r.process(t)

	c1 = *r.cont(t, c1.ID)
	if c1.Status != continuation.StatusCompleted || c1.Strategy != continuation.ContinueSameSession ||
		c1.Decision.Reason != continuation.ReasonBelowThreshold || *c1.Decision.ContextTokens != 32000 {
		t.Fatalf("14.103 decision %+v %+v", c1, c1.Decision)
	}
	if c1.SessionID == nil || *c1.SessionID != s1.ID || r.activeSession(t).ID != s1.ID {
		t.Fatal("14.103 S1 not retained")
	}
	if r.ops("new") != newBefore {
		t.Fatal("14.7 session created for a below-threshold continuation")
	}
	if c1.CWDConfirmedAt == nil || c1.SnapshotID == nil {
		t.Fatal("14.103 CWD / snapshot not confirmed")
	}
	d2 := r.dispatchOf(t, *c1.DispatchID).Dispatch
	if d2.SessionID != s1.ID || d2.ContinuationID == nil || *d2.ContinuationID != c1.ID || *d2.WorkUnitID != r.pos.UnitID ||
		d2.RoleKey != "ENGINEER" || d2.ID == d1.ID {
		t.Fatalf("14.35 dispatch D2 %+v", d2)
	}
	content := r.g.MessagesFor(r.g.Messages()[len(r.g.Messages())-1].SessionID)
	last := content[len(content)-1].Content
	for _, want := range []string{prompt.AuthoritativeContinuationOpen, prompt.PriorWorkerReportOpen, "A", "B", "C",
		"Do not redo work listed as completed", "Focus on the remaining work.", "Implement C"} {
		if !strings.Contains(last, want) {
			t.Fatalf("14.103/14.123 prompt missing %q:\n%s", want, last)
		}
	}
	if key := p14TemplateKey(t, r, *c1.WorkerPromptID); key != "CONTINUE_WORK_UNIT" {
		t.Fatalf("14.28 template %q", key)
	}
	if r.workflowRows(t) != before {
		t.Fatal("14.120 workflow changed")
	}
	// The continuation's own result was consumed; the COMPLETE result left
	// the loop (no further continuation).
	if cs := r.unitContinuations(t); len(cs) != 1 {
		t.Fatalf("14.51 continuation created from COMPLETE: %d", len(cs))
	}
	if ok, _ := r.h.App.Continuations().Continuation(ctx, r.pos.ProjectID, c1.ID); ok == nil {
		t.Fatal("lookup")
	}
	_ = worker.SessionActive
}
