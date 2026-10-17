// Phase 15: the execution bootstrap REST surface.
//
//	POST /api/v1/projects/{project}/bootstrap                               (WBS 15.95)
//	GET  /api/v1/projects/{project}/execution-readiness                     (WBS 15.96, 15.101/15.102 view)
//	GET  /api/v1/projects/{project}/bootstrap-preview                       (WBS 15.100)
//	GET  /api/v1/projects/{project}/executions                              (WBS 15.97)
//	GET  /api/v1/projects/{project}/executions/metrics                      (WBS 15.106)
//	GET  /api/v1/projects/{project}/executions/{execution}                  (WBS 15.97)
//	GET  /api/v1/projects/{project}/executions/{execution}/bootstrap        (WBS 15.98)
//	GET  /api/v1/projects/{project}/executions/{execution}/configuration    (WBS 15.99)
//	GET  /api/v1/projects/{project}/active-execution                        (WBS 15.97, 15.103 view)
//	GET  /api/v1/projects/{project}/settings/execution
//	PUT  /api/v1/projects/{project}/settings/execution
package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

type executionAPI struct {
	svc *service.BootstrapService
}

// RegisterExecutionRoutes exposes the Phase 15 endpoints.
func RegisterExecutionRoutes(r *Router, svc *service.BootstrapService) error {
	api := &executionAPI{svc: svc}
	base := "/api/v1/projects/{project}"
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodPost, base + "/bootstrap", api.bootstrap},
		{http.MethodGet, base + "/execution-readiness", api.readiness},
		{http.MethodGet, base + "/bootstrap-preview", api.preview},
		{http.MethodGet, base + "/executions", api.list},
		{http.MethodGet, base + "/executions/metrics", api.metrics},
		{http.MethodGet, base + "/executions/{execution}", api.get},
		{http.MethodGet, base + "/executions/{execution}/bootstrap", api.bootstrapStatus},
		{http.MethodGet, base + "/executions/{execution}/configuration", api.configuration},
		{http.MethodGet, base + "/active-execution", api.active},
		{http.MethodGet, base + "/settings/execution", api.getSettings},
		{http.MethodPut, base + "/settings/execution", api.putSettings},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func executionError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_request", "Request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("resource")
	case errors.Is(err, service.ErrConflict):
		return NewConflictCode("execution_conflict", "The execution changed concurrently.")
	}
	return NewInternalError("execution request failed")
}

// ToExecutionDTO renders an execution (no secrets, no prompt content).
func ToExecutionDTO(e *persistence.ExecutionRecord) map[string]any {
	if e == nil {
		return nil
	}
	return map[string]any{
		"id": e.ID.String(), "project_id": e.ProjectID.String(), "number": e.Number, "status": e.Status, "generation": e.Generation,
		"current_session_id": uuidPtr(e.CurrentSessionID), "recovery_required": e.RecoveryRequired(),
		"state_machine":    map[string]any{"id": e.StateMachineID.String(), "version": e.StateMachineVersion, "hash": e.StateMachineHash},
		"initial_phase_id": e.InitialPhaseID.String(), "configuration_snapshot_id": e.ConfigurationSnapshotID.String(),
		"wbs":     map[string]any{"version": e.WBSVersion, "hash": e.WBSHash},
		"plan_id": uuidPtr(e.PlanID), "plan_state": e.PlanState,
		"initial_state": e.InitialStateKey, "first_actionable_state": e.FirstActionableStateKey,
		"first_actionable_state_id": uuidPtr(e.FirstActionableStateID), "role": e.InitialRoleKey,
		"initial_work_unit_id": uuidPtr(e.InitialWorkUnitID), "initial_template": e.InitialTemplateKey,
		"initial_context_snapshot_id": uuidPtr(e.InitialContextSnapshotID), "initial_worker_prompt_id": uuidPtr(e.InitialWorkerPromptID),
		"initial_cache_key": e.InitialCacheKey, "plan_hash": e.PlanHash,
		"bootstrap": map[string]any{"stage": e.Stage, "failed_stage": e.FailedStage, "failure_code": e.FailureCode,
			"failure_class": e.FailureClass, "failure_reason": e.FailureReason},
		"started_at": rfc(e.StartedAt), "ready_at": timePtr(e.ReadyAt), "running_at": timePtr(e.RunningAt),
		"completed_at": timePtr(e.CompletedAt), "failed_at": timePtr(e.FailedAt), "cancelled_at": timePtr(e.CancelledAt),
		"version": e.Version, "created_at": rfc(e.CreatedAt), "updated_at": rfc(e.UpdatedAt),
		"engine": map[string]any{"current_phase_id": uuidPtr(e.CurrentPhaseID), "current_work_unit_id": uuidPtr(e.CurrentWorkUnitID),
			"runtime_state_id": uuidPtr(e.RuntimeStateID), "wait_reason": e.WaitReason, "recovery_reason": e.RecoveryReason,
			"recovery_evidence": e.RecoveryEvidence, "paused_at": timePtr(e.PausedAt), "last_scheduled_at": timePtr(e.LastScheduledAt),
			"driver_owner": e.DriverOwner, "finalization_generation": e.FinalizationGeneration},
	}
}

