package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/compression"
	"overseer/internal/persistence"
	"overseer/internal/precompute"
	"overseer/internal/statefile"
)

// LayerTemplate reports a resolved Phase 11 placeholder template in
// PrepareResult.Layers (never a stored entry layer).
const LayerTemplate cache.Layer = "TEMPLATE"

// compose composes the resolved input, recording the projection when the
// input is a Phase 11 projected context.
func (s *PromptCacheService) compose(ctx context.Context, b *cacheInput) (*persistence.CanonicalPromptRecord, error) {
	if b.projectionID != nil {
		return s.composer.ComposeResolvedForProjection(ctx, b.composition, b.canonicalKey, *b.projectionID)
	}
	return s.composer.ComposeResolved(ctx, b.composition, b.canonicalKey)
}

// activeTaskPin returns the snapshot record's ACTIVE_TASK pin (work-unit
// scoped), if any.
func activeTaskPin(rec *persistence.ContextSnapshotRecord) *persistence.ContextSnapshotFileRecord {
	for _, f := range rec.Files {
		if f.TypeKey == "ACTIVE_TASK" {
			return f
		}
	}
	return nil
}

// WithPlaceholderActiveTask returns a copy of the record whose ACTIVE_TASK
// pin is the unit's placeholder pin (PLAN Decision 5): the projected form
// used for precomputation, and the normalization used at resolution time.
// ok=false when the record has no work unit.
func WithPlaceholderActiveTask(rec *persistence.ContextSnapshotRecord, addIfMissing bool) (*persistence.ContextSnapshotRecord, bool) {
	if rec.WorkUnitID == nil {
		return nil, false
	}
	unit := *rec.WorkUnitID
	cp := *rec
	cp.Files = nil
	found := false
	for _, f := range rec.Files {
		g := *f
		if g.TypeKey == "ACTIVE_TASK" {
			pin := precompute.PlaceholderPin(unit)
			g.Version, g.Hash, g.Content, g.StateFileID = pin.Version, pin.Hash, precompute.PlaceholderToken(unit), uuid.Nil
			found = true
		}
		cp.Files = append(cp.Files, &g)
	}
	if !found {
		if !addIfMissing {
			return nil, false
		}
		pin := precompute.PlaceholderPin(unit)
		cp.Files = append(cp.Files, &persistence.ContextSnapshotFileRecord{
			ID: uuid.New(), SnapshotID: rec.ID, TypeKey: "ACTIVE_TASK", Scope: "WORK_UNIT",
			Version: pin.Version, Hash: pin.Hash, Content: precompute.PlaceholderToken(unit),
			Classification: string(statefile.ClassifySnapshotFile("ACTIVE_TASK", statefile.ScopeWorkUnit)),
		})
	}
	return &cp, true
}

