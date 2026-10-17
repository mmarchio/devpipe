// Phase 12: the worker REST surface.
//
//	GET  /api/v1/projects/{project}/worker                          (WBS 12.81, status + context; 12.86 read model)
//	GET  /api/v1/projects/{project}/worker/sessions                 (WBS 12.82, chronological history)
//	GET  /api/v1/projects/{project}/worker/sessions/{session}
//	GET  /api/v1/projects/{project}/worker/dispatches               (WBS 12.83, filters + pagination)
//	GET  /api/v1/projects/{project}/worker/dispatches/{dispatch}    (attempt history; never prompt content)
//	POST /api/v1/projects/{project}/worker/session/new              (WBS 12.84, creation only)
//	PUT  /api/v1/projects/{project}/worker/session/cwd              (WBS 12.85)
//	GET  /api/v1/projects/{project}/worker/metrics                  (WBS 12.54)
package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/worker"
)

type workerAPI struct {
	svc *service.WorkerService
}

// RegisterWorkerRoutes exposes the Phase 12 endpoints.
func RegisterWorkerRoutes(r *Router, svc *service.WorkerService) error {
	api := &workerAPI{svc: svc}
	base := "/api/v1/projects/{project}/worker"
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodGet, base, api.status},
		{http.MethodGet, base + "/sessions", api.sessions},
		{http.MethodGet, base + "/sessions/{session}", api.session},
		{http.MethodGet, base + "/dispatches", api.dispatches},
		{http.MethodGet, base + "/dispatches/{dispatch}", api.dispatch},
		{http.MethodPost, base + "/session/new", api.newSession},
		{http.MethodPut, base + "/session/cwd", api.cwd},
		{http.MethodGet, base + "/metrics", api.metrics},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