func readinessDTO(r *execution.ExecutionReadinessReport) any {
	if r == nil {
		return nil
	}
	return map[string]any{"report": r, "view": execution.NewReadinessView(r)}
}

// bootstrapFailureStatus maps a bootstrap code to its HTTP status.
func bootstrapFailureStatus(be *execution.BootstrapError, readinessFailed bool) int {
	switch {
	case readinessFailed && be.Code != execution.CodeProjectNotReady && be.Code != execution.CodeNoPhase:
		return http.StatusUnprocessableEntity
	case be.Code == execution.CodeProjectNotReady || be.Code == execution.CodeNoPhase || be.Code == execution.CodeConcurrentBootstrap:
		return http.StatusConflict
	case be.Class == execution.ClassConfiguration:
		return http.StatusUnprocessableEntity
	case be.Code == execution.CodePersistenceFailed:
		return http.StatusInternalServerError
	}
	return http.StatusConflict
}

func (a *executionAPI) bootstrap(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.svc.Bootstrap(r.Context(), ids[0])
	body := map[string]any{"outcome": res.Outcome, "execution": ToExecutionDTO(res.Execution), "readiness": readinessDTO(res.Readiness)}
	if res.Execution != nil {
		body["bootstrap_status"] = res.Execution.Stage
		body["initial_phase_id"] = res.Execution.InitialPhaseID.String()
		body["initial_workflow_state"] = res.Execution.InitialStateKey
		body["first_actionable_state"] = res.Execution.FirstActionableStateKey
		body["role"] = res.Execution.InitialRoleKey
	} else if res.Plan != nil {
		body["initial_workflow_state"] = res.Plan.InitialState
		body["first_actionable_state"] = res.Plan.FirstActionable
		body["role"] = res.Plan.Role
	}
	if err != nil {
		be, ok := execution.AsBootstrapError(err)
		if !ok {
			WriteError(w, r, executionError(err))
			return
		}
		readinessFailed := res.Readiness != nil && !res.Readiness.Ready && be.Stage == execution.StageValidating
		body["error"] = map[string]any{"code": be.Code, "class": be.Class, "stage": be.Stage, "message": be.Message}
		if res.Readiness != nil {
			body["completion_eligible"] = res.Readiness.CompletionEligible
		}
		body["request_id"] = RequestIDFromContext(r.Context())
		writeJSON(w, bootstrapFailureStatus(be, readinessFailed), body)
		return
	}
	status := http.StatusOK
	if res.Outcome == service.OutcomeBootstrapped {
		status = http.StatusCreated
	}
	writeJSON(w, status, body)
}

func (a *executionAPI) readiness(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rep, err := a.svc.Readiness(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": rep.Ready, "completion_eligible": rep.CompletionEligible,
		"errors": rep.Errors, "warnings": rep.Warnings, "view": execution.NewReadinessView(rep)})
}

func (a *executionAPI) preview(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := a.svc.Preview(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preview": p.View, "plan": p.Plan, "plan_hash": p.PlanHash,
		"steps": p.Plan.Steps(), "readiness": readinessDTO(p.Readiness)})
}

func (a *executionAPI) list(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit, offset, err := paging(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	list, total, err := a.svc.Executions(r.Context(), ids[0], limit, offset)
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, ToExecutionDTO(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"executions": out, "count": len(out), "total": total, "offset": offset})
}

