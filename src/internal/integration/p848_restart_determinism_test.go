package integration

import (
	"context"
	"testing"

	"overseer/internal/dbrepo"
)

// P8-48 (WBS 8.49): the restart determinism test over the P8-38 composer
// service. Compose a canonical prompt, record (content, hash, estimated
// tokens, provenance, snapshot id/hash, template version), RESTART the
// service (close the harness, boot a fresh one on the SAME persisted
// database — the caller-owned-DB / NewHarnessOn pattern), and recompose from
// the identical historical inputs (the persisted snapshot + the pinned
// template version). Content, hash, provenance, component ordering, and the
// token estimate (estimator unchanged) must be identical.
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - RESTART: h1.Close() + NewHarnessOn(base, sameDBName) — a fresh
//     service boot over the same persisted rows, no re-seeding of the
//     composed inputs (the p834RestartRig shape, P8-34).
//   - HISTORICAL INPUTS: the same snapshot id + template id + version —
//     exactly the pinned inputs the first composition consumed. A template
//     version APPEND after the restart (v2) must not move the output: the
//     recomposition is addressed at the pinned v1, so the pin is
//     load-bearing (a "latest" read would render the v2 content).
//   - SNAPSHOT HASH: context_snapshots carries no hash column (P7-18); the
//     WBS 8.49 "snapshot id/hash" record is the snapshot id plus a
//     deterministic digest of its persisted pins (ORDER BY type_key) —
//     computed identically on both boots.
//   - PROVENANCE: the WBS 8.37 provenance endpoint entries (pinned source
//     ids/versions/hashes) are the provenance surface — byte-identical
//     across the restart; the byte-identical CONTENT additionally pins the
//     component ordering (the content renders components in composed order).

// p848SnapshotHash derives the snapshot's hash record (pin digest).
func p848SnapshotHash(t *testing.T, h *Harness, snapID string) string {
	t.Helper()
	var out string
	if err := h.DBScan(context.Background(),
		"SELECT coalesce(string_agg(type_key||'='||version::text||'='||content_hash, '|' ORDER BY type_key), '') FROM context_snapshot_files WHERE snapshot_id = '"+snapID+"'",
		&out); err != nil {
		t.Fatalf("scan snapshot pins: %v", err)
	}
	return snapID + "|" + out
}

// p848TmplRepo builds the template-version repository on the harness pool.
func p848TmplRepo(h *Harness) *dbrepo.PromptTemplateVersionRepository {
	return dbrepo.NewPromptTemplateVersionRepository(h.App.DB().Raw())
}

// T8.49.1 — Restart determinism: a restart of the service (fresh boot on the
// SAME persisted database) leaves the composition byte-identical — content,
// hash, provenance, component ordering, and token estimate — when the
// composition is issued from the identical historical inputs (the persisted
// snapshot + the pinned template version).
func TestT8491RestartDeterminism(t *testing.T) {
	h, dbName := p834RestartRig(t)
	ctx := context.Background()
	_, snapID, templateID, _ := p838Fixture(t, h)
	svc := p838Composer(t, h)
	req := p838Request(snapID, templateID)

	rec, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	// Record the pre-restart fields (WBS 8.49: content, hash, estimated
	// tokens, provenance, snapshot id/hash, template version).
	beforeContent, beforeHash, beforeEst := rec.Content, rec.Hash, rec.EstimatedTokens
	beforeSnapHash := p848SnapshotHash(t, h, snapID.String())
	if beforeContent == "" || beforeEst <= 0 || beforeSnapHash == "" {
		t.Fatalf("non-vacuity: empty content, zero estimate, or empty snapshot hash")
	}
	beforeProvenance := p842GetEntries(t, h, rec.ProjectID, rec.ID)
	if len(beforeProvenance) == 0 {
		t.Fatalf("non-vacuity: no provenance entries")
	}

	// Advance the template source AFTER the capture so the pinned version is
	// genuinely historical at recomposition time (v2 exists; the request
	// pins v1 — a "latest" read would render the v2 content).
	if _, err := p848TmplRepo(h).AppendVersion(ctx, templateID, "v2 template source — pinned-input check"); err != nil {
		t.Fatalf("advance template: %v", err)
	}

	// RESTART: close the harness, boot a fresh service on the SAME database.
	h.Close()
	h2 := NewHarnessOn(t, baseDSN(), dbName)
	defer h2.Close()
	if !h2.App.IsReady() {
		t.Fatal("restart: app not ready")
	}

	// Recompose from the identical historical inputs.
	rec2, err := p838Composer(t, h2).ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("recompose after restart: %v", err)
	}

	// Content identical (this also pins the component ORDERING — the content
	// renders the components in their composed order).
	if rec2.Content != beforeContent {
		t.Fatalf("T8.49: content altered by restart:\\nbefore=%q\\nafter=%q", beforeContent, rec2.Content)
	}
	// Hash identical.
	if rec2.Hash != beforeHash {
		t.Fatalf("T8.49: hash altered by restart: %q vs %q", beforeHash, rec2.Hash)
	}
	// Token estimate identical (estimator/version unchanged).
	if rec2.EstimatedTokens != beforeEst {
		t.Fatalf("T8.49: token estimate altered by restart: %d vs %d", beforeEst, rec2.EstimatedTokens)
	}
	// Provenance identical (pinned source ids/versions/hashes, same order).
	afterProvenance := p842GetEntries(t, h2, rec2.ProjectID, rec2.ID)
	if len(afterProvenance) != len(beforeProvenance) {
		t.Fatalf("T8.49: provenance entry count changed across restart: %d -> %d", len(beforeProvenance), len(afterProvenance))
	}
	for i := range beforeProvenance {
		if afterProvenance[i] != beforeProvenance[i] {
			t.Fatalf("T8.49: provenance entry[%d] altered by restart: %+v vs %+v", i, afterProvenance[i], beforeProvenance[i])
		}
	}
	// Snapshot id/hash and template version unchanged on the recomposed
	// record (the historical inputs are the same pinned rows).
	if rec2.ContextSnapshotID != snapID || rec2.TemplateID != templateID || rec2.TemplateVersion != 1 {
		t.Fatalf("T8.49: recomposed inputs drifted: snap=%s tmpl=%s v%d", rec2.ContextSnapshotID, rec2.TemplateID, rec2.TemplateVersion)
	}
	if got := p848SnapshotHash(t, h2, snapID.String()); got != beforeSnapHash {
		t.Fatalf("T8.49: snapshot hash altered by restart: %q vs %q", got, beforeSnapHash)
	}
}
