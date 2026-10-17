package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"overseer/internal/cache"
	"overseer/internal/compression"
	"overseer/internal/dbrepo"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

// P10-28/P10-47 (WBS 10.16/10.50/10.13-10.17): MISS → compose → compress →
// cache; the identical second preparation HITS without composition or a
// provider call, returning the identical worker prompt; hit count and
// last-used advance.
func TestP1028MissThenHit(t *testing.T) {
	h := NewHarness(t)
	e := p10Setup(t, h, p9DefaultSpec())

	first := e.prepare(t, e.req(e.pos.SnapID))
	if first.Hit || e.sc.Calls() != 1 || first.WorkerPrompt.Source != compression.SourceCompressed || first.Entry == nil {
		t.Fatalf("T10.16.3 first preparation: hit=%v calls=%d %+v", first.Hit, e.sc.Calls(), first.Entry)
	}
	if first.Entry.Status != cache.StatusValid || first.Entry.Source != cache.SourceOnDemand || first.Entry.CompressedPromptID == nil ||
		!strings.HasPrefix(first.Entry.CacheKey, "prompt-cache:v2:") {
		t.Fatalf("T10.16.4/T10.15.1 stored entry %+v", first.Entry)
	}
	canonRows := e.count(t, "canonical_prompts", "")
	firstUsed := first.Entry.LastUsedAt

	time.Sleep(5 * time.Millisecond)
	second := e.prepare(t, e.req(e.pos.SnapID))
	if !second.Hit || e.sc.Calls() != 1 {
		t.Fatalf("T10.16.1 hit avoided provider: hit=%v calls=%d", second.Hit, e.sc.Calls())
	}
	if e.count(t, "canonical_prompts", "") != canonRows {
		t.Fatal("T10.16.2 hit composed a new canonical prompt")
	}
	if second.WorkerPrompt.ID != first.WorkerPrompt.ID || second.WorkerPrompt.Hash != first.WorkerPrompt.Hash {
		t.Fatal("T10.50 hit returned a different worker prompt")
	}
	stored, _ := dbrepo.NewPromptCacheRepository(h.App.DB().Raw()).Get(context.Background(), first.Entry.ID)
	if stored.HitCount != 1 || !stored.LastUsedAt.After(firstUsed) {
		t.Fatalf("T10.13.4/T10.13.5 hit count %d last used %v", stored.HitCount, stored.LastUsedAt)
	}
	// A fresh snapshot with identical semantic content still hits.
	third := e.prepare(t, e.req(e.snapshot(t)))
	if !third.Hit || e.sc.Calls() != 1 {
		t.Fatal("identical new snapshot missed")
	}
	st, _ := e.cache.Stats(context.Background(), e.pos.ProjectID)
	if st.Lookups != 3 || st.Hits != 2 || st.Misses != 1 || st.CompressionCallsAvoided != 2 || st.EstimatedTokensAvoided == 0 {
		t.Fatalf("T10.37 stats %+v", st)
	}
	for _, typ := range []string{"CACHE_LOOKUP", "CACHE_HIT", "CACHE_MISS", "CACHE_STORE", "CACHE_BUILD_STARTED", "CACHE_BUILD_COMPLETED"} {
		if e.events(t, typ) == 0 {
			t.Fatalf("T10.66 %s not emitted", typ)
		}
	}
}