func (a *executionAPI) get(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	e, err := a.svc.Execution(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	writeJSON(w, http.StatusOK, ToExecutionDTO(e))
}

func (a *executionAPI) active(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	e, v, err := a.svc.ActiveExecutionView(r.Context(), ids[0])
	if errors.Is(err, service.ErrNotFound) {
		if _, _, perr := a.svc.ExecutionSettings(r.Context(), ids[0]); errors.Is(perr, service.ErrNotFound) {
			WriteError(w, r, NewNotFoundError("project"))
			return
		}
		WriteError(w, r, &appError{sentinel: ErrNotFound, code: "no_active_execution", msg: "The project has no active execution."})
		return
	}
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"execution": ToExecutionDTO(e), "view": v})
}

func (a *executionAPI) bootstrapStatus(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	st, err := a.svc.BootstrapStatusOf(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	hist := make([]map[string]any, 0, len(st.History))
	for _, h := range st.History {
		var from any
		if h.FromStage != nil {
			from = *h.FromStage
		}
		hist = append(hist, map[string]any{"sequence": h.Sequence, "from": from, "to": h.ToStage, "status": h.Status,
			"reason": h.Reason, "created_at": rfc(h.CreatedAt)})
	}
	evs := make([]map[string]any, 0, len(st.Events))
	for _, e := range st.Events {
		evs = append(evs, map[string]any{"id": e.ID.String(), "type": e.Type, "payload": e.Payload, "created_at": rfc(e.CreatedAt)})
	}
	e := st.Execution
	writeJSON(w, http.StatusOK, map[string]any{"execution_id": e.ID.String(), "stage": e.Stage, "status": e.Status,
		"failed_stage": e.FailedStage, "failure_code": e.FailureCode, "failure_class": e.FailureClass, "failure_reason": e.FailureReason,
		"history": hist, "events": evs})
}

func (a *executionAPI) configuration(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	snap, c, err := a.svc.Configuration(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": snap.ID.String(), "execution_id": snap.ExecutionID.String(),
		"project_version": snap.ProjectVersion, "hash": snap.Hash, "created_at": rfc(snap.CreatedAt),
		"redacted": true, "configuration": c})
}

func (a *executionAPI) metrics(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	m, err := a.svc.Metrics(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type executionSettingsDTO struct {
	RequireGitRepository           bool   `json:"require_git_repository"`
	DirtyPolicy                    string `json:"dirty_policy"`
	MaterializeStateFiles          bool   `json:"materialize_state_files"`
	MaxBootstrapControlTransitions int    `json:"max_bootstrap_control_transitions"`
	BootstrapLeaseSeconds          int    `json:"bootstrap_lease_seconds"`
}

var executionSettingsFields = map[string]bool{"require_git_repository": true, "dirty_policy": true, "materialize_state_files": true,
	"max_bootstrap_control_transitions": true, "bootstrap_lease_seconds": true}

func toExecutionSettingsDTO(s execution.Settings) executionSettingsDTO {
	return executionSettingsDTO{RequireGitRepository: s.RequireGitRepository, DirtyPolicy: string(s.DirtyPolicy),
		MaterializeStateFiles: s.MaterializeStateFiles, MaxBootstrapControlTransitions: s.MaxBootstrapControlTransitions,
		BootstrapLeaseSeconds: int(s.BootstrapLease / time.Second)}
}

func (a *executionAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, stored, err := a.svc.ExecutionSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": toExecutionSettingsDTO(s), "stored": stored})
}

func (a *executionAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cur, _, err := a.svc.ExecutionSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	in := toExecutionSettingsDTO(cur)
	if err := decodeStrictJSON(r, "invalid_execution_settings", executionSettingsFields, nil, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	st := execution.Settings{RequireGitRepository: in.RequireGitRepository, DirtyPolicy: execution.DirtyPolicy(in.DirtyPolicy),
		MaterializeStateFiles: in.MaterializeStateFiles, MaxBootstrapControlTransitions: in.MaxBootstrapControlTransitions,
		BootstrapLease: time.Duration(in.BootstrapLeaseSeconds) * time.Second}
	if err := st.Validate(); err != nil {
		WriteError(w, r, NewValidationFields("invalid_execution_settings", "Execution settings are invalid.", map[string]string{"settings": err.Error()}))
		return
	}
	saved, err := a.svc.UpdateExecutionSettings(r.Context(), ids[0], st)
	if err != nil {
		WriteError(w, r, executionError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": toExecutionSettingsDTO(saved), "stored": true})
}

var _ = uuid.Nil
