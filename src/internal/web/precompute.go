// Phase 11: the precomputation REST surface.
//
//	GET  /api/v1/projects/{project}/settings/precompute          (WBS 11.3)
//	PUT  /api/v1/projects/{project}/settings/precompute
//	POST /api/v1/projects/{project}/precompute                   (WBS 11.55, manual job; ?wait=true runs synchronously)
//	GET  /api/v1/projects/{project}/precompute/jobs              (WBS 11.54, ?limit&offset)
//	GET  /api/v1/projects/{project}/precompute/jobs/{job}        (WBS 11.54, with candidate records)
//	GET  /api/v1/projects/{project}/precompute/candidates        (WBS 11.56, non-executing preview)
//	GET  /api/v1/projects/{project}/precompute/summary           (WBS 11.57, read model)
//	GET  /api/v1/projects/{project}/precompute/metrics           (WBS 11.51/11.52/11.53/11.77)
//	POST /api/v1/projects/{project}/worker-prompts/prepare-next  (WBS 11.62-11.66: prepare the current position)
package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"overseer/internal/precompute"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

type precomputeAPI struct {
	svc *service.PrecomputeService
}

// RegisterPrecomputeRoutes exposes the Phase 11 endpoints.
func RegisterPrecomputeRoutes(r *Router, svc *service.PrecomputeService) error {
	api := &precomputeAPI{svc: svc}
	base := "/api/v1/projects/{project}"
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodGet, base + "/settings/precompute", api.getSettings},
		{http.MethodPut, base + "/settings/precompute", api.putSettings},
		{http.MethodPost, base + "/precompute", api.manual},
		{http.MethodGet, base + "/precompute/jobs", api.jobs},
		{http.MethodGet, base + "/precompute/jobs/{job}", api.job},
		{http.MethodGet, base + "/precompute/candidates", api.candidates},
		{http.MethodGet, base + "/precompute/summary", api.summary},
		{http.MethodGet, base + "/precompute/metrics", api.metrics},
		{http.MethodPost, base + "/worker-prompts/prepare-next", api.prepareNext},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func precomputeError(err error) error {
	var se *precompute.SettingsError
	switch {
	case errors.As(err, &se):
		return NewValidationFields("invalid_precompute_settings", "Precompute settings are invalid.", se.Fields)
	case errors.Is(err, service.ErrPrecomputeDisabled):
		return NewConflictCode("precompute_disabled", "Precomputation is disabled for this project.")
	case errors.Is(err, service.ErrProjectNotActive):
		return NewConflictCode("project_not_active", "The project is paused, cancelled or finished.")
	}
	return promptCacheError(err)
}

// --- settings ----------------------------------------------------------------

type precomputeSettingsDTO struct {
	Enabled                bool   `json:"enabled"`
	Depth                  int    `json:"depth"`
	MaxCandidates          int    `json:"max_candidates"`
	Concurrency            int    `json:"concurrency"`
	CompressionEnabled     bool   `json:"compression_enabled"`
	StaticFragments        bool   `json:"static_fragments"`
	DuringWorkerExecution  bool   `json:"during_worker_execution"`
	SameSessionCompression bool   `json:"same_session_compression"`
	PlanNextPhase          bool   `json:"plan_next_phase"`
	MaxTokens              int    `json:"max_tokens"`
	MaxDurationMS          int64  `json:"max_duration_ms"`
	ResourcePolicy         string `json:"resource_policy"`
	WorkerIsLocal          bool   `json:"worker_is_local"`
	Stored                 bool   `json:"stored"`
}

func toPrecomputeSettingsDTO(s precompute.Settings, stored bool) precomputeSettingsDTO {
	return precomputeSettingsDTO{Enabled: s.Enabled, Depth: s.Depth, MaxCandidates: s.MaxCandidates, Concurrency: s.Concurrency,
		CompressionEnabled: s.CompressionEnabled, StaticFragments: s.StaticFragments, DuringWorkerExecution: s.DuringWorkerExecution,
		SameSessionCompression: s.SameSessionCompression, PlanNextPhase: s.PlanNextPhase, MaxTokens: s.MaxTokens,
		MaxDurationMS: s.MaxDuration.Milliseconds(), ResourcePolicy: string(s.ResourcePolicy), WorkerIsLocal: s.WorkerIsLocal, Stored: stored}
}

var precomputeSettingsFields = map[string]bool{
	"enabled": true, "depth": true, "max_candidates": true, "concurrency": true, "compression_enabled": true, "static_fragments": true,
	"during_worker_execution": true, "same_session_compression": true, "plan_next_phase": true, "max_tokens": true,
	"max_duration_ms": true, "resource_policy": true, "worker_is_local": true,
}

func (a *precomputeAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, stored, err := a.svc.PrecomputeSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	writeJSON(w, http.StatusOK, toPrecomputeSettingsDTO(s, stored))
}

