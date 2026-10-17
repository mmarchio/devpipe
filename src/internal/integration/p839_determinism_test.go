package integration

import (
	"context"
	"testing"

	"overseer/internal/prompt"
	"overseer/internal/role"
	"overseer/internal/statefile"

	"github.com/google/uuid"
)

// P8-39 (WBS 8.32): completes the prompt-determinism test set over real
// PostgreSQL. T8.32.1 (content) / T8.32.2 (hash) are already pinned in P8-38
// (TestP838RepeatedCompositionByteIdentical) and are re-verified here; this
// task adds T8.32.3 (token-estimate determinism) and T8.32.4 (the
// database-row-order half).
//
// Task-level choices (stated so TESTER/PM can verify; the WBS does not fix
// them):
//   - T8.32.3: the estimate is asserted byte-identical across three
//     compositions over the same pinned snapshot (a single-pair assert would
//     be indistinguishable from a stable-but-input-derived value; three
//     calls pin the estimator's determinism on the same inputs).
//   - T8.32.4: the output is a function of the file SET, not the row order.
//     The composer reads pins `ORDER BY type_key` and re-sorts by
//     (section rank, type_key, id), so it is provably invariant to physical
//     row order. Two pins prove it:
//     (a) a pure core pin over an input whose files are supplied in three
//         mutually distinct orders (incl. an order that is NOT the canonical
//         order, so the re-sort is load-bearing — not a vacuous
//         same-order pin), asserting content + hash + estimate are
//         byte-identical;
//     (b) a real-PG pin: the same logical snapshot is re-persisted with its
//         context_snapshot_files rows in reverse physical order, and the
//         recomposed prompt is byte-identical (content + hash + estimate).

// p839PureFile is one state-file pin in the pure-core input.
type p839PureFile struct {
	typeKey string
	class   role.Classification
	content string
}

func (f p839PureFile) snap() statefile.StateFileSnapshot {
	return statefile.StateFileSnapshot{
		StateFileID:    uuid.New(),
		TypeKey:        f.typeKey,
		Scope:          statefile.ScopeProject,
		Version:        1,
		Hash:           "h-" + f.typeKey,
		Content:        f.content,
		Classification: f.class,
	}
}

// p839PureInput builds a minimal valid CanonicalCompositionInput whose state
// file set is the three pins in the caller-supplied order. The three pins
// carry distinct (classification, type_key) so that the canonical
// (section-rank, type_key, id) order differs from any one input order — the
// re-sort is load-bearing.
func p839PureInput(order []p839PureFile) prompt.CanonicalCompositionInput {
	roleSnap := role.RoleSnapshot{
		RoleID:      uuid.New(),
		RoleKey:     "P839-ROLE",
		RoleVersion: 1,
		SystemPrompt: role.PromptSnapshot{
			Version:     1,
			Content:     "You are the p839 engineer.",
			ContentHash: "h-sys",
		},
	}
	var files []statefile.StateFileSnapshot
	for _, f := range order {
		files = append(files, f.snap())
	}
	return prompt.CanonicalCompositionInput{
		ProjectID:           uuid.New(),
		ContextSnapshotID:   uuid.New(),
		TemplateID:          uuid.New(),
		TemplateVersion:     1,
		RoleID:              roleSnap.RoleID,
		RoleVersion:         1,
		StateMachineID:      uuid.New(),
		StateMachineVersion: 1,
		StateKey:            "S",
		Role:                roleSnap,
		StateFiles:          files,
		Required:            []string{"A", "M", "X"},
		ProtocolID:          uuid.New(),
		ProtocolVersion:     1,
		ProtocolContent:     "respond: {status}",
		TemplateSource:      "F: {{project.context}}\n{{phase.context}}\n{{runtime.tests}}",
	}
}

// p839Permutations returns three mutually distinct orderings of the same
// three-file set. The pins carry distinct classifications so the canonical
// order (section rank, type_key) is {X(project), A(phase), M(task)} — the
// first ordering below (A,M,X) is NOT the canonical order, so the composer's
// re-sort is genuinely exercised.
func p839Permutations() [][]p839PureFile {
	a := p839PureFile{"A", role.ClassificationPhaseContext, "phase body A"}
	m := p839PureFile{"M", role.ClassificationTaskContext, "task body M"}
	x := p839PureFile{"X", role.ClassificationProjectContext, "project body X"}
	return [][]p839PureFile{
		{a, m, x}, // input order A,M,X — differs from canonical X,A,M
		{x, a, m}, // canonical order
		{m, x, a}, // reverse canonical
	}
}

// T8.32.3 — The token estimate is deterministic: three compositions over the
// same pinned snapshot yield a byte-identical EstimatedTokens (and, as a
// re-verification of the P8-38 halves, identical content + hash).
func TestP839TokenEstimateDeterministic(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, snapID, templateID, _ := p838Fixture(t, h)
	svc := p838Composer(t, h)
	req := p838Request(snapID, templateID)

	var firstContent, firstHash string
	var firstEst int
	for i := 0; i < 3; i++ {
		rec, err := svc.ComposePrompt(ctx, req)
		if err != nil {
			t.Fatalf("compose #%d: %v", i+1, err)
		}
		if i == 0 {
			firstContent, firstHash, firstEst = rec.Content, rec.Hash, rec.EstimatedTokens
			continue
		}
		if rec.Content != firstContent {
			t.Fatalf("T8.32.1 re-verify: content differs on compose #%d", i+1)
		}
		if rec.Hash != firstHash {
			t.Fatalf("T8.32.2 re-verify: hash differs on compose #%d", i+1)
		}
		if rec.EstimatedTokens != firstEst {
			t.Fatalf("T8.32.3: token estimate not deterministic: #1=%d #%d=%d", firstEst, i+1, rec.EstimatedTokens)
		}
	}
	if firstContent == "" || firstEst <= 0 {
		t.Fatalf("non-vacuity: empty content or zero estimate")
	}
}

