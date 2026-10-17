package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/continuation"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

// P14-80 — WBS 14.118 + 14.26 + 14.30: a fresh session with zero history
// receives enough reconstructed context to resume the same work unit; the
// prior raw conversation is absent; the prompt is reconstructable after a
// restart.
func TestP14118FreshSessionIndependence(t *testing.T) {
	r, boot := p14RestartRig(t)
	ctx := context.Background()
	raw := p14Report("incomplete", []string{"parser", "repository"}, []string{"handler"}, "Implement the handler")
	r.initial(t, raw, 60000)
	r.reply(p14Report("complete", []string{"parser", "repository", "handler"}, nil, "x"), 3000)
	r.process(t)
	c := r.unitContinuations(t)[0]
	s2 := r.activeSession(t)
	ms := r.g.MessagesFor(s2.ProviderSessionID)
	if c.Strategy != continuation.ContinueFreshSession || len(ms) != 1 {
		t.Fatalf("setup: %s, %d messages in S2", c.Strategy, len(ms))
	}
	p := ms[0].Content
	for what, want := range map[string]string{
		"project": "PROJECT_STATE", "phase": "PHASE_STATE", "work unit": r.pos.UnitID.String(), "objective": "Objective 11.3",
		"acceptance criteria": "AC-1: 11.3 criterion one.", "completed work": "completed: parser; repository", "remaining work": "remaining: handler",
		"constraints": "CONSTRAINT-1", "next action": "Implement the handler", "response protocol": "Response protocol v1",
		"continuation instruction": "Do not redo work listed as completed",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("14.118 prompt cannot identify the %s (%q)", what, want)
		}
	}
	// T14.30.1: the previous raw worker response is not replayed.
	if strings.Contains(p, `"protocol_version":"1.0"`) || strings.Contains(p, raw) {
		t.Fatal("T14.30.1 raw conversation replayed")
	}
	if !strings.Contains(p, prompt.PriorWorkerReportOpen) {
		t.Fatal("T14.30.2 structured status absent")
	}
	// T14.30.3: after a restart the persisted continuation recomposes the
	// byte-identical canonical prompt.
	var original string
	if err := r.h.DBScan(ctx, `SELECT cp.content FROM worker_prompts wp JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id
		WHERE wp.id = '`+c.WorkerPromptID.String()+`'`, &original); err != nil {
		t.Fatal(err)
	}
	r.restart(t, boot)
	cs, _, _ := r.h.App.PromptCache().CacheSettings(ctx, r.pos.ProjectID)
	cs.Enabled = false
	if _, err := r.h.App.PromptCache().UpdateCacheSettings(ctx, r.pos.ProjectID, cs); err != nil {
		t.Fatal(err)
	}
	persisted := r.cont(t, c.ID)
	var tmplID string
	var tmplVer int
	if err := r.h.DBScan(ctx, `SELECT cp.template_id::text, cp.template_version FROM worker_prompts wp JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id
		WHERE wp.id = '`+c.WorkerPromptID.String()+`'`, &tmplID, &tmplVer); err != nil {
		t.Fatal(err)
	}
	in := persisted.Captured.PromptInput(persisted.WorkUnitID, persisted.CreatedAt)
	res, err := r.h.App.PromptCache().PrepareForContext(ctx, r.pos.ProjectID, service.PrepareRequest{SnapshotID: *persisted.SnapshotID,
		TemplateID: p14UUID(t, tmplID), TemplateVersion: tmplVer, Continuation: &in, ContinuationMode: service.ContinuationResetSession})
	if err != nil {
		t.Fatal(err)
	}
	var again string
	if err := r.h.DBScan(ctx, "SELECT content FROM canonical_prompts WHERE id = '"+res.CanonicalPromptID.String()+"'", &again); err != nil {
		t.Fatal(err)
	}
	if again != original {
		t.Fatal("T14.30.3 prompt not reconstructable from persisted state")
	}
}

// P14-69 (14.119) — even in the same session the prompt is understandable
// without the conversation history: it carries the same structured
// sections as a fresh-session continuation.
func TestP14119SameSessionIndependence(t *testing.T) {
	r := p14NewRig(t)
	r.initial(t, p14Report("incomplete", []string{"parser", "repository"}, []string{"handler"}, "Implement the handler"), 20000)
	r.reply(p14Report("complete", []string{"parser", "repository", "handler"}, nil, "x"), 21000)
	r.process(t)
	p := r.lastContent(t)
	for _, want := range []string{r.pos.UnitID.String(), "Objective 11.3", "AC-1", "completed: parser; repository", "remaining: handler",
		"Implement the handler", "Response protocol v1", prompt.AuthoritativeContinuationOpen} {
		if !strings.Contains(p, want) {
			t.Fatalf("14.119 same-session prompt relies on history: missing %q", want)
		}
	}
}
