// Phase 10: the prompt-cache REST surface.
//
//	POST /api/v1/projects/{project}/worker-prompts/prepare             (WBS 10.16)
//	GET  /api/v1/projects/{project}/settings/prompt-cache               (cache policy)
//	PUT  /api/v1/projects/{project}/settings/prompt-cache
//	GET  /api/v1/projects/{project}/prompt-cache                        (WBS 10.39)
//	GET  /api/v1/projects/{project}/prompt-cache/{entry}                (WBS 10.39)
//	POST /api/v1/projects/{project}/prompt-cache/explain                (WBS 10.40)
//	POST /api/v1/projects/{project}/prompt-cache/{entry}/invalidate     (WBS 10.41)
//	POST /api/v1/projects/{project}/prompt-cache/invalidate             (WBS 10.42)
//	GET  /api/v1/projects/{project}/prompt-cache/stats                  (WBS 10.64)
//	GET  /api/v1/projects/{project}/prompt-cache/activity               (WBS 10.65)
//	GET  /api/v1/projects/{project}/prompt-cache/fragments              (WBS 10.46)
//	POST /api/v1/projects/{project}/prompt-cache/integrity-scan         (WBS 10.58)
//	POST /api/v1/projects/{project}/prompt-cache/gc                     (WBS 10.36)
//
// No response carries prompt content except the prepare result's worker
// prompt; none carries secrets. Every route is project-scoped.
package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

type promptCacheAPI struct {
	svc *service.PromptCacheService
}

// RegisterPromptCacheRoutes exposes the Phase 10 endpoints.
func RegisterPromptCacheRoutes(r *Router, svc *service.PromptCacheService) error {
	api := &promptCacheAPI{svc: svc}
	base := "/api/v1/projects/{project}"
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodPost, base + "/worker-prompts/prepare", api.prepare},
		{http.MethodGet, base + "/settings/prompt-cache", api.getSettings},
		{http.MethodPut, base + "/settings/prompt-cache", api.putSettings},
		{http.MethodGet, base + "/prompt-cache", api.list},
		{http.MethodGet, base + "/prompt-cache/stats", api.stats},
		{http.MethodGet, base + "/prompt-cache/activity", api.activity},
		{http.MethodGet, base + "/prompt-cache/fragments", api.fragments},
		{http.MethodPost, base + "/prompt-cache/explain", api.explain},
		{http.MethodPost, base + "/prompt-cache/invalidate", api.invalidateProject},
		{http.MethodPost, base + "/prompt-cache/integrity-scan", api.integrityScan},
		{http.MethodPost, base + "/prompt-cache/gc", api.gc},
		{http.MethodGet, base + "/prompt-cache/{entry}", api.get},
		{http.MethodPost, base + "/prompt-cache/{entry}/invalidate", api.invalidate},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func promptCacheError(err error) error {
	switch {
	case errors.Is(err, service.ErrCacheSuppressed):
		return NewConflictCode("prompt_preparation_suppressed", "A recent transient failure for this prompt is inside its retry window.")
	case errors.Is(err, service.ErrCacheUnavailable):
		return NewConflictCode("prompt_cache_unavailable", "The prompt cache is unavailable and bypass is disabled.")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("cache entry is not in a state that allows this operation")
	}
	return compressionServiceError(err)
}

// --- prepare / explain ------------------------------------------------------

type continuationDTO struct {
	WorkUnitID string   `json:"work_unit_id"`
	Summary    string   `json:"summary"`
	Completed  []string `json:"completed"`
	Remaining  []string `json:"remaining"`
	Blocking   []string `json:"blocking"`
	NextAction string   `json:"next_action"`
}

type prepareInput struct {
	SnapshotID       string           `json:"snapshot_id"`
	TemplateID       string           `json:"template_id"`
	TemplateVersion  int              `json:"template_version"`
	Continuation     *continuationDTO `json:"continuation"`
	ContinuationMode string           `json:"continuation_mode"`
	Source           string           `json:"source"`
}

var prepareFields = map[string]bool{
	"snapshot_id": true, "template_id": true, "template_version": true, "continuation": true, "continuation_mode": true, "source": true,
}

