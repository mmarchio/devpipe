package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/compression/compressiontest"
	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// Phase 10 integration rig: the Phase 9 implementation position plus a
// prompt-cache service over the harness pool whose compression provider is
// a test double (scripted mock counting calls, or the providertest fake).

// p10Service builds composer + compression + prompt-cache services.
func p10Service(h *Harness, src service.CompressorSource) (*service.PromptCacheService, *service.CompressionService) {
	raw := h.App.DB().Raw()
	comp := p9Service(h, src)
	composer := service.NewPromptComposerService(
		dbrepo.NewContextSnapshotRepository(raw),
		dbrepo.NewPromptTemplateVersionRepository(raw),
		dbrepo.NewWorkflowStateRepository(raw),
		dbrepo.NewResponseProtocolRepository(raw),
		dbrepo.NewStateInstructionRepository(raw),
		dbrepo.NewPromptTemplateRepository(raw),
		dbrepo.NewWorkUnitRepository(raw),
		dbrepo.NewPlanRepository(raw),
		dbrepo.NewCanonicalPromptRepository(raw),
	).WithTxPool(raw).
		WithWBSVersions(dbrepo.NewWBSVersionRepository(raw)).
		WithSettings(dbrepo.NewSettingsRepository(raw)).
		WithComponents(dbrepo.NewCanonicalPromptComponentRepository(raw))
	cacheSvc := service.NewPromptCacheService(composer, comp,
		dbrepo.NewSettingsRepository(raw),
		dbrepo.NewPromptCacheRepository(raw),
		dbrepo.NewPromptCacheEventRepository(raw),
		dbrepo.NewPromptFragmentRepository(raw),
		dbrepo.NewPromptCacheSettingsRepository(raw),
		dbrepo.NewCanonicalPromptRepository(raw),
		dbrepo.NewCanonicalPromptComponentRepository(raw),
		dbrepo.NewCompressionRepository(raw),
	)
	return cacheSvc, comp
}

// p10Env is a ready implementation position with a cache service whose
// compressor is a call-counting heuristic.
type p10Env struct {
	h     *Harness
	pos   p9Position
	cache *service.PromptCacheService
	comp  *service.CompressionService
	sc    *compressiontest.Scripted
}

// p10Setup builds the position (default spec) with compression target
// 4,000 tokens in a 64,000-token context (the default canonical estimate is
// ~10,000 tokens, so preparation compresses).
func p10Setup(t *testing.T, h *Harness, spec p9Spec) *p10Env {
	t.Helper()
	pos := p9Rig(t, h, spec)
	sc := compressiontest.NewScripted()
	cacheSvc, comp := p10Service(h, p9Source{compressor: sc})
	p9Configure(t, h, comp, pos.ProjectID, 64000, p9Settings(4000, 2))
	return &p10Env{h: h, pos: pos, cache: cacheSvc, comp: comp, sc: sc}
}

func (e *p10Env) req(snap uuid.UUID) service.PrepareRequest {
	return service.PrepareRequest{SnapshotID: snap, TemplateID: e.pos.TemplateID, TemplateVersion: 1}
}

func (e *p10Env) prepare(t *testing.T, req service.PrepareRequest) *service.PrepareResult {
	t.Helper()
	res, err := e.cache.PrepareForContext(context.Background(), e.pos.ProjectID, req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if res.WorkerPrompt == nil {
		t.Fatalf("prepare returned no worker prompt: %+v", res)
	}
	return res
}

// snapshot creates a fresh execution snapshot of the project's position.
func (e *p10Env) snapshot(t *testing.T) uuid.UUID {
	t.Helper()
	snap, err := p718Svc(t, e.h).CreateExecutionSnapshot(context.Background(), e.pos.ProjectID.String())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return snap.ID
}

// writeFile appends a new version of one of the position's state files.
func (e *p10Env) writeFile(t *testing.T, typeKey, content string) {
	t.Helper()
	ctx := context.Background()
	id := e.pos.Files[typeKey]
	var cur int
	if err := e.h.DBScan(ctx, "SELECT current_version FROM state_files WHERE id = '"+id.String()+"'", &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := p708Service(t, e.h).UpdateContent(ctx, id.String(), content, cur); err != nil {
		t.Fatalf("write %s: %v", typeKey, err)
	}
}

func (e *p10Env) count(t *testing.T, table, where string) int {
	t.Helper()
	return p9Count(t, e.h, table, "project_id = '"+e.pos.ProjectID.String()+"'"+where)
}

func (e *p10Env) events(t *testing.T, typ string) int {
	t.Helper()
	return e.count(t, "prompt_cache_events", " AND event_type = '"+typ+"'")
}

func hasLayer(res *service.PrepareResult, l string) bool {
	for _, x := range res.Layers {
		if string(x) == l {
			return true
		}
	}
	return false
}
