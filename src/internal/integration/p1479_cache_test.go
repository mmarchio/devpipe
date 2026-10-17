package integration

import (
	"context"
	"strings"
	"testing"

	"overseer/internal/cache"
	"overseer/internal/continuation"
	"overseer/internal/persistence"
	"overseer/internal/precompute"
	"overseer/internal/prompt"
)

// P14-79 — WBS 14.32-14.34: continuation prompts use the normal cache path;
// the captured status is part of the semantic identity; precomputed
// continuation templates never contain a worker's prior status.
func TestP1432ContinuationCache(t *testing.T) {
	r := p14NewRig(t)
	ctx := context.Background()
	st, _, err := r.h.App.PromptCache().CacheSettings(ctx, r.pos.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	st.CacheSameSessionContinuation = true
	if _, err := r.h.App.PromptCache().UpdateCacheSettings(ctx, r.pos.ProjectID, st); err != nil {
		t.Fatal(err)
	}
	same := p14Report("incomplete", []string{"A"}, []string{"B"}, "Implement B")
	r.initial(t, same, 5000)
	r.reply(same, 6000) // C1's result repeats the status exactly → C2 has the same captured status
	r.reply(p14Report("incomplete", []string{"A"}, []string{"B", "C"}, "Implement B"), 7000)
	r.reply(p14Report("complete", []string{"A", "B", "C"}, nil, "x"), 8000)
	r.process(t)
	cs := r.unitContinuations(t)
	if len(cs) != 3 {
		t.Fatalf("setup: %d continuations", len(cs))
	}
	c1, c2, c3 := cs[0], cs[1], cs[2]
	if c1.StatusHash != c2.StatusHash || c2.StatusHash == c3.StatusHash {
		t.Fatal("setup: status hashes")
	}
	// T14.32.2: the first continuation prompt was composed + compressed on a miss.
	if p14TemplateKey(t, r, *c1.WorkerPromptID) != "CONTINUE_WORK_UNIT" {
		t.Fatal("T14.32.2 not composed through the continuation template")
	}
	// T14.32.1: the identical captured status reuses the cached prompt.
	if *c2.WorkerPromptID != *c1.WorkerPromptID {
		t.Fatal("T14.32.1 cache hit not used for an identical continuation")
	}
	// 14.33 / T14.32.3: a changed remaining list is a different semantic identity.
	if *c3.WorkerPromptID == *c1.WorkerPromptID {
		t.Fatal("14.33 changed remaining reused a stale prompt")
	}
	layer := cache.LayerWorker
	entries, err := r.h.App.PromptCache().ListEntries(ctx, persistence.PromptCacheFilter{ProjectID: r.pos.ProjectID, Layer: &layer})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, e := range entries {
		if e.WorkerPromptID != nil {
			keys[e.WorkerPromptID.String()] = e.CacheKey
		}
	}
	if k1, k3 := keys[c1.WorkerPromptID.String()], keys[c3.WorkerPromptID.String()]; k1 == "" || k3 == "" || k1 == k3 {
		t.Fatalf("14.33 semantic keys %q %q", k1, k3)
	}
	_ = continuation.StatusCompleted
}

func TestP1434PrecomputedContinuationTemplate(t *testing.T) {
	r := p14NewRig(t)
	r.initial(t, p14Report("incomplete", []string{"A-SECRET-PRIOR"}, []string{"B-SECRET-PRIOR"}, "next-SECRET"), 5000)
	r.warm(t, precompute.TriggerWorkerDispatch)
	rows, err := r.h.App.DB().Raw().Query(context.Background(), `SELECT cp.content FROM precomputed_templates pt
		JOIN canonical_prompts cp ON cp.id = pt.canonical_prompt_id JOIN prompt_templates t ON t.id = cp.template_id
		WHERE pt.project_id = $1 AND t.key = 'CONTINUE_WORK_UNIT'`, r.pos.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatal(err)
		}
		n++
		if strings.Contains(content, "SECRET-PRIOR") || strings.Contains(content, prompt.PriorWorkerReportOpen) {
			t.Fatal("14.34 precomputed continuation template carries prior status")
		}
	}
	// The continuation candidate cannot be fully precomputed: the worker
	// status is unknowable before the worker reports (WBS 14.34).
	var cands, full int
	if err := r.h.DBScan(context.Background(), "SELECT count(*), count(*) FILTER (WHERE readiness = 'FULLY_KNOWN') FROM precompute_candidates WHERE project_id = '"+
		r.pos.ProjectID.String()+"' AND state_key = 'CONTINUE'", &cands, &full); err != nil {
		t.Fatal(err)
	}
	if cands == 0 || full != 0 {
		t.Fatalf("14.34 CONTINUE candidates %d, fully known %d (templates %d)", cands, full, n)
	}
	var leaked int
	if err := r.h.DBScan(context.Background(), `SELECT count(*) FROM prompt_cache_entries e JOIN worker_prompts wp ON wp.id = e.worker_prompt_id
		WHERE e.project_id = '`+r.pos.ProjectID.String()+`' AND e.source = 'PRECOMPUTED' AND wp.content LIKE '%SECRET-PRIOR%'`, &leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("14.34 precomputed prompt carries a prior worker status")
	}
}