// P10-45 (WBS 10.19/10.7/10.51): each state-file edit (new snapshot) misses;
// the old entry stays VALID and queryable; role/state fragments remain
// reusable after an ARCHITECTURE change.
func TestP1045StateFileChangesMiss(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	e := p10Setup(t, h, p9DefaultSpec())
	base := e.prepare(t, e.req(e.pos.SnapID))
	keys := map[string]bool{base.CacheKey: true}
	for _, f := range []string{"ACTIVE_TASK", "ARCHITECTURE", "PROJECT_STATE", "PHASE_STATE"} {
		content := f + ": edited content for the cache test.\n" + p9Background(f+"-EDIT", 120)
		if f == "ACTIVE_TASK" { // exact (task context): keep it small
			content = "ACTIVE_TASK v2: implement work unit 9.17 (edited)."
		}
		e.writeFile(t, f, content)
		calls := e.sc.Calls()
		res := e.prepare(t, e.req(e.snapshot(t)))
		if res.Hit || keys[res.CacheKey] || e.sc.Calls() != calls+1 {
			t.Fatalf("T10.19 %s edit did not miss (hit=%v)", f, res.Hit)
		}
		keys[res.CacheKey] = true
	}
	old, err := dbrepo.NewPromptCacheRepository(h.App.DB().Raw()).Get(ctx, base.Entry.ID)
	if err != nil || old.Status != cache.StatusValid || old.WorkerPromptID == nil || *old.WorkerPromptID != base.WorkerPrompt.ID {
		t.Fatal("T10.19.4/T10.18.2 old entry not intact")
	}
	// The old snapshot still resolves its historical entry.
	if again := e.prepare(t, e.req(e.pos.SnapID)); !again.Hit || again.WorkerPrompt.ID != base.WorkerPrompt.ID {
		t.Fatal("historical snapshot lost its entry")
	}
	// WBS 10.51: the SYSTEM/ROLE and STATE prefix fragments were reused
	// across the architecture change; the static-context fragment was not.
	if e.count(t, "prompt_cache_events", " AND event_type = 'CACHE_HIT' AND layer = 'FRAGMENT' AND reason = 'SYSTEM_ROLE_PREFIX'") == 0 ||
		e.count(t, "prompt_cache_events", " AND event_type = 'CACHE_HIT' AND layer = 'FRAGMENT' AND reason = 'STATE_PREFIX'") == 0 {
		t.Fatal("static role/state fragments not reused")
	}
	frags, _ := e.cache.Fragments(ctx, e.pos.ProjectID)
	static := 0
	for _, f := range frags {
		if f.Kind == cache.FragmentStaticContext {
			static++
		}
	}
	if static < 2 {
		t.Fatalf("architecture change did not produce a new static-context fragment (%d)", static)
	}
	st, _ := e.cache.Stats(ctx, e.pos.ProjectID)
	if st.FragmentHits == 0 || st.WorkerHits != 1 {
		t.Fatalf("T10.48.3 metrics distinguish fragment and worker hits: %+v", st)
	}
}

// P10-47 (WBS 10.21): spec / acceptance / dependency changes of the bound
// unit miss; a change to an unrelated unit does not.
func TestP1047WorkUnitSpecChanges(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	e := p10Setup(t, h, p9DefaultSpec())
	other := p710UnitWithSpec(t, h, e.pos.PlanID, 2, "10.18", "PENDING", []string{"other AC"}, []string{"other test"}, nil)
	base := e.prepare(t, e.req(e.pos.SnapID))

	if _, err := h.DBExec(ctx, `UPDATE work_units SET acceptance_criteria = '["changed criterion"]' WHERE id = '`+other.String()+`'`); err != nil {
		t.Fatal(err)
	}
	if res := e.prepare(t, e.req(e.pos.SnapID)); !res.Hit {
		t.Fatal("T10.21.4 unrelated unit change invalidated the entry")
	}
	if _, err := h.DBExec(ctx, `UPDATE work_units SET acceptance_criteria = acceptance_criteria || '["AC-5: new criterion"]'::jsonb WHERE id = '`+e.pos.UnitID.String()+`'`); err != nil {
		t.Fatal(err)
	}
	res := e.prepare(t, e.req(e.pos.SnapID))
	if res.Hit || res.CacheKey == base.CacheKey || !strings.Contains(res.WorkerPrompt.Content, "AC-5: new criterion") {
		t.Fatal("T10.21.1/T10.21.3 acceptance change did not miss")
	}
	if _, err := h.DBExec(ctx, `UPDATE work_units SET objective = 'Objective changed' WHERE id = '`+e.pos.UnitID.String()+`'`); err != nil {
		t.Fatal(err)
	}
	if res2 := e.prepare(t, e.req(e.pos.SnapID)); res2.Hit || res2.CacheKey == res.CacheKey {
		t.Fatal("T10.21.1 specification change did not miss")
	}
	// T10.21.2 dependency change: the unit's dependency set is a cache
	// input (WorkUnitSpecHash, sorted) — pinned at the domain level since
	// the Phase 8 composer loads no dependency names (P8-44 note).
}

