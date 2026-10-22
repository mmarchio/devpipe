package handlers

import (
	"net/http"
	"net/url"
	"time"

	"overseer/internal/access"
	"overseer/internal/readmodel"
	"overseer/internal/web/viewmodels"
)

// routes is the console route table (WBS 17.141: URLs represent the
// resource; every GET is read-only, every mutation is a POST delegated to
// an /api/v1 route).
func (c *Console) routes() []route {
	p := "/projects/{id}"
	return []route{
		get("/{$}", c.dashboard),
		get("/projects", c.projects),
		get("/projects/new", c.newProject),
		post("/projects", access.ActionProjectCreate, c.createProject),
		get(p, c.overviewPage),
		get(p+"/readiness", c.readiness),
		post(p+"/ready", access.ActionProjectUpdate, c.markReady),
		get(p+"/start", c.startPage),
		post(p+"/start", access.ActionLifecycleStart, c.start),
		post(p+"/pause", access.ActionLifecyclePause, c.lifecycle("pause")),
		post(p+"/resume", access.ActionLifecycleResume, c.lifecycle("resume")),
		post(p+"/pause-after-task", access.ActionLifecyclePause, c.pauseAfterTask(true)),
		post(p+"/pause-after-task/cancel", access.ActionLifecyclePause, c.pauseAfterTask(false)),
		get(p+"/cancel", c.cancelPage),
		post(p+"/cancel", access.ActionLifecycleCancel, c.cancel),
		get(p+"/requeue", c.requeuePage),
		post(p+"/requeue", access.ActionRecoveryAction, c.requeue),
		get(p+"/delete", c.deleteProjectPage),
		post(p+"/delete", access.ActionProjectDelete, c.deleteProject),
		get(p+"/phases", c.phases),
		post(p+"/phases", access.ActionPhaseAppend, c.appendPhase),
		post(p+"/phases/wbs-upload", access.ActionPhaseAppend, c.uploadPhases),
		post(p+"/phases/order", access.ActionPhaseUpdate, c.reorderPhases),
		get(p+"/phases/{phase}", c.phaseDetail),
		get(p+"/phases/{phase}/wbs", c.wbsEditor),
		post(p+"/phases/{phase}/wbs", access.ActionWBSEdit, c.wbsSave),
		get(p+"/phases/{phase}/wbs/history", c.wbsHistory),
		get(p+"/phases/{phase}/work-units", c.units),
		get(p+"/phases/{phase}/graph", c.graph),
		get(p+"/work-units/{unit}", c.unit),
		get(p+"/execution", c.executionPage),
		get(p+"/executions/{execution}", c.executionPage),
		get(p+"/executions/{execution}/recovery", c.recoveryPage),
		post(p+"/executions/{execution}/recovery", access.ActionRecoveryAction, c.recoveryAction),
		post(p+"/executions/{execution}/recovery/analyze", access.ActionRecoveryAction, c.taskOp("analyze")),
		post(p+"/executions/{execution}/recovery/fix", access.ActionRecoveryAction, c.taskOp("fix")),
		get(p+"/worker", c.workerPage),
		post(p+"/worker/test", access.ActionWorkerOperate, c.workerTest),
		get(p+"/compressor", c.compressorPage),
		post(p+"/compressor/test", access.ActionWorkerOperate, c.compressorTest),
		get(p+"/results/{result}", c.resultPage),
		get(p+"/continuations", c.continuationsPage),
		get(p+"/validations", c.validationsPage),
		get(p+"/validations/{run}", c.validationPage),
		get(p+"/validations/{run}/output/{result}", c.outputPage),
		get(p+"/checkpoints/{checkpoint}", c.checkpointPage),
		get(p+"/repository", c.repositoryPage),
		get(p+"/metrics", c.metricsPage),
		get(p+"/roles", c.roles),
		get(p+"/roles/{role}", c.role),
		post(p+"/roles/{role}", access.ActionRoleEdit, c.roleSave),
		get(p+"/roles/{role}/preview", c.rolePreview),
		post(p+"/roles/{role}/reset", access.ActionRoleEdit, c.roleReset),
		post(p+"/roles/{role}/set-default", access.ActionRoleEdit, c.roleSetDefault),
		get(p+"/workflow", c.workflow),
		post(p+"/workflow/validate", access.ActionPreview, c.workflowValidate),
		get(p+"/workflow/states/{key}", c.stateEdit),
		post(p+"/workflow/states/{key}", access.ActionWorkflowEdit, c.stateSave),
		get(p+"/workflow/transitions/{tid}", c.transitionEdit),
		post(p+"/workflow/transitions/{tid}", access.ActionWorkflowEdit, c.transitionSave),
		get(p+"/state-files", c.stateFiles),
		get(p+"/state-files/{file}", c.stateFile),
		post(p+"/state-files/{file}", access.ActionStateFileEdit, c.stateFileSave),
		post(p+"/state-files/{file}/drift", access.ActionDriftResolve, c.stateFileDrift),
		get(p+"/templates", c.templatesPage),
		get(p+"/templates/{template}", c.templatePage),
		post(p+"/templates/{template}", access.ActionTemplateEdit, c.templateSave),
		get(p+"/prompts", c.promptsPage),
		get(p+"/prompts/preview", c.promptPreviewPage),
		post(p+"/prompts/preview", access.ActionPreview, c.promptPreview),
		get(p+"/prompts/worker/{worker}", c.workerPrompt),
		get(p+"/prompts/{prompt}", c.promptPage),
		get(p+"/prompts/{prompt}/diff/{attempt}", c.promptDiff),
		get(p+"/cache", c.cachePage),
		get(p+"/cache/{entry}", c.cacheEntry),
		get(p+"/precompute", c.precomputePage),
		get(p+"/config", c.configIndex),
		get(p+"/config/worker", c.configWorker),
		post(p+"/config/worker", access.ActionConfigEdit, c.configWorkerSave),
		get(p+"/config/compression", c.configCompression),
		post(p+"/config/compression", access.ActionConfigEdit, c.configCompressionSave),
		get(p+"/config/planning", c.configPlanning),
		post(p+"/config/planning", access.ActionConfigEdit, c.configPlanningSave),
		get(p+"/config/continuation", c.configContinuation),
		post(p+"/config/continuation", access.ActionConfigEdit, c.configContinuationSave),
		get(p+"/config/checkpoint", c.configCheckpoint),
		post(p+"/config/checkpoint", access.ActionConfigEdit, c.configCheckpointSave),
		// Live-update fragments (WBS 17.79).
		get(p+"/fragments/overview", c.fragOverview),
		get(p+"/fragments/current-task", c.fragCurrentTask),
		get(p+"/fragments/actions", c.fragActions),
		get(p+"/fragments/flows", c.fragFlows),
		get(p+"/fragments/units", c.fragUnits),
		get(p+"/fragments/timeline", c.fragTimeline),
		get(p+"/fragments/recovery", c.fragRecovery),
		get(p+"/fragments/task-ops", c.fragTaskOps),
		get(p+"/fragments/task-ops-overview", c.fragTaskOpsOverview),
		// Global.
		get("/executions", c.executions),
		get("/workers", c.workers),
		get("/configuration", c.configuration),
		get("/configuration/system", c.configSystem),
		post("/configuration/system", access.ActionMaintenance, c.configSystemSave),
		post("/configuration/system/test", access.ActionMaintenance, c.configSystemTest),
		get("/configuration/models", c.configModels),
		get("/configuration/models/{kind}/new", c.configModelNew),
		post("/configuration/models/{kind}", access.ActionMaintenance, c.configModelSave),
		get("/configuration/models/{kind}/{name}", c.configModel),
		post("/configuration/models/{kind}/{name}", access.ActionMaintenance, c.configModelSave),
		post("/configuration/models/{kind}/{name}/delete", access.ActionMaintenance, c.configModelDelete),
		get("/audit", c.audit),
		get("/audit/{event}", c.auditEvent),
		get("/system", c.system),
	}
}

