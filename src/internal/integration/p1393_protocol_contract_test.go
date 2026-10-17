package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/precompute"
	"overseer/internal/service"
	"overseer/internal/workerresult"
)

func p13Pin(t *testing.T, r *p13Rig, state string, protocolID uuid.UUID, version int) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE state_protocol_references SET protocol_id = '"+protocolID.String()+"', protocol_version = "+itoa(version)+
		" WHERE state_id = '"+r.states[state].String()+"'"); err != nil {
		t.Fatal(err)
	}
}

// P13-47 (WBS 13.7, 13.93-13.95): producer and parser share one contract;
// the protocol version is part of prompt identity.
func TestP1393ProtocolPromptContract(t *testing.T) {
	r := p13NewRig(t)
	ctx := context.Background()
	old := r.prepareNext(t, service.NextRequest{})

	// T13.7.1: the instructions are resolvable through the Phase 8 protocol
	// system; readiness reports the missing instruction before.
	if err := r.rs().Readiness(ctx, r.pos.ProjectID); err == nil || !strings.Contains(err.Error(), "instruction") {
		t.Fatalf("readiness before ensure: %v", err)
	}
	ref, err := r.rs().EnsureProtocol(ctx, r.pos.ProjectID)
	if err != nil || ref.Version < 2 || !ref.Appended {
		t.Fatalf("ensure %+v %v", ref, err)
	}
	if again, _ := r.rs().EnsureProtocol(ctx, r.pos.ProjectID); again.Version != ref.Version || again.Appended {
		t.Fatal("ensure not idempotent")
	}
	if err := r.rs().Readiness(ctx, r.pos.ProjectID); err != nil {
		t.Fatalf("13.97 %v", err)
	}
	for _, st := range []string{"IMPLEMENT", "CONTINUE", "FIX"} {
		p13Pin(t, r, st, ref.ProtocolID, ref.Version)
	}

	// 13.93: the actual worker prompt carries the contract.
	cur := r.prepareNext(t, service.NextRequest{})
	for _, want := range []string{"protocol 1.0", `"protocol_version": "1.0"`, `"complete", "incomplete", "blocked", "failed"`, `"summary"`, `"completed"`,
		`"remaining"`, `"blocking"`, `"next_action"`, "exactly one JSON object", "Do not report tests as passing unless they were actually executed and passed"} {
		if !strings.Contains(cur.WorkerPrompt.Content, want) {
			t.Fatalf("13.93 prompt lacks %q", want)
		}
	}
	// T13.94.1/T13.94.2 + T13.7.4: new protocol version → new identity.
	if cur.CacheKey == old.CacheKey || cur.Hit || cur.WorkerPrompt.ID == old.WorkerPrompt.ID {
		t.Fatal("13.94 protocol change reused the old prompt")
	}
	// The parser accepts what the prompt asks for.
	d := r.report(t, p13Complete)
	if r.result(t, d).Result.Status != workerresult.StatusComplete {
		t.Fatal("producer/parser disagree")
	}

	// 13.95: a precomputed next-unit prompt keyed to protocol 1.0 is not
	// used once the protocol moves to 1.1.
	res := r.warm(t, precompute.TriggerWorkerDispatch)
	next := r.candidates(t, res.JobID)["IMPLEMENT+next"]
	if next.CacheEntryID == nil {
		t.Fatalf("no precomputed next-unit prompt: %+v", next)
	}
	st := workerresult.DefaultSettings()
	st.ProtocolVersion = workerresult.ProtocolVersion{Major: 1, Minor: 1}
	if _, err := r.rs().UpdateProtocolSettings(ctx, r.pos.ProjectID, st); err != nil {
		t.Fatal(err)
	}
	ref2, _ := r.rs().EnsureProtocol(ctx, r.pos.ProjectID)
	for _, s := range []string{"IMPLEMENT", "CONTINUE", "FIX"} {
		p13Pin(t, r, s, ref2.ProtocolID, ref2.Version)
	}
	r.moveTo(t, "VALIDATE", r.units["11.3"])
	r.markDone(t, r.units["11.3"])
	r.moveTo(t, "IMPLEMENT", r.units["11.4"])
	got := r.prepareNext(t, service.NextRequest{})
	if got.Entry != nil && got.Entry.ID == *next.CacheEntryID {
		t.Fatal("13.95 stale precomputed prompt used")
	}
	if !strings.Contains(got.WorkerPrompt.Content, "protocol 1.1") || got.Entry == nil || got.Entry.Source == cache.SourcePrecomputed {
		t.Fatalf("13.95 new prompt not generated (entry %+v)", got.Entry)
	}
}