// P10-48 (WBS 10.22/10.52): an ENGINEER instruction edit and a state-file
// requirement change change the role hash and the key; the old entry
// remains; the prompt is recomposed and recompressed.
func TestP1048RoleChanges(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	raw := h.App.DB().Raw()
	e := p10Setup(t, h, p9DefaultSpec())
	base := e.prepare(t, e.req(e.pos.SnapID))
	canon := e.count(t, "canonical_prompts", "")

	if _, err := dbrepo.NewInstructionVersionRepository(raw).AppendVersion(ctx, e.pos.InstructionID.String(), "ENGINEER.md v2: write the failing test first; keep units small."); err != nil {
		t.Fatal(err)
	}
	res := e.prepare(t, e.req(e.snapshot(t)))
	if res.Hit || res.CacheKey == base.CacheKey || e.sc.Calls() != 2 || e.count(t, "canonical_prompts", "") != canon+1 {
		t.Fatalf("T10.22.2/T10.52: hit=%v calls=%d", res.Hit, e.sc.Calls())
	}
	if !strings.Contains(res.WorkerPrompt.Content, "ENGINEER.md v2") {
		t.Fatal("recomposed prompt lacks the new instruction")
	}
	if got, _ := dbrepo.NewPromptCacheRepository(raw).Get(ctx, base.Entry.ID); got.Status != cache.StatusValid {
		t.Fatal("T10.22.4 old role entry not retained")
	}
	p712RequireRoleFiles(t, h, e.pos.RoleID, []string{"PROJECT_STATE", "ARCHITECTURE", "PHASE_STATE", "ACTIVE_TASK", "TEST_STATE"})
	res2 := e.prepare(t, e.req(e.snapshot(t)))
	if res2.Hit || res2.CacheKey == res.CacheKey {
		t.Fatal("T10.22.3 state-file requirement change did not miss")
	}
}

// P10-49 (WBS 10.8/10.9/10.23): template version, response-protocol
// version and state-instruction edits miss; historical entries remain
// resolvable.
func TestP1049TemplateProtocolStateChanges(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	raw := h.App.DB().Raw()
	e := p10Setup(t, h, p9DefaultSpec())
	base := e.prepare(t, e.req(e.pos.SnapID))
	if hit := e.prepare(t, e.req(e.pos.SnapID)); !hit.Hit {
		t.Fatal("T10.8.1 same template must hit")
	}
	// Template v2 with byte-identical text still misses (version identity).
	if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, e.pos.TemplateID, "Implement the active work unit.\n"); err != nil {
		t.Fatal(err)
	}
	v2 := e.req(e.pos.SnapID)
	v2.TemplateVersion = 2
	if res := e.prepare(t, v2); res.Hit || res.CacheKey == base.CacheKey {
		t.Fatal("T10.8.2 template version change did not miss")
	}
	if res := e.prepare(t, e.req(e.pos.SnapID)); !res.Hit {
		t.Fatal("T10.8.3 historical template entry not resolvable")
	}
	// Response protocol v2 pinned by the state.
	if _, err := dbrepo.NewResponseProtocolRepository(raw).AppendVersion(ctx, e.pos.ProtocolID, p9Protocol+"\n\"notes\": \"...\""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx, "UPDATE state_protocol_references SET protocol_version = 2 WHERE state_id = '"+e.pos.StateID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	pr := e.prepare(t, e.req(e.pos.SnapID))
	if pr.Hit || pr.CacheKey == base.CacheKey {
		t.Fatal("T10.9.2 protocol edit did not miss")
	}
	if got, _ := dbrepo.NewPromptCacheRepository(raw).Get(ctx, base.Entry.ID); got.Status != cache.StatusValid {
		t.Fatal("T10.9.3 historical protocol entry lost")
	}
	// State instruction v2.
	if _, err := dbrepo.NewStateInstructionRepository(raw).AppendVersion(ctx, e.pos.StateID, "IMPLEMENT v2: implement and report."); err != nil {
		t.Fatal(err)
	}
	if si := e.prepare(t, e.req(e.pos.SnapID)); si.Hit || si.CacheKey == pr.CacheKey {
		t.Fatal("T10.23 state instruction change did not miss")
	}
}