func decodePrepare(r *http.Request) (service.PrepareRequest, error) {
	var in prepareInput
	if err := decodeStrictJSON(r, "invalid_prepare_request", prepareFields, []string{"snapshot_id", "template_id", "template_version"}, &in); err != nil {
		return service.PrepareRequest{}, err
	}
	bad := map[string]string{}
	snap, err := uuid.Parse(in.SnapshotID)
	if err != nil {
		bad["snapshot_id"] = "must be a UUID"
	}
	tmpl, err := uuid.Parse(in.TemplateID)
	if err != nil {
		bad["template_id"] = "must be a UUID"
	}
	if in.TemplateVersion <= 0 {
		bad["template_version"] = "must be positive"
	}
	switch in.ContinuationMode {
	case "", service.ContinuationSameSession, service.ContinuationResetSession:
	default:
		bad["continuation_mode"] = "must be SAME_SESSION or RESET_SESSION"
	}
	src := cache.EntrySource(in.Source)
	if in.Source != "" && !src.Known() {
		bad["source"] = "must be ON_DEMAND or PRECOMPUTED"
	}
	req := service.PrepareRequest{SnapshotID: snap, TemplateID: tmpl, TemplateVersion: in.TemplateVersion, ContinuationMode: in.ContinuationMode, Source: src}
	if c := in.Continuation; c != nil {
		u, err := uuid.Parse(c.WorkUnitID)
		if err != nil {
			bad["continuation.work_unit_id"] = "must be a UUID"
		}
		req.Continuation = &prompt.ContinuationStatusInput{WorkUnitID: u, Summary: c.Summary, Completed: c.Completed,
			Remaining: c.Remaining, Blocking: c.Blocking, NextAction: c.NextAction}
	}
	if len(bad) > 0 {
		return service.PrepareRequest{}, NewValidationFields("invalid_prepare_request", "Prepare request is invalid.", bad)
	}
	return req, nil
}