// resolvePlaceholder resolves a pending placeholder template for this exact
// context (Decision 5). It returns done=true with res filled when a worker
// prompt was produced; done=false lets the normal miss path run.
func (s *PromptCacheService) resolvePlaceholder(ctx context.Context, b *cacheInput, entry *cache.PromptCacheEntry, res *PrepareResult, start time.Time, source cache.EntrySource) (bool, error) {
	if s.templates == nil || b.precompute || b.snapshot.WorkUnitID == nil {
		return false, nil
	}
	pin := activeTaskPin(b.snapshot)
	if pin == nil || pin.Version <= 0 || compression.ContainsPlaceholder(pin.Content) {
		return false, nil
	}
	norm, ok := WithPlaceholderActiveTask(b.snapshot, false)
	if !ok {
		return false, nil
	}
	nb, err := s.inputFromResolved(ctx, b.input.ProjectID, b.composition, norm, b.mode)
	if err != nil {
		return false, nil
	}
	tmpl, err := s.templates.GetPending(ctx, b.input.ProjectID, nb.key)
	if err != nil {
		return false, nil
	}
	fail := func(status, reason string) (bool, error) {
		_, _ = s.templates.Finish(context.WithoutCancel(ctx), tmpl.ID, status, reason, nil, s.now())
		id := entry.ID
		s.emit(ctx, b, "PRECOMPUTE_TEMPLATE_REJECTED", LayerTemplate, &id, reason, nil)
		return false, nil
	}
	// The real canonical prompt, composed normally (Layer 1 reuse allowed).
	cp, err := s.canonical.FindByCacheKey(ctx, b.input.ProjectID, b.canonicalKey)
	if errors.Is(err, persistence.ErrNotFound) {
		cp, err = s.compose(ctx, b)
	}
	if err != nil {
		return fail(persistence.TemplateFailed, "compose real canonical prompt: "+err.Error())
	}
	// The real rendering of the ACTIVE_TASK slot: the real component at the
	// placeholder component's position, minus the shared prefix/suffix.
	unit := *b.snapshot.WorkUnitID
	token := precompute.PlaceholderToken(unit)
	phComps, err := s.components.ListComponents(ctx, tmpl.CanonicalPromptID)
	if err != nil {
		return fail(persistence.TemplateFailed, "template components unavailable")
	}
	realComps, err := s.components.ListComponents(ctx, cp.ID)
	if err != nil || len(realComps) != len(phComps) {
		return fail(persistence.TemplateStale, "component structure changed")
	}
	realValue, found := "", false
	for i, c := range phComps {
		k := strings.Index(c.Content, token)
		if k < 0 {
			continue
		}
		prefix, suffix := c.Content[:k], c.Content[k+len(token):]
		rc := realComps[i].Content
		if realComps[i].Type != c.Type || !strings.HasPrefix(rc, prefix) || !strings.HasSuffix(rc, suffix) || len(rc) < len(prefix)+len(suffix) {
			return fail(persistence.TemplateStale, "ACTIVE_TASK slot does not line up with the real prompt")
		}
		realValue, found = rc[len(prefix):len(rc)-len(suffix)], true
		break
	}
	if !found {
		return fail(persistence.TemplateFailed, "placeholder slot not found in the template components")
	}
	var content string
	if tmpl.ContentSource == TemplateFromCompressed && tmpl.CompressedPromptID != nil {
		c, err := s.artifacts.GetCompressedPrompt(ctx, *tmpl.CompressedPromptID)
		if err != nil {
			return fail(persistence.TemplateFailed, "template compressed prompt unavailable")
		}
		content = c.Content
	} else {
		c, err := s.canonical.Get(ctx, tmpl.CanonicalPromptID)
		if err != nil {
			return fail(persistence.TemplateFailed, "template canonical prompt unavailable")
		}
		content = c.Content
	}
	resolved, err := precompute.ReplacePlaceholder(content, unit, realValue)
	if err != nil {
		return fail(persistence.TemplateFailed, err.Error())
	}
	pr, err := s.compression.AdoptPrecomputed(ctx, b.input.ProjectID, cp.ID, resolved, tmpl.ContentSource)
	if err != nil {
		return fail(persistence.TemplateFailed, err.Error())
	}
	wp := pr.WorkerPrompt
	src := cache.SourcePrecomputed
	ok, err = s.entries.FinalizeValid(ctx, entry.ID, s.owner, s.now(), persistence.BuildResult{
		CanonicalPromptID: cp.ID, CompressedPromptID: wp.CompressedPromptID, WorkerPromptID: wp.ID,
		Metadata: b.metadata, Source: src, CompressionCalls: 0, CompressionInputTokens: 0, BuildDuration: time.Since(start),
	})
	if err != nil {
		return true, err
	}
	id := entry.ID
	_, _ = s.templates.Finish(ctx, tmpl.ID, persistence.TemplateResolved, "resolved with the real ACTIVE_TASK", &id, s.now())
	res.WorkerPrompt, res.Run, res.CanonicalPromptID = wp, pr.Run, cp.ID
	res.Layers = append(res.Layers, LayerTemplate)
	if ok {
		if stored, err := s.entries.Get(ctx, entry.ID); err == nil {
			res.Entry = stored
		}
		s.emit(ctx, b, cache.EventStore, cache.LayerWorker, &id, "placeholder template resolved", nil)
		s.emit(ctx, b, cache.EventBuildCompleted, cache.LayerWorker, &id, "", nil)
	}
	_ = source
	return true, nil
}

// recordLatency stores the measured transition latency (WBS 11.53).
func (s *PromptCacheService) recordLatency(ctx context.Context, b *cacheInput, res *PrepareResult, received time.Time) {
	if s.latencies == nil || res.WorkerPrompt == nil {
		return
	}
	ready := s.now()
	srcName := precompute.LatencyOnDemand
	switch {
	case hasLayerOf(res, LayerTemplate):
		srcName = precompute.LatencyPlaceholder
	case res.Hit && res.Entry != nil && res.Entry.Source == cache.SourcePrecomputed:
		srcName = precompute.LatencyPrecomputed
	case res.Hit:
		srcName = precompute.LatencyCacheHit
	}
	d := ready.Sub(received)
	if d < 0 {
		d = 0
	}
	wid := res.WorkerPrompt.ID
	l := &persistence.TransitionLatency{
		ID: uuid.New(), ProjectID: b.input.ProjectID, StateKey: b.input.StateKey, WorkUnitID: b.snapshot.WorkUnitID,
		WorkerPromptID: &wid, Source: srcName, ResultReceivedAt: received, PromptReadyAt: ready, Duration: d, CreatedAt: ready,
	}
	if res.Entry != nil {
		eid := res.Entry.ID
		l.CacheEntryID = &eid
	}
	_ = s.latencies.Create(context.WithoutCancel(ctx), l)
}

func hasLayerOf(res *PrepareResult, l cache.Layer) bool {
	for _, x := range res.Layers {
		if x == l {
			return true
		}
	}
	return false
}