var auditActions = []string{"PROJECT_CREATE", "PROJECT_UPDATE", "PROJECT_DELETE", "LIFECYCLE_START", "LIFECYCLE_PAUSE", "LIFECYCLE_RESUME",
	"LIFECYCLE_CANCEL", "ENGINE_STEP", "PHASE_APPEND", "PHASE_UPDATE", "WBS_EDIT", "CONFIG_EDIT", "ROLE_EDIT", "WORKFLOW_EDIT",
	"STATE_FILE_EDIT", "TEMPLATE_EDIT", "DRIFT_RESOLVE", "RECOVERY_ACTION", "WORKER_OPERATE", "MAINTENANCE",
	"ACKNOWLEDGE_AUDIT_FAILURE", "RESTORE_ENABLE"}

type auditData struct {
	Page        readmodel.AuditPage
	Q           map[string]string
	Actions     []string
	Results     []string
	QueryString string
	// System carries the latest audit chain verification (WBS 18.110).
	System readmodel.SystemStatus
}

// audit — GET /audit (WBS 17.103 / 17.104): server-side filters and keyset
// pagination.
func (c *Console) audit(w http.ResponseWriter, r *http.Request) {
	keys := []string{"project", "execution", "phase", "unit", "action", "result", "from", "to"}
	q := map[string]string{}
	for _, k := range keys {
		q[k] = r.URL.Query().Get(k)
	}
	filters := qs(r, keys...)
	d := auditData{Q: q, Actions: auditActions, Results: []string{"SUCCEEDED", "REJECTED", "FAILED"}, QueryString: filters}
	page := viewmodels.Page{Shell: c.shell(r, "Audit", "Audit")}
	resp := c.d.API.Get(r, "/api/v1/audit?"+qs(r, append(keys, "cursor")...), &d.Page)
	if !resp.OK() {
		if resp.Status != http.StatusBadRequest {
			c.apiError(w, r, resp)
			return
		}
		_, page.Message, _ = resp.Error()
	}
	_ = c.d.API.Get(r, "/api/v1/system/status", &d.System)
	page.Data = d
	c.render(w, r, http.StatusOK, "audit", page)
}