// P10-50 (WBS 10.53/10.54/10.55/10.10/10.11/10.25): compression model,
// target and context-window changes miss Layer 3 but reuse the Layer 1
// canonical prompt; AUTO→ALWAYS misses a bypassed entry.
func TestP1050CompressionConfigLayers(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	e := p10Setup(t, h, p9DefaultSpec())
	base := e.prepare(t, e.req(e.pos.SnapID))
	canon := e.count(t, "canonical_prompts", "")

	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_model = 'model-B' WHERE project_id = '"+e.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	mb := e.prepare(t, e.req(e.pos.SnapID))
	if mb.Hit || mb.CacheKey == base.CacheKey || mb.CanonicalKey != base.CanonicalKey || !hasLayer(mb, "CANONICAL") {
		t.Fatalf("T10.53/T10.54.1 model change: hit=%v layers=%v", mb.Hit, mb.Layers)
	}
	if e.count(t, "canonical_prompts", "") != canon || mb.CanonicalPromptID != base.CanonicalPromptID {
		t.Fatal("T10.54.4 canonical prompt rebuilt for a compression-only change")
	}
	if e.sc.Calls() != 2 || mb.WorkerPrompt.ID == base.WorkerPrompt.ID || mb.Run.Model != "model-B" {
		t.Fatal("T10.55.1 Model B did not recompress")
	}
	// Target change.
	p9Configure(t, h, e.comp, e.pos.ProjectID, 64000, p9Settings(3500, 2))
	if tg := e.prepare(t, e.req(e.pos.SnapID)); tg.Hit || !hasLayer(tg, "CANONICAL") {
		t.Fatal("T10.10.3 target change did not miss")
	}
	// Context window: reduce and increase both miss (strict identity).
	p9Configure(t, h, e.comp, e.pos.ProjectID, 32000, p9Settings(3500, 2))
	cw := e.prepare(t, e.req(e.pos.SnapID))
	if cw.Hit {
		t.Fatal("T10.25.2 reduced context did not miss")
	}
	if same := e.prepare(t, e.req(e.pos.SnapID)); !same.Hit {
		t.Fatal("T10.25.1 same context must hit")
	}
	p9Configure(t, h, e.comp, e.pos.ProjectID, 128000, p9Settings(3500, 2))
	if up := e.prepare(t, e.req(e.pos.SnapID)); up.Hit {
		t.Fatal("T10.25.3 increased context follows the strict identity policy")
	}
	// Bypass identity (WBS 10.11): target above the canonical size → AUTO
	// bypass cached; AUTO→ALWAYS misses.
	auto := p9Settings(60000, 2)
	p9Configure(t, h, e.comp, e.pos.ProjectID, 128000, auto)
	by := e.prepare(t, e.req(e.pos.SnapID))
	if by.WorkerPrompt.Source != compression.SourceCanonical || by.Entry == nil || by.Entry.CompressedPromptID != nil {
		t.Fatal("T10.11.1 bypassed worker prompt not cached")
	}
	if again := e.prepare(t, e.req(e.pos.SnapID)); !again.Hit || again.WorkerPrompt.ID != by.WorkerPrompt.ID {
		t.Fatal("T10.11.2 bypass entry not reused")
	}
	always := auto
	always.Mode = compression.ModeAlways
	p9Configure(t, h, e.comp, e.pos.ProjectID, 128000, always)
	if al := e.prepare(t, e.req(e.pos.SnapID)); al.Hit || al.WorkerPrompt.Source != compression.SourceCompressed {
		t.Fatal("T10.11.3 AUTO→ALWAYS did not miss")
	}
	// Layer 2 reuse: dropping back to model-B/target-3500/32k context
	// misses Layer 3 only if never seen — it was seen, so it hits Layer 3.
	p9Configure(t, h, e.comp, e.pos.ProjectID, 32000, p9Settings(3500, 2))
	if back := e.prepare(t, e.req(e.pos.SnapID)); !back.Hit {
		t.Fatal("returning to a cached configuration must hit")
	}
}

