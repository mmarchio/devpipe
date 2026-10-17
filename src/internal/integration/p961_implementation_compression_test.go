package integration

import (
	"context"
	"testing"

	"overseer/internal/compression"
	"overseer/internal/compression/compressiontest"
	"overseer/internal/prompt"
)

// P9-61 (WBS 9.50): compress the canonical implementation prompt for Work
// Unit 9.17 and verify the identity, acceptance criteria, tests,
// constraints and response schema survive while the token count is reduced.
// The user <preserve> requirement is exercised by a region inside the
// COMPRESSIBLE ARCHITECTURE state file.
func TestP961ImplementationPromptCompression(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	spec := p9DefaultSpec()
	const keep = "NOTE: deployments must go through ./scripts/release.sh --verify"
	spec.Architecture += "Operational rule: <preserve>" + keep + "</preserve> (keep verbatim).\n" + p9Background("ARCHITECTURE-TAIL", 30)
	pos := p9Rig(t, h, spec)
	rec := p9Compose(t, h, pos)

	// P9-14: components + manifest persisted with the canonical prompt.
	comps, err := p9ComponentRepo(h).ListComponents(ctx, rec.ID)
	if err != nil || len(comps) == 0 {
		t.Fatalf("components not persisted: %v", err)
	}
	manifest, _ := p9ComponentRepo(h).ListManifest(ctx, rec.ID)
	for _, id := range []string{"AC-1", "AC-4", "TEST-3", "CONSTRAINT-2", "WORK_UNIT", "FIELD-status", "STATUS-blocked"} {
		found := false
		for _, it := range manifest {
			found = found || it.ID == id
		}
		if !found {
			t.Fatalf("manifest lacks %s: %+v", id, manifest)
		}
	}

	canonTokens := p9Tokens(rec)
	before := p9CanonicalRow(t, h, rec.ID)
	sc := compressiontest.NewScripted() // heuristic compressor
	svc := p9Service(h, p9Source{compressor: sc})
	p9Configure(t, h, svc, pos.ProjectID, canonTokens*4, p9Settings(canonTokens/2, 2))

	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	wp := res.WorkerPrompt
	if wp == nil || wp.Source != compression.SourceCompressed || res.Run.Outcome != compression.RunCompressed {
		t.Fatalf("expected compressed worker prompt, got run %+v", res.Run)
	}
	p9MustContainAll(t, "worker prompt", wp.Content,
		"name: 9.17", pos.UnitID.String(),
		p9AC1, p9AC2, p9AC3, p9AC4, p9T1, p9T2, p9T3, p9C1, p9C2,
		`"status": "complete|incomplete|blocked|failed"`, `"next_action"`,
		keep, // user <preserve> requirement
		"Implement only the active work unit.", "Do not skip or weaken required tests.",
	)
	if wp.EstimatedTokens >= canonTokens || wp.EstimatedTokens > canonTokens/2 {
		t.Fatalf("token count not reduced: %d vs canonical %d", wp.EstimatedTokens, canonTokens)
	}
	// Every exact fragment survived verbatim.
	for _, f := range compression.ExactFragments(compression.ApplyEffectivePolicies(comps)) {
		if !contains(wp.Content, f.Text) {
			t.Fatalf("exact fragment %s lost", f.ID)
		}
	}
	// Background shrank.
	if contains(wp.Content, "Earlier phase 57 delivered") {
		t.Fatal("compressible background not compressed")
	}
	// Canonical prompt unchanged.
	if p9CanonicalRow(t, h, rec.ID) != before {
		t.Fatal("canonical prompt modified")
	}
	// The compressor saw the policy-framed structured input with the
	// preserve region as its own exact section.
	si, err := compression.ParseStructuredInput(sc.Requests()[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	exactRegion := false
	for _, s := range si.Sections {
		if s.Policy == prompt.PolicyPreserveExact && s.Content == keep {
			exactRegion = true
		}
	}
	if !exactRegion {
		t.Fatal("preserve region not framed as an exact section")
	}
}