// workerError maps service and worker errors to API errors. Messages are
// the sanitized worker messages; diagnostics never leave the server.
func workerError(err error) error {
	var we *worker.WorkerError
	switch {
	case errors.As(err, &we):
		code := strings.ToLower(string(we.Code))
		if !strings.HasPrefix(code, "worker_") {
			code = "worker_" + code
		}
		switch we.Code {
		case worker.CodeInvalidRequest:
			return NewValidationCode(code, we.Message)
		case worker.CodeSessionLost, worker.CodeBusy, worker.CodeNoSession, worker.CodeClosed, worker.CodeContextLimit, worker.CodeCancelled:
			return NewConflictCode(code, we.Message)
		}
		return NewBadGatewayCode(code, we.Message)
	case errors.Is(err, service.ErrWorkerNotConfigured):
		return NewConflictCode("worker_not_configured", "The project has no configured worker provider.")
	case errors.Is(err, service.ErrNoActiveSession):
		return NewConflictCode("no_active_session", "The project has no active worker session.")
	case errors.Is(err, service.ErrWorkingDirectoryNotReady):
		return NewConflictCode("working_directory_not_ready", "The worker session's working directory is not confirmed.")
	case errors.Is(err, service.ErrSessionBusy):
		return NewConflictCode("worker_session_busy", "The worker session already has a request in flight.")
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_worker_request", "Worker request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("resource")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("worker state conflict")
	}
	return NewInternalError("worker request failed")
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func toSessionDTO(s *persistence.WorkerSessionRecord) map[string]any {
	return map[string]any{
		"id": s.ID.String(), "provider": s.Provider, "provider_session_id": s.ProviderSessionID, "status": s.Status,
		"policy": s.Policy, "working_directory": s.WorkingDirectory, "status_reason": s.StatusReason,
		"created_at": rfc(s.CreatedAt), "last_seen_at": rfc(s.LastSeenAt), "closed_at": timePtr(s.ClosedAt),
	}
}

func toContextDTO(c worker.ContextProgress) map[string]any {
	if !c.Known {
		return map[string]any{"known": false, "tokens": nil, "window": nil, "percentage": nil, "estimated": false, "source": c.Source, "label": c.Label}
	}
	return map[string]any{"known": true, "tokens": c.Tokens, "window": c.Window, "percentage": c.Percent, "estimated": c.Estimated, "source": c.Source, "label": c.Label}
}

func (a *workerAPI) status(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.svc.View(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	h, err := a.svc.Health(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	out := map[string]any{"provider": v.Provider, "connection": v.Connection, "session": nil, "context": toContextDTO(v.Context), "current_request": nil,
		"last_successful_request": timePtr(h.LastSuccessfulRequest), "last_error": nil,
		"liveness": map[string]bool{"transport": h.Liveness.TransportAlive, "session": h.Liveness.SessionAlive, "request_executing": h.Liveness.RequestExecuting}}
	if v.SessionID != nil {
		out["session"] = map[string]any{"id": v.SessionID.String(), "status": v.SessionStatus, "working_directory": v.WorkingDirectory}
	}
	if c := v.Current; c != nil {
		out["current_request"] = map[string]any{"dispatch_id": c.DispatchID.String(), "work_unit_id": uuidPtr(c.WorkUnitID), "role": c.RoleKey,
			"state": c.StateKey, "status": c.Status, "started_at": rfc(c.StartedAt), "duration_ms": c.Duration.Milliseconds()}
	}
	if e := h.LastError; e != nil {
		out["last_error"] = map[string]any{"code": e.Code, "message": e.Message, "delivery": e.Delivery, "provider_code": e.ProviderCode, "at": rfc(e.At)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *workerAPI) sessions(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if _, err := a.svc.Health(r.Context(), ids[0]); err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	ss, err := a.svc.Sessions(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	out := make([]map[string]any, 0, len(ss))
	for i := range ss {
		out = append(out, toSessionDTO(&ss[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "count": len(out)})
}

func (a *workerAPI) session(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "session")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.svc.Session(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(s))
}

// toDispatchDTO never includes prompt content (WBS 12.83.4): prompt id +
// hash only.
func toDispatchDTO(d worker.WorkerDispatch) map[string]any {
	out := map[string]any{
		"id": d.ID.String(), "session_id": d.SessionID.String(), "phase_id": uuidPtr(d.PhaseID), "work_unit_id": uuidPtr(d.WorkUnitID),
		"worker_prompt_id": d.WorkerPromptID.String(), "prompt_hash": d.PromptHash, "runtime_state_id": d.RuntimeStateID.String(),
		"state": d.StateKey, "role": d.RoleKey, "request_id": d.RequestID.String(), "operation_id": d.OperationID, "attempt": d.Attempt,
		"status": d.Status, "delivery": d.Delivery, "retry_class": d.RetryClass, "error_code": d.ErrorCode, "error_message": d.ErrorMessage,
		"provider_code": d.ProviderCode, "retry_after_ms": nil, "created_at": rfc(d.CreatedAt), "started_at": timePtr(d.StartedAt),
		"completed_at": timePtr(d.CompletedAt),
	}
	if d.RetryAfter != nil {
		out["retry_after_ms"] = d.RetryAfter.Milliseconds()
	}
	return out
}

func optUUID(r *http.Request, name string) (*uuid.UUID, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, NewValidationCode("invalid_filter", name+" must be a UUID")
	}
	return &id, nil
}

func (a *workerAPI) dispatches(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if _, err := a.svc.Health(r.Context(), ids[0]); err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	q := r.URL.Query()
	f := persistence.DispatchFilter{ProjectID: ids[0], StateKey: q.Get("state"), RoleKey: q.Get("role"), Status: worker.DispatchStatus(q.Get("status"))}
	if f.Status != "" && !f.Status.Valid() {
		WriteError(w, r, NewValidationCode("invalid_filter", "unknown dispatch status"))
		return
	}
	for name, dst := range map[string]**uuid.UUID{"phase": &f.PhaseID, "work_unit": &f.WorkUnitID, "session": &f.SessionID} {
		if *dst, err = optUUID(r, name); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	for name, dst := range map[string]*int{"limit": &f.Limit, "offset": &f.Offset} {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				WriteError(w, r, NewValidationCode("invalid_filter", name+" must be a non-negative integer"))
				return
			}
			*dst = n
		}
	}
	ds, total, err := a.svc.Dispatches(r.Context(), f)
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	out := make([]map[string]any, 0, len(ds))
	for _, d := range ds {
		out = append(out, toDispatchDTO(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"dispatches": out, "count": len(out), "total": total, "offset": f.Offset})
}

func (a *workerAPI) dispatch(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "dispatch")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	dd, err := a.svc.GetDispatch(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	out := toDispatchDTO(dd.Dispatch)
	att := make([]map[string]any, 0, len(dd.Attempts))
	for _, x := range dd.Attempts {
		att = append(att, toDispatchDTO(x))
	}
	out["attempts"] = att
	out["response"] = nil
	if rr := dd.Response; rr != nil {
		out["response"] = map[string]any{"id": rr.ID.String(), "content_hash": rr.ContentHash, "content_bytes": len(rr.Content),
			"completed_at": rfc(rr.CompletedAt)}
	}
	tools := make([]map[string]any, 0, len(dd.Tools))
	for _, t := range dd.Tools {
		tools = append(tools, map[string]any{"name": t.Activity.Name, "state": t.Activity.State, "started_at": rfc(t.Activity.StartedAt)})
	}
	out["tool_activity"] = tools
	writeJSON(w, http.StatusOK, out)
}

func (a *workerAPI) newSession(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in struct{}
	if err := decodeStrictOptionalJSON(r, "invalid_session_request", map[string]bool{}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.svc.NewSession(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toSessionDTO(s))
}

func (a *workerAPI) cwd(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeStrictJSON(r, "invalid_cwd_request", map[string]bool{"path": true}, []string{"path"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(in.Path) == "" {
		WriteError(w, r, NewValidationFields("invalid_cwd_request", "Working directory is required.", map[string]string{"path": "must not be empty"}))
		return
	}
	s, err := a.svc.ChangeDirectory(r.Context(), ids[0], in.Path)
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(s))
}

func (a *workerAPI) metrics(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if _, err := a.svc.Health(r.Context(), ids[0]); err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	m, err := a.svc.Metrics(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests": m.Requests, "successful": m.Successful, "failed": m.Failed, "cancelled": m.Cancelled, "delivery_unknown": m.DeliveryUnknown,
		"in_flight": m.InFlight, "sessions_created": m.SessionsCreated, "sessions_lost": m.SessionsLost, "session_resets": m.SessionResets,
		"connection_failures": m.ConnectionFailures, "reconnections": m.Reconnections, "avg_duration_ms": m.AvgDuration.Milliseconds(),
		"input_tokens": m.InputTokens, "output_tokens": m.OutputTokens, "avg_utilization": m.AvgUtilization, "peak_utilization": m.PeakUtilization,
	})
}