// P10-51 (WBS 10.56/10.57): reset-session continuation status A hits on
// repeat, status B misses; same-session caching is off by default.
func TestP1051ContinuationCache(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	e := p10Setup(t, h, p9DefaultSpec())
	statusA := &prompt.ContinuationStatusInput{WorkUnitID: e.pos.UnitID, Completed: []string{"repository"}, Remaining: []string{"tests"}}
	statusB := &prompt.ContinuationStatusInput{WorkUnitID: e.pos.UnitID, Completed: []string{"repository", "tests"}, Remaining: []string{"integration"}}
	reset := func(st *prompt.ContinuationStatusInput) *servicePrepare {
		r := e.req(e.pos.SnapID)
		r.Continuation, r.ContinuationMode = st, "RESET_SESSION"
		return &servicePrepare{r}
	}
	a1 := e.prepare(t, reset(statusA).r)
	a2 := e.prepare(t, reset(statusA).r)
	if a1.Hit || !a2.Hit || a2.WorkerPrompt.ID != a1.WorkerPrompt.ID {
		t.Fatal("WBS 10.56 identical status must hit")
	}
	b := e.prepare(t, reset(statusB).r)
	if b.Hit || b.CacheKey == a1.CacheKey || !strings.Contains(b.WorkerPrompt.Content, "remaining: integration") {
		t.Fatal("WBS 10.56 changed status must miss")
	}
	plain := e.prepare(t, e.req(e.pos.SnapID))
	if plain.CacheKey == a1.CacheKey {
		t.Fatal("continuation hash not part of identity")
	}
	same := func() *servicePrepare {
		r := e.req(e.pos.SnapID)
		r.Continuation, r.ContinuationMode = statusA, "SAME_SESSION"
		return &servicePrepare{r}
	}
	before := e.count(t, "prompt_cache_entries", "")
	s1 := e.prepare(t, same().r)
	if !s1.Bypassed || s1.Hit || e.count(t, "prompt_cache_entries", "") != before {
		t.Fatal("T10.57.1 same-session continuation must bypass the cache by default")
	}
	st, _, _ := e.cache.CacheSettings(ctx, e.pos.ProjectID)
	st.CacheSameSessionContinuation = true
	if _, err := e.cache.UpdateCacheSettings(ctx, e.pos.ProjectID, st); err != nil {
		t.Fatal(err)
	}
	s2 := e.prepare(t, same().r)
	s3 := e.prepare(t, same().r)
	if s2.Bypassed || !s3.Hit {
		t.Fatal("T10.57.2 enabled same-session caching must permit reuse")
	}
	if s2.CacheKey == a1.CacheKey {
		t.Fatal("T10.57.3 same-session and reset-session identities must be independent")
	}
	if r := e.prepare(t, reset(statusA).r); !r.Hit {
		t.Fatal("T10.57.3 reset-session policy affected")
	}
}

type servicePrepare struct{ r service.PrepareRequest }
