// Phase 16: the autonomous execution engine REST surface (P16-138/139).
//
//	POST /api/v1/projects/{project}/executions/{execution}/step              (WBS 16.1)
//	POST /api/v1/projects/{project}/executions/{execution}/run               (WBS 16.1)
//	POST /api/v1/projects/{project}/executions/{execution}/pause             (WBS 16.72)
//	POST /api/v1/projects/{project}/executions/{execution}/resume            (WBS 16.73)
//	POST /api/v1/projects/{project}/executions/{execution}/cancel            (WBS 16.74)
//	POST /api/v1/projects/{project}/executions/{execution}/requeue           restart the current task
//	POST /api/v1/projects/{project}/executions/{execution}/pause-after-task  pause before the next task's work
//	POST /api/v1/projects/{project}/executions/{execution}/pause-after-task/cancel
//	GET  /api/v1/projects/{project}/executions/{execution}/task-ops          task analysis / fix panel
//	POST /api/v1/projects/{project}/executions/{execution}/task-ops/analyze  analyze the task (recovery)
//	POST /api/v1/projects/{project}/executions/{execution}/task-ops/fix      implement the analysis
//	POST /api/v1/projects/{project}/executions/{execution}/recovery/resolve  (WBS 16.76)
//	GET  /api/v1/projects/{project}/executions/{execution}/timeline          (WBS 16.81)
//	GET  /api/v1/projects/{project}/executions/{execution}/steps
//	GET  /api/v1/projects/{project}/executions/{execution}/validations       (WBS 16.44)
//	GET  /api/v1/projects/{project}/executions/{execution}/checkpoints       (WBS 16.54)
//	GET  /api/v1/projects/{project}/executions/{execution}/fix-attempts      (WBS 16.50)
//	GET  /api/v1/projects/{project}/executions/{execution}/summary           (WBS 16.161)
//	GET  /api/v1/projects/{project}/executions/{execution}/hardening         (WBS 16.145/16.146)
//	GET  /api/v1/projects/{project}/executions/{execution}/hardening/groups  (WBS 16.147/16.148)
//	GET  /api/v1/projects/{project}/settings/engine
//	PUT  /api/v1/projects/{project}/settings/engine
package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/service"
	"overseer/internal/wbs"
)

type engineAPI struct {
	eng       *service.Engine
	lifecycle *service.LifecycleService
}

// RegisterEngineRoutes exposes the Phase 16 endpoints.
func RegisterEngineRoutes(r *Router, eng *service.Engine, lifecycle *service.LifecycleService) error {
	api := &engineAPI{eng: eng, lifecycle: lifecycle}
	base := "/api/v1/projects/{project}"
	ex := base + "/executions/{execution}"
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodPost, ex + "/step", api.step},
		{http.MethodPost, ex + "/run", api.run},
		{http.MethodPost, ex + "/pause", api.lifecycleOp("pause")},
		{http.MethodPost, ex + "/resume", api.lifecycleOp("resume")},
		{http.MethodPost, ex + "/cancel", api.lifecycleOp("cancel")},
		{http.MethodPost, ex + "/requeue", api.requeue},
		{http.MethodPost, ex + "/pause-after-task", api.pauseAfterTask(true)},
		{http.MethodPost, ex + "/pause-after-task/cancel", api.pauseAfterTask(false)},
		{http.MethodGet, ex + "/task-ops", api.taskOps},
		{http.MethodPost, ex + "/task-ops/analyze", api.taskOp(service.TaskOpAnalyze)},
		{http.MethodPost, ex + "/task-ops/fix", api.taskOp(service.TaskOpFix)},
		{http.MethodPost, ex + "/recovery/resolve", api.resolve},
		{http.MethodGet, ex + "/timeline", api.timeline},
		{http.MethodGet, ex + "/steps", api.steps},
		{http.MethodGet, ex + "/validations", api.validations},
		{http.MethodGet, ex + "/checkpoints", api.checkpoints},
		{http.MethodGet, ex + "/fix-attempts", api.fixAttempts},
		{http.MethodGet, ex + "/summary", api.summary},
		{http.MethodGet, ex + "/hardening", api.hardening},
		{http.MethodGet, ex + "/hardening/groups", api.hardeningGroups},
		{http.MethodGet, base + "/settings/engine", api.getSettings},
		{http.MethodPut, base + "/settings/engine", api.putSettings},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func engineError(err error) error {
	switch {
	case errors.Is(err, service.ErrExecutionBusy):
		return NewConflictCode("execution_busy", "The execution is driven by another scheduler.")
	case errors.Is(err, service.ErrTaskOpRunning):
		return NewConflictCode("task_op_running", "A task analysis or fix is running; wait until it finishes.")
	}
	return executionError(err)
}