func (a *precomputeAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cur, _, err := a.svc.PrecomputeSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	in := toPrecomputeSettingsDTO(cur, true)
	if err := decodeStrictJSON(r, "invalid_precompute_settings", precomputeSettingsFields, nil, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	st := precompute.Settings{Enabled: in.Enabled, Depth: in.Depth, MaxCandidates: in.MaxCandidates, Concurrency: in.Concurrency,
		CompressionEnabled: in.CompressionEnabled, StaticFragments: in.StaticFragments, DuringWorkerExecution: in.DuringWorkerExecution,
		SameSessionCompression: in.SameSessionCompression, PlanNextPhase: in.PlanNextPhase, MaxTokens: in.MaxTokens,
		MaxDuration: time.Duration(in.MaxDurationMS) * time.Millisecond, ResourcePolicy: precompute.ResourcePolicy(in.ResourcePolicy),
		WorkerIsLocal: in.WorkerIsLocal}
	out, err := a.svc.UpdatePrecomputeSettings(r.Context(), ids[0], st)
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	writeJSON(w, http.StatusOK, toPrecomputeSettingsDTO(out, true))
}

// --- jobs ----------------------------------------------------------------------

func toJobDTO(j *precompute.Job) map[string]any {
	return map[string]any{
		"id": j.ID.String(), "trigger": j.Trigger, "status": j.Status, "owner": j.Owner,
		"base_snapshot_id": uuidPtr(j.BaseSnapshotID), "state_key": j.StateKey, "work_unit_id": uuidPtr(j.WorkUnitID),
		"depth": j.Depth, "max_candidates": j.MaxCandidates, "candidate_count": j.CandidateCount, "existing_hits": j.HitCount,
		"full_prompts": j.BuiltCount, "fragments": j.FragmentCount, "templates": j.TemplateCount, "skipped": j.SkippedCount,
		"failed": j.FailedCount, "reason": j.Reason, "created_at": j.CreatedAt.UTC().Format(time.RFC3339Nano),
		"started_at": timePtr(j.StartedAt), "completed_at": timePtr(j.CompletedAt),
	}
}

func toCandidateRecordDTO(c precompute.CandidateRecord) map[string]any {
	frags := make([]string, 0, len(c.FragmentIDs))
	for _, f := range c.FragmentIDs {
		frags = append(frags, f.String())
	}
	missing := c.Missing
	if missing == nil {
		missing = []precompute.DynamicInputType{}
	}
	return map[string]any{
		"id": c.ID.String(), "state_key": c.StateKey, "role_key": c.RoleKey, "phase_id": uuidPtr(c.PhaseID), "work_unit_id": uuidPtr(c.WorkUnitID),
		"paths": c.Paths, "depth": c.Depth, "next_unit": c.NextUnit, "readiness": c.Readiness, "priority": c.Priority,
		"missing_dependencies": missing, "result": c.Result, "reason": c.Reason, "cache_key": c.CacheKey,
		"cache_entry_id": uuidPtr(c.CacheEntryID), "fragment_ids": frags, "template_id": uuidPtr(c.TemplateID),
		"projection_id": uuidPtr(c.ProjectionID),
	}
}

func (a *precomputeAPI) manual(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in struct {
		MaxDepth      int    `json:"max_depth"`
		MaxCandidates int    `json:"max_candidates"`
		BaseSnapshot  string `json:"base_snapshot_id"`
	}
	if err := decodeStrictOptionalJSON(r, "invalid_precompute_request", map[string]bool{"max_depth": true, "max_candidates": true, "base_snapshot_id": true}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		rt, err := a.svc.RuntimeID(r.Context(), ids[0])
		if err != nil {
			WriteError(w, r, precomputeError(err))
			return
		}
		req := precompute.Request{ProjectID: ids[0], RuntimeStateID: rt, Trigger: precompute.TriggerManual, MaxDepth: in.MaxDepth, MaxCandidates: in.MaxCandidates}
		if in.BaseSnapshot != "" {
			b, err := uuid.Parse(in.BaseSnapshot)
			if err != nil {
				WriteError(w, r, NewValidationFields("invalid_precompute_request", "Invalid base snapshot.", map[string]string{"base_snapshot_id": "must be a UUID"}))
				return
			}
			req.BaseSnapshotID = &b
		}
		res, err := a.svc.WarmUpcomingPrompts(r.Context(), req)
		if err != nil {
			WriteError(w, r, precomputeError(err))
			return
		}
		j, _, err := a.svc.GetJob(r.Context(), ids[0], res.JobID)
		if err != nil {
			WriteError(w, r, precomputeError(err))
			return
		}
		writeJSON(w, http.StatusOK, toJobDTO(j))
		return
	}
	j, err := a.svc.Trigger(r.Context(), ids[0], precompute.TriggerManual)
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	writeJSON(w, http.StatusAccepted, toJobDTO(j))
}

func (a *precomputeAPI) jobs(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	jobs, total, err := a.svc.ListJobs(r.Context(), ids[0], limit, offset)
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	out := make([]map[string]any, 0, len(jobs))
	for i := range jobs {
		out = append(out, toJobDTO(&jobs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "count": len(out), "total": total, "offset": offset})
}

func (a *precomputeAPI) job(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "job")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	j, recs, err := a.svc.GetJob(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	out := toJobDTO(j)
	cands := make([]map[string]any, 0, len(recs))
	for _, c := range recs {
		cands = append(cands, toCandidateRecordDTO(c))
	}
	out["candidates"] = cands
	writeJSON(w, http.StatusOK, out)
}

func candidateViews(cs []service.CandidateView) []map[string]any {
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		missing := c.Missing
		if missing == nil {
			missing = []precompute.DynamicInputType{}
		}
		status := "READY"
		switch c.Readiness {
		case precompute.PartiallyKnown:
			status = "awaiting " + joinInputs(missing)
		case precompute.Unknown:
			status = "not preparable: " + c.Reason
		}
		out = append(out, map[string]any{
			"state_key": c.StateKey, "role_key": c.RoleKey, "work_unit_id": uuidPtr(c.WorkUnitID), "phase_id": uuidPtr(c.PhaseID),
			"readiness": c.Readiness, "priority": c.Priority, "depth": c.Depth, "next_unit": c.NextUnit, "paths": c.Paths,
			"missing_dependencies": missing, "reason": c.Reason, "template": c.Template, "status": status,
		})
	}
	return out
}

func joinInputs(in []precompute.DynamicInputType) string {
	s := ""
	for i, x := range in {
		if i > 0 {
			s += ", "
		}
		s += string(x)
	}
	return s
}

func (a *precomputeAPI) candidates(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cs, err := a.svc.PreviewCandidates(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": candidateViews(cs), "count": len(cs)})
}

func (a *precomputeAPI) summary(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.svc.ReadModel(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	var latest map[string]any
	if s.Latest != nil {
		latest = toJobDTO(s.Latest)
	}
	writeJSON(w, http.StatusOK, map[string]any{"current_job": latest, "candidates": candidateViews(s.Candidates)})
}

func (a *precomputeAPI) metrics(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	m, err := a.svc.Metrics(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	byDepth := map[string]precompute.Ratio{}
	for k, v := range m.ByDepth {
		byDepth[strconv.Itoa(k)] = v
	}
	byPrio := map[string]precompute.Ratio{}
	for k, v := range m.ByPriority {
		byPrio[strconv.Itoa(k)] = v
	}
	lat := make([]map[string]any, 0, len(m.Latency))
	for _, l := range m.Latency {
		lat = append(lat, map[string]any{"source": l.Source, "count": l.Count, "average_ms": l.Average.Milliseconds(), "max_ms": l.Max.Milliseconds()})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": m.Jobs, "candidates": m.Candidates, "full_prompts": m.FullPrompts, "fragments": m.Fragments, "templates": m.Templates,
		"existing_hits": m.Hits, "failed": m.Failed, "skipped": m.Skipped, "utilization": m.Overall,
		"utilization_by_depth": byDepth, "utilization_by_priority": byPrio, "transition_latency": lat,
	})
}

// --- prepare-next ------------------------------------------------------------

func (a *precomputeAPI) prepareNext(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in struct {
		ResultReceivedAt string           `json:"result_received_at"`
		Continuation     *continuationDTO `json:"continuation"`
		ContinuationMode string           `json:"continuation_mode"`
	}
	if err := decodeStrictOptionalJSON(r, "invalid_prepare_request", map[string]bool{"result_received_at": true, "continuation": true, "continuation_mode": true}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	req := service.NextRequest{ContinuationMode: in.ContinuationMode}
	if in.ResultReceivedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, in.ResultReceivedAt)
		if err != nil {
			WriteError(w, r, NewValidationFields("invalid_prepare_request", "Invalid timestamp.", map[string]string{"result_received_at": "must be RFC3339"}))
			return
		}
		req.ResultReceivedAt = &t
	}
	if c := in.Continuation; c != nil {
		u, err := uuid.Parse(c.WorkUnitID)
		if err != nil {
			WriteError(w, r, NewValidationFields("invalid_prepare_request", "Invalid continuation.", map[string]string{"continuation.work_unit_id": "must be a UUID"}))
			return
		}
		req.Continuation = &prompt.ContinuationStatusInput{WorkUnitID: u, Summary: c.Summary, Completed: c.Completed,
			Remaining: c.Remaining, Blocking: c.Blocking, NextAction: c.NextAction}
	}
	res, err := a.svc.PrepareNext(r.Context(), ids[0], req)
	if err != nil {
		WriteError(w, r, precomputeError(err))
		return
	}
	layers := make([]string, 0, len(res.Layers))
	for _, l := range res.Layers {
		layers = append(layers, string(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hit": res.Hit, "layers": layers, "cache_key": res.CacheKey, "entry": toCacheEntryDTO(res.Entry),
		"worker_prompt": toWorkerDTO(res.WorkerPrompt), "run": toRunDTO(res.Run), "bypassed": res.Bypassed,
	})
}
