// Phase 14: the continuation REST surface.
//
//	GET  /api/v1/projects/{project}/continuations                    (WBS 14.81, filters + pagination)
//	GET  /api/v1/projects/{project}/continuations/metrics            (WBS 14.88)
//	GET  /api/v1/projects/{project}/continuations/{continuation}
//	GET  /api/v1/projects/{project}/continuations/{continuation}/history (WBS 14.83)
//	GET  /api/v1/projects/{project}/continuations/{continuation}/state   (WBS 14.73, derived CONTINUATION_STATE)
//	POST /api/v1/projects/{project}/continuations/{continuation}/retry   (WBS 14.84)
//	POST /api/v1/projects/{project}/continuations/{continuation}/override (WBS 14.85/14.86)
//	GET  /api/v1/projects/{project}/current-continuation             (WBS 14.82 + 14.77-14.80 read model)
//	GET  /api/v1/projects/{project}/settings/continuation
//	PUT  /api/v1/projects/{project}/settings/continuation
package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

type continuationAPI struct {
	svc *service.ContinuationService
}

// RegisterContinuationRoutes exposes the Phase 14 endpoints.
func RegisterContinuationRoutes(r *Router, svc *service.ContinuationService) error {
	api := &continuationAPI{svc: svc}
	base := "/api/v1/projects/{project}"
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodGet, base + "/continuations", api.list},
		{http.MethodGet, base + "/continuations/metrics", api.metrics},
		{http.MethodGet, base + "/continuations/{continuation}", api.get},
		{http.MethodGet, base + "/continuations/{continuation}/history", api.history},
		{http.MethodGet, base + "/continuations/{continuation}/state", api.state},
		{http.MethodPost, base + "/continuations/{continuation}/retry", api.retry},
		{http.MethodPost, base + "/continuations/{continuation}/override", api.override},
		{http.MethodGet, base + "/current-continuation", api.current},
		{http.MethodGet, base + "/settings/continuation", api.getSettings},
		{http.MethodPut, base + "/settings/continuation", api.putSettings},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func continuationError(err error) error {
	switch {
	case errors.Is(err, service.ErrContinuationNotFound):
		return NewNotFoundError("continuation")
	case errors.Is(err, continuation.ErrReconciliationAck):
		return NewConflictCode("reconciliation_required", "Delivery of the previous request is unknown; reconcile the repository and retry with ack_reconciliation=true.")
	case errors.Is(err, continuation.ErrRetryNotAllowed):
		return NewConflictCode("retry_not_allowed", "Only a RECOVERY_REQUIRED or FAILED continuation can be retried.")
	case errors.Is(err, continuation.ErrUnsafeOverride):
		return NewConflictCode("unsafe_override", "A same-session continuation is unsafe: the session is not active or the context limit/threshold is reached.")
	case errors.Is(err, continuation.ErrOverrideNotAllowed):
		return NewConflictCode("override_not_allowed", "The continuation can no longer change strategy.")
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_request", "Request is invalid.")
	case errors.Is(err, service.ErrConflict):
		return NewConflictCode("continuation_conflict", "The continuation cannot be changed in its current state.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("resource")
	}
	return NewInternalError("continuation request failed")
}

func decisionDTO(d *continuation.Decision) any {
	if d == nil {
		return nil
	}
	return map[string]any{"strategy": d.Strategy, "reason": d.Reason, "context_tokens": d.ContextTokens, "context_window": d.ContextWindow,
		"utilization": d.Utilization, "threshold": d.Threshold, "estimated": d.Estimated, "source": d.Source,
		"reason_text": continuation.ReasonText(d)}
}

// toContinuationDTO renders a continuation. The captured status is the
// worker's prior report (labelled as such; next_action advisory).
func toContinuationDTO(c *continuation.Continuation) map[string]any {
	cs := c.Captured
	return map[string]any{
		"id": c.ID.String(), "project_id": c.ProjectID.String(), "phase_id": c.PhaseID.String(), "work_unit_id": c.WorkUnitID.String(),
		"runtime_state_id": c.RuntimeStateID.String(), "runtime_state_version": c.RuntimeStateVersion,
		"source_dispatch_id": c.SourceDispatchID.String(), "source_result_id": uuidPtr(c.SourceResultID), "trigger": c.Trigger,
		"status": c.Status, "strategy": c.Strategy, "decision": decisionDTO(c.Decision), "attempt": c.Attempt,
		"captured_status": map[string]any{"summary": cs.Summary, "completed": strs(cs.Completed), "remaining": strs(cs.Remaining),
			"blocking": strs(cs.Blocking), "next_action": map[string]string{"text": cs.NextAction, "label": "worker-reported, advisory"},
			"source": "worker-reported", "unknown": c.StatusUnknown},
		"status_hash": c.StatusHash, "no_progress_streak": c.NoProgressStreak, "scope_expanded": c.ScopeExpanded,
		"source_snapshot_id": uuidPtr(c.SourceSnapshotID), "snapshot_id": uuidPtr(c.SnapshotID), "role": c.RoleKey, "state": c.StateKey,
		"session_id": uuidPtr(c.SessionID), "old_session_id": uuidPtr(c.OldSessionID), "new_session_id": uuidPtr(c.NewSessionID),
		"session_confirmed_at": timePtr(c.SessionConfirmedAt), "cwd_confirmed_at": timePtr(c.CWDConfirmedAt),
		"session_create_failures": c.SessionCreateFailures, "worker_prompt_id": uuidPtr(c.WorkerPromptID), "dispatch_id": uuidPtr(c.DispatchID),
		"recovery_reason": c.RecoveryReason, "override_strategy": c.OverrideStrategy, "retry_of_id": uuidPtr(c.RetryOfID),
		"failure_message": c.FailureMessage, "version": c.Version,
		"created_at": rfc(c.CreatedAt), "updated_at": rfc(c.UpdatedAt), "completed_at": timePtr(c.CompletedAt),
	}
}

func (a *continuationAPI) list(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := persistence.ContinuationFilter{ProjectID: ids[0]}
	if v := q.Get("status"); v != "" {
		st, err := continuation.ParseStatus(v)
		if err != nil {
			WriteError(w, r, NewValidationCode("invalid_filter", "unknown continuation status"))
			return
		}
		f.Status = st
	}
	if v := q.Get("strategy"); v != "" {
		st, err := continuation.ParseStrategy(v)
		if err != nil {
			WriteError(w, r, NewValidationCode("invalid_filter", "unknown continuation strategy"))
			return
		}
		f.Strategy = st
	}
	if f.PhaseID, err = optUUID(r, "phase"); err != nil {
		WriteError(w, r, err)
		return
	}
	if f.WorkUnitID, err = optUUID(r, "work_unit"); err != nil {
		WriteError(w, r, err)
		return
	}
	if f.Limit, f.Offset, err = paging(r); err != nil {
		WriteError(w, r, err)
		return
	}
	cs, total, err := a.svc.Continuations(r.Context(), f)
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	out := make([]map[string]any, 0, len(cs))
	for i := range cs {
		out = append(out, toContinuationDTO(&cs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"continuations": out, "count": len(out), "total": total, "offset": f.Offset})
}

func (a *continuationAPI) get(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "continuation")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := a.svc.Continuation(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	writeJSON(w, http.StatusOK, toContinuationDTO(c))
}

func (a *continuationAPI) current(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	c, v, err := a.svc.CurrentView(r.Context(), ids[0])
	if errors.Is(err, service.ErrContinuationNotFound) {
		WriteError(w, r, &appError{sentinel: ErrNotFound, code: "no_active_continuation", msg: "The project has no active continuation."})
		return
	}
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"continuation": toContinuationDTO(c), "view": v})
}