func (a *engineAPI) step(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if _, err := a.eng.Execution(r.Context(), ids[0], ids[1]); err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	res, err := a.eng.Step(r.Context(), ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (a *engineAPI) run(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	budget := 0
	if b := r.URL.Query().Get("budget"); b != "" {
		if budget, err = strconv.Atoi(b); err != nil || budget < 1 || budget > execution.MaxStepBudget {
			WriteError(w, r, NewValidationCode("invalid_budget", "budget must be in [1, 1000]"))
			return
		}
	}
	if _, err := a.eng.Execution(r.Context(), ids[0], ids[1]); err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	if budget == 0 {
		budget = execution.DefaultEngineSettings().StepBudget
	}
	res, err := a.eng.Run(r.Context(), ids[1], budget)
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res, "count": len(res)})
}

func (a *engineAPI) lifecycleOp(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids, err := pathUUIDs(r, "project", "execution")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if _, err := a.eng.Execution(r.Context(), ids[0], ids[1]); err != nil {
			WriteError(w, r, engineError(err))
			return
		}
		pid := ids[0].String()
		switch op {
		case "pause":
			_, err = a.lifecycle.Pause(r.Context(), pid)
		case "resume":
			_, err = a.lifecycle.Resume(r.Context(), pid)
		default:
			_, err = a.lifecycle.Cancel(r.Context(), pid)
		}
		if err != nil {
			if errors.Is(err, service.ErrInvalidInput) {
				WriteError(w, r, NewConflictCode("invalid_transition", "The project cannot "+op+" from its current status."))
				return
			}
			WriteError(w, r, engineError(err))
			return
		}
		e, err := a.eng.Execution(r.Context(), ids[0], ids[1])
		if err != nil {
			WriteError(w, r, engineError(err))
			return
		}
		writeJSON(w, http.StatusOK, ToExecutionDTO(e))
	}
}

var resolveFields = map[string]bool{"action": true, "work_unit_id": true, "check_indexes": true, "by": true, "note": true}