// auditEvent — GET /audit/{event} (WBS 17.105 / 17.142).
func (c *Console) auditEvent(w http.ResponseWriter, r *http.Request) {
	var e readmodel.AuditRow
	if !c.load(w, r, "/api/v1/audit/"+url.PathEscape(r.PathValue("event")), &e) {
		return
	}
	c.render(w, r, http.StatusOK, "audit_event", viewmodels.Page{Shell: c.shell(r, "Audit event", "Audit"), Data: e})
}

// system — GET /system (WBS 17.107–17.109).
func (c *Console) system(w http.ResponseWriter, r *http.Request) {
	var s systemData
	if !c.load(w, r, "/api/v1/system/status", &s.SystemStatus) {
		return
	}
	// Active alerts (decision U10: API, console, logs).
	var al struct {
		Alerts []systemAlert `json:"alerts"`
	}
	_ = c.d.API.Get(r, "/api/v1/system/alerts", &al)
	for _, a := range al.Alerts {
		if a.Active {
			s.Alerts = append(s.Alerts, a)
		}
	}
	// Production readiness from the backend report (WBS 18.209).
	var rd systemReadiness
	if resp := c.d.API.Get(r, "/api/v1/system/readiness", &rd); resp.OK() {
		s.Readiness = &rd
	}
	c.render(w, r, http.StatusOK, "system", viewmodels.Page{Shell: c.shell(r, "System", "System"), Data: s})
}

type systemReadiness struct {
	Status string `json:"status"`
	Checks []struct {
		Area   string `json:"area"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"checks"`
}

type systemAlert struct {
	ID     string    `json:"id"`
	Active bool      `json:"active"`
	Detail string    `json:"detail"`
	Since  time.Time `json:"since"`
}

type systemData struct {
	readmodel.SystemStatus
	Alerts    []systemAlert
	Readiness *systemReadiness
}

// executions — GET /executions: active executions across projects.
func (c *Console) executions(w http.ResponseWriter, r *http.Request) {
	var l readmodel.ProjectList
	if !c.load(w, r, "/api/v1/console/projects?active=active&"+qs(r, "limit", "offset"), &l) {
		return
	}
	c.render(w, r, http.StatusOK, "executions", viewmodels.Page{Shell: c.shell(r, "Executions", "Executions"), Data: l})
}

// workers — GET /workers: configured worker providers + health.
func (c *Console) workers(w http.ResponseWriter, r *http.Request) {
	workers, _ := c.providerLists(r)
	var s readmodel.SystemStatus
	c.d.API.Get(r, "/api/v1/system/status", &s)
	c.render(w, r, http.StatusOK, "workers", viewmodels.Page{Shell: c.shell(r, "Workers", "Workers"),
		Data: map[string]any{"Providers": workers, "Health": s.Providers}})
}

// configuration — GET /configuration: global provider configuration (no
// secrets).
func (c *Console) configuration(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Providers []readmodel.ProviderInfo `json:"providers"`
	}
	c.d.API.Get(r, "/api/v1/providers", &p)
	c.render(w, r, http.StatusOK, "configuration", viewmodels.Page{Shell: c.shell(r, "Configuration", "Configuration"), Data: p.Providers})
}