// T8.32.4 (pure half) — The composition output is a pure function of the file
// set: supplying the identical three-file set in three mutually distinct
// input orders (one of which is NOT the canonical order) yields
// byte-identical content + hash + estimate.
func TestP839PureOrderInvariant(t *testing.T) {
	var firstContent, firstHash string
	var firstEst int
	for i, order := range p839Permutations() {
		cp, _, err := prompt.ComposeCanonicalPrompt(p839PureInput(order))
		if err != nil {
			t.Fatalf("compose #%d: %v", i+1, err)
		}
		if i == 0 {
			firstContent, firstHash, firstEst = cp.Content, cp.Hash, cp.EstimatedTokens
			continue
		}
		if cp.Content != firstContent {
			t.Fatalf("T8.32.4: content differs across input orders #1 vs #%d:\n#1=%q\n#%d=%q", i+1, firstContent, i+1, cp.Content)
		}
		if cp.Hash != firstHash {
			t.Fatalf("T8.32.4: hash differs across input orders")
		}
		if cp.EstimatedTokens != firstEst {
			t.Fatalf("T8.32.4: estimate differs across input orders")
		}
	}
	if firstContent == "" {
		t.Fatal("non-vacuity: composed content empty")
	}
}

// T8.32.4 (real-PG half) — Physical row order cannot alter the output: the
// same logical snapshot is re-persisted with its context_snapshot_files rows
// in reverse physical order, and the recomposed prompt is byte-identical
// (content + hash + estimate). The composer reads pins ORDER BY type_key and
// re-sorts, so the reverse reinsertion must not move the output.
func TestP839DBRowOrderInvariant(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	_, snapID, templateID, _ := p838Fixture(t, h)
	svc := p838Composer(t, h)
	req := p838Request(snapID, templateID)

	base, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("baseline compose: %v", err)
	}

	// Read the snapshot's file pins in type_key order, then reinsert them in
	// reverse order so the physical/heap order differs while the logical set
	// is unchanged.
	rows, err := h.App.DB().Raw().Query(ctx,
		"SELECT id::text, snapshot_id::text, state_file_id::text, type_key, scope, version, content_hash, content, state_file_token_estimate, classification, created_at::text FROM context_snapshot_files WHERE snapshot_id = $1", snapID)
	if err != nil {
		t.Fatalf("query file pins: %v", err)
	}
	var recs []p839FileRow
	for rows.Next() {
		var r p839FileRow
		if err := rows.Scan(&r.ID, &r.SnapshotID, &r.StateFileID, &r.TypeKey, &r.Scope, &r.Version, &r.Hash, &r.Content, &r.Est, &r.Classification, &r.CreatedAt); err != nil {
			rows.Close()
			t.Fatalf("scan file pin: %v", err)
		}
		recs = append(recs, r)
	}
	rows.Close()
	if len(recs) == 0 {
		t.Fatal("non-vacuity: snapshot has no file pins to reorder")
	}

	if _, err := h.DBExec(ctx, "DELETE FROM context_snapshot_files WHERE snapshot_id = '"+snapID.String()+"'"); err != nil {
		t.Fatalf("clear pins: %v", err)
	}
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if _, err := h.DBExec(ctx,
			"INSERT INTO context_snapshot_files (id, snapshot_id, state_file_id, type_key, scope, version, content_hash, content, state_file_token_estimate, classification, created_at) VALUES ('"+r.ID+"', '"+r.SnapshotID+"', '"+r.StateFileID+"', '"+r.TypeKey+"', '"+r.Scope+"', "+r.Version+", '"+r.Hash+"', '"+r.Content+"', "+r.Est+", '"+r.Classification+"', now())"); err != nil {
			t.Fatalf("reinsert pin: %v", err)
		}
	}

	after, err := svc.ComposePrompt(ctx, req)
	if err != nil {
		t.Fatalf("reordered compose: %v", err)
	}
	if after.Content != base.Content {
		t.Fatalf("T8.32.4: content altered by physical row order:\nbefore=%q\nafter=%q", base.Content, after.Content)
	}
	if after.Hash != base.Hash {
		t.Fatalf("T8.32.4: hash altered by physical row order")
	}
	if after.EstimatedTokens != base.EstimatedTokens {
		t.Fatalf("T8.32.4: estimate altered by physical row order: %d vs %d", base.EstimatedTokens, after.EstimatedTokens)
	}
}

type p839FileRow struct {
	ID, SnapshotID, StateFileID string
	TypeKey, Scope              string
	Version                     string
	Hash                        string
	Content                     string
	Est                         string
	Classification              string
	CreatedAt                   string
}