func (a *engineAPI) resolve(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in service.RecoveryResolution
	if err := decodeStrictJSON(r, "invalid_recovery_resolution", resolveFields, []string{"action"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	e, err := a.eng.ResolveRecovery(r.Context(), ids[0], ids[1], in)
	if err != nil {
		if errors.Is(err, service.ErrConflict) {
			WriteError(w, r, NewConflictCode("not_recovery_required", "The execution does not require recovery."))
			return
		}
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, ToExecutionDTO(e))
}

// pauseAfterTask requests (on) or cancels a pause once the next task is
// loaded, before work on it begins.
func (a *engineAPI) pauseAfterTask(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids, err := pathUUIDs(r, "project", "execution")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		set := a.eng.CancelPauseAfterTask
		if on {
			set = a.eng.RequestPauseAfterTask
		}
		if err := set(r.Context(), ids[0], ids[1], "operator"); err != nil {
			if errors.Is(err, service.ErrPauseAfterTaskUnavailable) {
				WriteError(w, r, NewConflictCode("pause_after_task_unavailable", strings.TrimPrefix(err.Error(), service.ErrConflict.Error()+": ")))
				return
			}
			WriteError(w, r, engineError(err))
			return
		}
		pending, err := a.eng.PauseAfterTaskPending(r.Context(), ids[1])
		if err != nil {
			WriteError(w, r, engineError(err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pause_after_task": pending})
	}
}

// requeue restarts the execution's current task from the beginning
// (applied now when the execution is idle, else when the running step
// finishes). The reason is the X-Overseer-Reason header.
func (a *engineAPI) requeue(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	reason := strings.TrimSpace(r.Header.Get(ReasonHeader))
	if reason == "" {
		WriteError(w, r, NewValidationCode("reason_required", "Requeueing a task requires a reason ("+ReasonHeader+" header)."))
		return
	}
	res, err := a.eng.Requeue(r.Context(), ids[0], ids[1], "operator", reason)
	if err != nil {
		if errors.Is(err, service.ErrNotRequeueable) {
			WriteError(w, r, NewConflictCode("not_requeueable", strings.TrimPrefix(err.Error(), service.ErrConflict.Error()+": ")))
			return
		}
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// taskOps returns the recovery screen's task analysis / fix panel.
func (a *engineAPI) taskOps(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.eng.TaskOps(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// taskOp starts a task analysis or fix (202: it runs in the background;
// GET task-ops reports progress and outcome).
func (a *engineAPI) taskOp(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids, err := pathUUIDs(r, "project", "execution")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		start := a.eng.AnalyzeTask
		if op == service.TaskOpFix {
			start = a.eng.FixTask
		}
		if err := start(r.Context(), ids[0], ids[1], "operator"); err != nil {
			if errors.Is(err, service.ErrConflict) {
				WriteError(w, r, NewConflictCode("task_op_unavailable", strings.TrimPrefix(err.Error(), service.ErrConflict.Error()+": ")))
				return
			}
			WriteError(w, r, engineError(err))
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"started": op})
	}
}

func (a *engineAPI) timeline(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	evs, err := a.eng.Timeline(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		out = append(out, map[string]any{"id": e.ID.String(), "seq": e.ExecSeq, "type": e.Type, "payload": e.Payload, "created_at": rfc(e.CreatedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "count": len(out)})
}

func (a *engineAPI) steps(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit, offset, err := paging(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	list, total, err := a.eng.Steps(r.Context(), ids[0], ids[1], limit, offset)
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": list, "count": len(list), "total": total, "offset": offset})
}

func (a *engineAPI) validations(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	runs, err := a.eng.ValidationRuns(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "count": len(runs)})
}

func (a *engineAPI) checkpoints(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cps, err := a.eng.Checkpoints(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": cps, "count": len(cps)})
}

func (a *engineAPI) fixAttempts(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	fs, err := a.eng.FixAttempts(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fix_attempts": fs, "count": len(fs)})
}

func (a *engineAPI) summary(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.eng.View(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"execution": ToExecutionDTO(v.Execution), "state": v.State, "summary": v.Summary,
		"wbs_progress": v.WBSProgress, "completion": v.Completion})
}

func (a *engineAPI) hardening(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.eng.Hardening(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pass": v.Pass, "progress": v.Progress, "report": v.Report})
}

func (a *engineAPI) hardeningGroups(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.eng.Hardening(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	out := make([]map[string]any, 0, len(v.Groups))
	for _, g := range v.Groups {
		out = append(out, map[string]any{"id": g.ID.String(), "key": g.Key, "round": g.Round, "classification": g.Classification,
			"reason": g.Reason, "status": g.Status, "destination": g.Destination, "lineage": g.Lineage, "members": len(g.Members),
			"functions_removed": g.FunctionsRemoved, "references_migrated": g.RefsMigrated, "tests_before": g.TestsBefore, "tests_after": g.TestsAfter})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "count": len(out)})
}

// engineSettingsDTO is the wire form of the engine + hardening settings.
type engineSettingsDTO struct {
	MaxFixAttempts   int    `json:"max_fix_attempts"`
	MaxPlanAttempts  int    `json:"max_plan_attempts"`
	CheckpointPolicy string `json:"checkpoint_policy"`
	// UseGit is the project's "use git" option: setting it sets the task
	// and hardening-group checkpoint policies (REQUIRED / DISABLED).
	UseGit                   bool                 `json:"use_git"`
	UnitValidation           []wbs.ValidationSpec `json:"unit_validation"`
	PhaseValidation          []wbs.ValidationSpec `json:"phase_validation"`
	FinalValidation          []wbs.ValidationSpec `json:"final_validation"`
	ValidationTimeoutSeconds int                  `json:"validation_timeout_seconds"`
	OutputCapBytes           int                  `json:"output_cap_bytes"`
	StepBudget               int                  `json:"step_budget"`
	DriverLeaseSeconds       int                  `json:"driver_lease_seconds"`
	AtomicPolicy             wbs.AtomicPolicy     `json:"atomic_policy"`
	FinalHardening           hardening.Settings   `json:"final_hardening"`
}

var engineSettingsFields = map[string]bool{"max_fix_attempts": true, "max_plan_attempts": true, "checkpoint_policy": true, "use_git": true,
	"unit_validation": true, "phase_validation": true, "final_validation": true, "validation_timeout_seconds": true,
	"output_cap_bytes": true, "step_budget": true, "driver_lease_seconds": true, "atomic_policy": true, "final_hardening": true}

func toEngineSettingsDTO(s execution.EngineSettings, h hardening.Settings) engineSettingsDTO {
	s.Normalize()
	return engineSettingsDTO{MaxFixAttempts: s.MaxFixAttempts, MaxPlanAttempts: s.MaxPlanAttempts, CheckpointPolicy: string(s.CheckpointPolicy),
		UseGit:         s.CheckpointPolicy.UsesGit(),
		UnitValidation: s.UnitValidation, PhaseValidation: s.PhaseValidation, FinalValidation: s.FinalValidation,
		ValidationTimeoutSeconds: int(s.ValidationTimeout / time.Second), OutputCapBytes: s.OutputCapBytes, StepBudget: s.StepBudget,
		DriverLeaseSeconds: int(s.DriverLease / time.Second), AtomicPolicy: s.AtomicPolicy, FinalHardening: h}
}

func (a *engineAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, h, stored, err := a.eng.EngineSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": toEngineSettingsDTO(s, h), "stored": stored})
}

func (a *engineAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cur, h, _, err := a.eng.EngineSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	in := toEngineSettingsDTO(cur, h)
	if err := decodeStrictJSON(r, "invalid_engine_settings", engineSettingsFields, nil, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	if in.UseGit != cur.CheckpointPolicy.UsesGit() {
		// "Use git" toggled: both checkpoint policies follow it.
		pol := execution.CheckpointPolicyFor(in.UseGit)
		in.CheckpointPolicy, in.FinalHardening.GroupCheckpointPolicy = string(pol), string(pol)
	}
	st := execution.EngineSettings{MaxFixAttempts: in.MaxFixAttempts, MaxPlanAttempts: in.MaxPlanAttempts,
		CheckpointPolicy: execution.CheckpointPolicy(in.CheckpointPolicy), UnitValidation: in.UnitValidation, PhaseValidation: in.PhaseValidation,
		FinalValidation: in.FinalValidation, ValidationTimeout: time.Duration(in.ValidationTimeoutSeconds) * time.Second,
		OutputCapBytes: in.OutputCapBytes, StepBudget: in.StepBudget, DriverLease: time.Duration(in.DriverLeaseSeconds) * time.Second,
		AtomicPolicy: in.AtomicPolicy}
	if err := errors.Join(st.Validate(), in.FinalHardening.Validate()); err != nil {
		WriteError(w, r, NewValidationFields("invalid_engine_settings", "Engine settings are invalid.", map[string]string{"settings": err.Error()}))
		return
	}
	if err := a.eng.UpdateEngineSettings(r.Context(), ids[0], st, in.FinalHardening); err != nil {
		WriteError(w, r, engineError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": toEngineSettingsDTO(st, in.FinalHardening), "stored": true})
}

var _ = json.Marshal