func (a *promptCacheAPI) prepare(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	req, err := decodePrepare(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.svc.PrepareForContext(r.Context(), ids[0], req)
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	layers := make([]string, 0, len(res.Layers))
	for _, l := range res.Layers {
		layers = append(layers, string(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hit":                 res.Hit,
		"layers":              layers,
		"cache_key":           res.CacheKey,
		"canonical_cache_key": res.CanonicalKey,
		"canonical_prompt_id": nilUUID(res.CanonicalPromptID),
		"entry":               toCacheEntryDTO(res.Entry),
		"worker_prompt":       toWorkerDTO(res.WorkerPrompt),
		"run":                 toRunDTO(res.Run),
		"bypassed":            res.Bypassed,
		"bypass_reason":       res.BypassReason,
	})
}

func nilUUID(id uuid.UUID) *string {
	if id == uuid.Nil {
		return nil
	}
	s := id.String()
	return &s
}

func (a *promptCacheAPI) explain(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	req, err := decodePrepare(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ex, err := a.svc.Explain(r.Context(), ids[0], req)
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	changed := ex.Changed
	if changed == nil {
		changed = []cache.Change{}
	}
	unchanged := ex.Unchanged
	if unchanged == nil {
		unchanged = []string{}
	}
	result := "MISS"
	if ex.Hit {
		result = "HIT"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result": result, "cache_key": ex.CacheKey, "entry_id": uuidPtr(ex.EntryID), "reason": ex.Reason,
		"reference": toCacheEntryDTO(ex.Reference), "changed": changed, "unchanged": unchanged,
	})
}

// --- entries ---------------------------------------------------------------

type cacheEntryDTO struct {
	ID                 string                  `json:"id"`
	CacheKey           string                  `json:"cache_key"`
	Layer              string                  `json:"layer"`
	Status             string                  `json:"status"`
	Source             string                  `json:"source"`
	StateKey           string                  `json:"state_key"`
	PhaseID            *string                 `json:"phase_id"`
	WorkUnitID         *string                 `json:"work_unit_id"`
	CanonicalPromptID  *string                 `json:"canonical_prompt_id"`
	CompressedPromptID *string                 `json:"compressed_prompt_id"`
	WorkerPromptID     *string                 `json:"worker_prompt_id"`
	HitCount           uint64                  `json:"hit_count"`
	InvalidationReason string                  `json:"invalidation_reason,omitempty"`
	ErrorKind          string                  `json:"error_kind,omitempty"`
	ErrorMessage       string                  `json:"error_message,omitempty"`
	RetryAfter         *string                 `json:"retry_after,omitempty"`
	BuildOwner         string                  `json:"build_owner,omitempty"`
	LeaseExpiresAt     *string                 `json:"lease_expires_at,omitempty"`
	Metadata           cache.KeySourceMetadata `json:"metadata"`
	CompressionCalls   int                     `json:"compression_calls"`
	CompressionTokens  int                     `json:"compression_input_tokens"`
	BuildDurationMS    int64                   `json:"build_duration_ms"`
	CreatedAt          string                  `json:"created_at"`
	LastUsedAt         string                  `json:"last_used_at"`
}

func timePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func toCacheEntryDTO(e *cache.PromptCacheEntry) *cacheEntryDTO {
	if e == nil {
		return nil
	}
	return &cacheEntryDTO{
		ID: e.ID.String(), CacheKey: e.CacheKey, Layer: string(e.Layer), Status: string(e.Status), Source: string(e.Source),
		StateKey: e.StateKey, PhaseID: uuidPtr(e.PhaseID), WorkUnitID: uuidPtr(e.WorkUnitID),
		CanonicalPromptID: uuidPtr(e.CanonicalPromptID), CompressedPromptID: uuidPtr(e.CompressedPromptID), WorkerPromptID: uuidPtr(e.WorkerPromptID),
		HitCount: e.HitCount, InvalidationReason: e.InvalidationReason, ErrorKind: e.ErrorKind, ErrorMessage: e.ErrorMessage,
		RetryAfter: timePtr(e.RetryAfter), BuildOwner: e.BuildOwner, LeaseExpiresAt: timePtr(e.LeaseExpiresAt), Metadata: e.Metadata,
		CompressionCalls: e.CompressionCalls, CompressionTokens: e.CompressionInputTokens, BuildDurationMS: e.BuildDuration.Milliseconds(),
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano), LastUsedAt: e.LastUsedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (a *promptCacheAPI) list(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	f := persistence.PromptCacheFilter{ProjectID: ids[0]}
	q := r.URL.Query()
	bad := map[string]string{}
	if v := q.Get("state"); v != "" {
		f.StateKey = &v
	}
	for _, p := range []struct {
		name string
		dst  **uuid.UUID
	}{{"phase", &f.PhaseID}, {"work_unit", &f.WorkUnitID}} {
		if v := q.Get(p.name); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				bad[p.name] = "must be a UUID"
				continue
			}
			*p.dst = &id
		}
	}
	if v := q.Get("status"); v != "" {
		st := cache.Status(v)
		if !st.Known() {
			bad["status"] = "unknown status"
		}
		f.Status = &st
	}
	if v := q.Get("source"); v != "" {
		src := cache.EntrySource(v)
		if !src.Known() {
			bad["source"] = "unknown source"
		}
		f.Source = &src
	}
	if v := q.Get("layer"); v != "" {
		l := cache.Layer(v)
		f.Layer = &l
	}
	if len(bad) > 0 {
		WriteError(w, r, NewValidationFields("invalid_filter", "Cache filter is invalid.", bad))
		return
	}
	entries, err := a.svc.ListEntries(r.Context(), f)
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	out := make([]*cacheEntryDTO, 0, len(entries))
	for i := range entries {
		out = append(out, toCacheEntryDTO(&entries[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "count": len(out)})
}

func (a *promptCacheAPI) get(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "entry")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	e, err := a.svc.GetEntry(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	writeJSON(w, http.StatusOK, toCacheEntryDTO(e))
}

type reasonInput struct {
	Reason string `json:"reason"`
}

func (a *promptCacheAPI) invalidate(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "entry")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in reasonInput
	if err := decodeStrictOptionalJSON(r, "invalid_invalidation", map[string]bool{"reason": true}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	e, err := a.svc.Invalidate(r.Context(), ids[0], ids[1], in.Reason)
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	writeJSON(w, http.StatusOK, toCacheEntryDTO(e))
}

func (a *promptCacheAPI) invalidateProject(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in reasonInput
	if err := decodeStrictOptionalJSON(r, "invalid_invalidation", map[string]bool{"reason": true}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	n, err := a.svc.InvalidateProject(r.Context(), ids[0], in.Reason)
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invalidated": n})
}

// --- maintenance / stats ----------------------------------------------------

func (a *promptCacheAPI) integrityScan(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rep, err := a.svc.IntegrityScan(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	corrupt := make([]map[string]string, 0, len(rep.Corrupt))
	for _, c := range rep.Corrupt {
		corrupt = append(corrupt, map[string]string{"entry_id": c.EntryID.String(), "reason": c.Reason})
	}
	writeJSON(w, http.StatusOK, map[string]any{"scanned": rep.Scanned, "healthy": rep.Healthy, "corrupt": corrupt})
}

func (a *promptCacheAPI) gc(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	n, err := a.svc.GarbageCollect(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

func (a *promptCacheAPI) stats(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.svc.Stats(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": s.Entries, "valid": s.Valid, "invalidated": s.Invalidated, "failed": s.Failed, "building": s.Building,
		"lookups": s.Lookups, "hits": s.Hits, "misses": s.Misses, "hit_rate": s.HitRate,
		"canonical_hits": s.CanonicalHits, "compression_hits": s.CompressionHits, "worker_prompt_hits": s.WorkerHits,
		"fragment_hits": s.FragmentHits, "bypasses": s.Bypasses, "suppressions": s.Suppressions,
		"compression_calls_avoided":    s.CompressionCallsAvoided,
		"estimated_tokens_avoided":     s.EstimatedTokensAvoided,
		"estimated_latency_avoided_ms": s.EstimatedLatencyAvoided.Milliseconds(),
	})
}

func (a *promptCacheAPI) activity(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := a.svc.Activity(r.Context(), ids[0], limit)
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"event": it.Event, "layer": it.Layer, "state_key": it.StateKey, "work_unit_id": uuidPtr(it.WorkUnitID),
			"reason": it.Reason, "entry_id": uuidPtr(it.EntryID), "at": it.At,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity": out, "count": len(out)})
}

func (a *promptCacheAPI) fragments(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	fs, err := a.svc.Fragments(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	out := make([]map[string]any, 0, len(fs))
	for _, f := range fs {
		out = append(out, map[string]any{"id": f.ID.String(), "fragment_key": f.FragmentKey, "kind": f.Kind, "hash": f.Hash,
			"estimated_tokens": f.EstimatedTokens, "sources": f.Sources, "created_at": f.CreatedAt.UTC().Format(time.RFC3339Nano)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"fragments": out, "count": len(out)})
}

// --- settings -----------------------------------------------------------------

type cacheSettingsDTO struct {
	Enabled                      bool   `json:"enabled"`
	RetentionPolicy              string `json:"retention_policy"`
	RetentionValue               int    `json:"retention_value"`
	NegativeWindowMS             int64  `json:"negative_window_ms"`
	LeaseMS                      int64  `json:"lease_ms"`
	CacheSameSessionContinuation bool   `json:"cache_same_session_continuation"`
	BypassOnCacheError           bool   `json:"bypass_on_cache_error"`
	Stored                       bool   `json:"stored"`
}

func toCacheSettingsDTO(s cache.Settings, stored bool) cacheSettingsDTO {
	return cacheSettingsDTO{Enabled: s.Enabled, RetentionPolicy: string(s.Retention.Policy), RetentionValue: s.Retention.Value,
		NegativeWindowMS: s.NegativeWindow.Milliseconds(), LeaseMS: s.Lease.Milliseconds(),
		CacheSameSessionContinuation: s.CacheSameSessionContinuation, BypassOnCacheError: s.BypassOnCacheError, Stored: stored}
}

func (a *promptCacheAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, stored, err := a.svc.CacheSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	writeJSON(w, http.StatusOK, toCacheSettingsDTO(s, stored))
}

var cacheSettingsFields = map[string]bool{
	"enabled": true, "retention_policy": true, "retention_value": true, "negative_window_ms": true, "lease_ms": true,
	"cache_same_session_continuation": true, "bypass_on_cache_error": true,
}

func (a *promptCacheAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cur, _, err := a.svc.CacheSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, promptCacheError(err))
		return
	}
	in := toCacheSettingsDTO(cur, true)
	if err := decodeStrictJSON(r, "invalid_prompt_cache_settings", cacheSettingsFields, nil, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	st := cache.Settings{Enabled: in.Enabled, Retention: cache.Retention{Policy: cache.RetentionPolicy(in.RetentionPolicy), Value: in.RetentionValue},
		NegativeWindow: time.Duration(in.NegativeWindowMS) * time.Millisecond, Lease: time.Duration(in.LeaseMS) * time.Millisecond,
		CacheSameSessionContinuation: in.CacheSameSessionContinuation, BypassOnCacheError: in.BypassOnCacheError}
	out, err := a.svc.UpdateCacheSettings(r.Context(), ids[0], st)
	if err != nil {
		if errors.Is(err, service.ErrInvalidInput) {
			WriteError(w, r, NewValidationCode("invalid_prompt_cache_settings", err.Error()))
			return
		}
		WriteError(w, r, promptCacheError(err))
		return
	}
	writeJSON(w, http.StatusOK, toCacheSettingsDTO(out, true))
}