func (a *continuationAPI) history(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "continuation")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	h, err := a.svc.History(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	trs := make([]map[string]any, 0, len(h.Transitions))
	for _, t := range h.Transitions {
		var from any
		if t.From != nil {
			from = *t.From
		}
		trs = append(trs, map[string]any{"sequence": t.Sequence, "from": from, "to": t.To, "reason": t.Reason, "created_at": rfc(t.CreatedAt)})
	}
	atts := make([]map[string]any, 0, len(h.Attempts))
	for _, c := range h.Attempts {
		atts = append(atts, map[string]any{"id": c.ID.String(), "attempt": c.Attempt, "status": c.Status, "strategy": c.Strategy,
			"trigger": c.Trigger, "dispatch_id": uuidPtr(c.DispatchID), "status_hash": c.StatusHash, "created_at": rfc(c.CreatedAt)})
	}
	ds := make([]map[string]any, 0, len(h.Dispatches))
	for _, d := range h.Dispatches {
		var del any
		if d.Delivery != nil {
			del = *d.Delivery
		}
		ds = append(ds, map[string]any{"id": d.ID.String(), "session_id": d.SessionID.String(), "status": d.Status, "delivery": del,
			"error_code": d.ErrorCode, "attempt": d.Attempt, "created_at": rfc(d.CreatedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"continuation_id": h.Continuation.ID.String(), "transitions": trs, "attempts": atts, "dispatches": ds})
}

func (a *continuationAPI) state(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "continuation")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	doc, err := a.svc.State(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logical_file": "CONTINUATION_STATE", "derived": true, "content": doc})
}

func (a *continuationAPI) metrics(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	m, err := a.svc.Metrics(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	per := map[string]any{}
	for k, v := range m.PerWorkUnit {
		per[k.String()] = map[string]any{"continuations": v.Continuations, "session_resets": v.SessionResets}
	}
	writeJSON(w, http.StatusOK, map[string]any{"continuations_total": m.ContinuationsTotal, "same_session_continuations": m.SameSessionContinuations,
		"fresh_session_continuations": m.FreshSessionContinuations, "continuation_failures": m.ContinuationFailures,
		"continuation_blocks": m.ContinuationBlocks, "continuation_no_progress": m.ContinuationNoProgress,
		"continuation_recovery_required": m.ContinuationRecoveryRequired, "continuation_duration_ms_total": m.CompletedDurationTotal.Milliseconds(),
		"continuation_duration_ms_avg": m.CompletedDurationAvg.Milliseconds(), "per_work_unit": per})
}

func (a *continuationAPI) retry(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "continuation")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in struct {
		Ack bool `json:"ack_reconciliation"`
	}
	if r.ContentLength != 0 {
		if err := decodeStrictJSON(r, "invalid_retry", map[string]bool{"ack_reconciliation": true}, nil, &in); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	c, err := a.svc.Retry(r.Context(), ids[0], ids[1], in.Ack)
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toContinuationDTO(c))
}

func (a *continuationAPI) override(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "continuation")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in struct {
		Strategy string `json:"strategy"`
	}
	if err := decodeStrictJSON(r, "invalid_override", map[string]bool{"strategy": true}, []string{"strategy"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	st, err := continuation.ParseStrategy(in.Strategy)
	if err != nil {
		WriteError(w, r, NewValidationFields("invalid_override", "Override is invalid.", map[string]string{"strategy": "must be same_session or fresh_session"}))
		return
	}
	c, err := a.svc.Override(r.Context(), ids[0], ids[1], st)
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	writeJSON(w, http.StatusOK, toContinuationDTO(c))
}

type continuationSettingsDTO struct {
	UnknownContextPolicy        string `json:"unknown_context_policy"`
	EstimatedMayReset           bool   `json:"estimated_may_reset"`
	MaxContinuationsPerWorkUnit int    `json:"max_continuations_per_work_unit"`
	MaxSessionResetsPerWorkUnit int    `json:"max_session_resets_per_work_unit"`
	MaxNoProgressContinuations  int    `json:"max_no_progress_continuations"`
	SessionCreateRetries        int    `json:"session_create_retries"`
	LeaseSeconds                int    `json:"lease_seconds"`
}

var continuationSettingsFields = map[string]bool{"unknown_context_policy": true, "estimated_may_reset": true,
	"max_continuations_per_work_unit": true, "max_session_resets_per_work_unit": true, "max_no_progress_continuations": true,
	"session_create_retries": true, "lease_seconds": true}

func toContinuationSettingsDTO(s continuation.Settings) continuationSettingsDTO {
	return continuationSettingsDTO{UnknownContextPolicy: string(s.UnknownContextPolicy), EstimatedMayReset: s.EstimatedMayReset,
		MaxContinuationsPerWorkUnit: s.MaxContinuationsPerWorkUnit, MaxSessionResetsPerWorkUnit: s.MaxSessionResetsPerWorkUnit,
		MaxNoProgressContinuations: s.MaxNoProgressContinuations, SessionCreateRetries: s.SessionCreateRetries,
		LeaseSeconds: int(s.LeaseDuration / time.Second)}
}

func (a *continuationAPI) settingsResponse(w http.ResponseWriter, r *http.Request, projectID uuid.UUID, s continuation.Settings, stored bool) {
	th, err := a.svc.Threshold(r.Context(), projectID)
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": toContinuationSettingsDTO(s), "stored": stored,
		"context_threshold": th, "context_threshold_source": "project_settings.context_threshold"})
}

func (a *continuationAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, stored, err := a.svc.ContinuationSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	a.settingsResponse(w, r, ids[0], s, stored)
}

func (a *continuationAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cur, _, err := a.svc.ContinuationSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	in := toContinuationSettingsDTO(cur)
	if err := decodeStrictJSON(r, "invalid_continuation_settings", continuationSettingsFields, nil, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	st := continuation.Settings{UnknownContextPolicy: continuation.Strategy(in.UnknownContextPolicy), EstimatedMayReset: in.EstimatedMayReset,
		MaxContinuationsPerWorkUnit: in.MaxContinuationsPerWorkUnit, MaxSessionResetsPerWorkUnit: in.MaxSessionResetsPerWorkUnit,
		MaxNoProgressContinuations: in.MaxNoProgressContinuations, SessionCreateRetries: in.SessionCreateRetries,
		LeaseDuration: time.Duration(in.LeaseSeconds) * time.Second}
	if err := st.Validate(); err != nil {
		fields := map[string]string{}
		for _, line := range strings.Split(err.Error(), "\n") {
			msg := strings.TrimPrefix(line, continuation.ErrInvalidSettings.Error()+": ")
			field, rest, _ := strings.Cut(msg, " ")
			if field == "lease_duration" {
				field = "lease_seconds"
			}
			fields[field] = rest
		}
		WriteError(w, r, NewValidationFields("invalid_continuation_settings", "Continuation settings are invalid.", fields))
		return
	}
	out, err := a.svc.UpdateContinuationSettings(r.Context(), ids[0], st)
	if err != nil {
		WriteError(w, r, continuationError(err))
		return
	}
	a.settingsResponse(w, r, ids[0], out, true)
}
