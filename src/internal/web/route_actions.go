package web

import (
	"strings"

	"overseer/internal/access"
)

// apiActions is the authorization / audit action of every state-changing
// API route (WBS 17.128, 17.106). GET / HEAD routes are READ. Registering
// an unsafe route that is not listed here fails (P17-05), so a new
// mutation endpoint cannot bypass authorization or audit.
var apiActions = map[string]access.Action{
	// Projects (Phase 3).
	"POST /api/v1/projects":              access.ActionProjectCreate,
	"POST /api/v1/console/projects":      access.ActionProjectCreate,
	"PATCH /api/v1/projects/{id}":        access.ActionProjectUpdate,
	"DELETE /api/v1/projects/{id}":       access.ActionProjectDelete,
	"PUT /api/v1/projects/{id}/settings": access.ActionConfigEdit,
	"POST /api/v1/projects/{id}/ready":   access.ActionProjectUpdate,
	// Audit (Phase 18): acknowledging a verification failure (decision U9).
	"POST /api/v1/audit/verify/acknowledge": access.ActionAcknowledgeAuditFailure,
	// Restore (Phase 18): enabling execution on a restored database (U4).
	"POST /api/v1/system/restore/enable": access.ActionRestoreEnable,
	"POST /api/v1/system/gc":             access.ActionMaintenance,
	// Lifecycle (Phase 3 / 15 / 16).
	"POST /api/v1/projects/{id}/start":                                        access.ActionLifecycleStart,
	"POST /api/v1/projects/{id}/pause":                                        access.ActionLifecyclePause,
	"POST /api/v1/projects/{id}/resume":                                       access.ActionLifecycleResume,
	"POST /api/v1/projects/{id}/cancel":                                       access.ActionLifecycleCancel,
	"POST /api/v1/projects/{project}/bootstrap":                               access.ActionLifecycleStart,
	"POST /api/v1/projects/{project}/executions/{execution}/pause":            access.ActionLifecyclePause,
	"POST /api/v1/projects/{project}/executions/{execution}/resume":           access.ActionLifecycleResume,
	"POST /api/v1/projects/{project}/executions/{execution}/cancel":           access.ActionLifecycleCancel,
	"POST /api/v1/projects/{project}/executions/{execution}/step":             access.ActionEngineStep,
	"POST /api/v1/projects/{project}/executions/{execution}/run":              access.ActionEngineStep,
	"POST /api/v1/projects/{project}/executions/{execution}/recovery/resolve": access.ActionRecoveryAction,
	"POST /api/v1/projects/{project}/executions/{execution}/recovery/actions": access.ActionRecoveryAction,
	"POST /api/v1/projects/{project}/continuations/{continuation}/override":   access.ActionRecoveryAction,
	"POST /api/v1/projects/{project}/continuations/{continuation}/retry":      access.ActionRecoveryAction,
	// Phases / WBS (Phase 2 / 4).
	"POST /api/v1/projects/{id}/phases":              access.ActionPhaseAppend,
	"PATCH /api/v1/projects/{id}/phases/{phaseID}":   access.ActionPhaseUpdate,
	"DELETE /api/v1/projects/{id}/phases/{phaseID}":  access.ActionPhaseUpdate,
	"PUT /api/v1/projects/{id}/phases/order":         access.ActionPhaseUpdate,
	"PUT /api/v1/projects/{id}/phases/{phaseID}/wbs": access.ActionWBSEdit,
	// Roles / instructions (Phase 5).
	"POST /api/v1/projects/{project}/roles":                                     access.ActionRoleEdit,
	"PATCH /api/v1/projects/{project}/roles/{roleID}":                           access.ActionRoleEdit,
	"DELETE /api/v1/projects/{project}/roles/{roleID}":                          access.ActionRoleEdit,
	"PUT /api/v1/projects/{project}/roles/{roleID}/system-prompt":               access.ActionRoleEdit,
	"PUT /api/v1/projects/{project}/roles/{role}/state-files":                   access.ActionRoleEdit,
	"PUT /api/v1/projects/{project}/roles/{role}/states":                        access.ActionRoleEdit,
	"POST /api/v1/projects/{project}/roles/{role}/instructions":                 access.ActionRoleEdit,
	"DELETE /api/v1/projects/{project}/roles/{role}/instructions/{instruction}": access.ActionRoleEdit,
	"POST /api/v1/projects/{project}/roles/{role}/reset":                        access.ActionRoleEdit,
	"POST /api/v1/projects/{project}/roles/{role}/set-default":                  access.ActionRoleEdit,
	"POST /api/v1/projects/{project}/instructions":                              access.ActionRoleEdit,
	"PUT /api/v1/projects/{project}/instructions/{instructionID}":               access.ActionRoleEdit,
	"DELETE /api/v1/projects/{project}/instructions/{instructionID}":            access.ActionRoleEdit,
	// Workflow (Phase 6).
	"PUT /api/v1/projects/{project}/state-machine":                              access.ActionWorkflowEdit,
	"POST /api/v1/projects/{project}/state-machine/states":                      access.ActionWorkflowEdit,
	"PUT /api/v1/projects/{project}/state-machine/states/{key}":                 access.ActionWorkflowEdit,
	"DELETE /api/v1/projects/{project}/state-machine/states/{key}":              access.ActionWorkflowEdit,
	"PUT /api/v1/projects/{project}/state-machine/states/{key}/instructions":    access.ActionWorkflowEdit,
	"PUT /api/v1/projects/{project}/state-machine/states/{key}/prompt-template": access.ActionWorkflowEdit,
	"PUT /api/v1/projects/{project}/state-machine/states/{key}/state-files":     access.ActionWorkflowEdit,
	"POST /api/v1/projects/{project}/state-machine/states/{key}/roles/{role}":   access.ActionWorkflowEdit,
	"POST /api/v1/projects/{project}/state-machine/transitions":                 access.ActionWorkflowEdit,
	"PUT /api/v1/projects/{project}/state-machine/transitions/{id}":             access.ActionWorkflowEdit,
	"PUT /api/v1/projects/{project}/state-machine/transitions/{id}/condition":   access.ActionWorkflowEdit,
	"PUT /api/v1/projects/{project}/state-machine/transitions/{id}/priority":    access.ActionWorkflowEdit,
	"POST /api/v1/projects/{project}/state-machine/transitions/{id}/disable":    access.ActionWorkflowEdit,
	"POST /api/v1/projects/{project}/transitions/preview":                       access.ActionPreview,
	// State files (Phase 7).
	"PUT /api/v1/projects/{project}/state-files/{file}":                access.ActionStateFileEdit,
	"POST /api/v1/projects/{project}/state-files/materialize":          access.ActionStateFileEdit,
	"POST /api/v1/projects/{project}/state-files/{file}/drift/restore": access.ActionDriftResolve,
	"POST /api/v1/projects/{project}/state-files/{file}/drift/import":  access.ActionDriftResolve,
	"POST /api/v1/projects/{project}/state-files/{file}/drift/ignore":  access.ActionDriftResolve,
	// Prompts / templates / compression / cache / precompute (Phases 8–11).
	"POST /api/v1/projects/{project}/prompts/preview":                      access.ActionPreview,
	"POST /api/v1/projects/{project}/prompts/{prompt}/compression-preview": access.ActionPreview,
	"POST /api/v1/projects/{project}/prompts/{prompt}/worker-prompt":       access.ActionMaintenance,
	"POST /api/v1/projects/{project}/prompt-templates/{template}/versions": access.ActionTemplateEdit,
	"PUT /api/v1/projects/{project}/settings/compression":                  access.ActionConfigEdit,
	"POST /api/v1/projects/{project}/prompt-cache/explain":                 access.ActionPreview,
	"POST /api/v1/projects/{project}/prompt-cache/gc":                      access.ActionMaintenance,
	"POST /api/v1/projects/{project}/prompt-cache/integrity-scan":          access.ActionMaintenance,
	"POST /api/v1/projects/{project}/prompt-cache/invalidate":              access.ActionMaintenance,
	"POST /api/v1/projects/{project}/prompt-cache/{entry}/invalidate":      access.ActionMaintenance,
	"PUT /api/v1/projects/{project}/settings/prompt-cache":                 access.ActionConfigEdit,
	"POST /api/v1/projects/{project}/worker-prompts/prepare":               access.ActionMaintenance,
	"POST /api/v1/projects/{project}/worker-prompts/prepare-next":          access.ActionMaintenance,
	"POST /api/v1/projects/{project}/precompute":                           access.ActionMaintenance,
	"PUT /api/v1/projects/{project}/settings/precompute":                   access.ActionConfigEdit,
	// Worker / results / continuation / execution / engine settings
	// (Phases 12–16).
	"POST /api/v1/projects/{project}/worker/session/new":                    access.ActionWorkerOperate,
	"POST /api/v1/providers/{kind}/{name}/test":                             access.ActionWorkerOperate,
	"PUT /api/v1/projects/{project}/worker/session/cwd":                     access.ActionWorkerOperate,
	"PUT /api/v1/projects/{project}/settings/worker":                        access.ActionConfigEdit,
	"POST /api/v1/projects/{project}/worker/dispatches/{dispatch}/reparse":  access.ActionMaintenance,
	"PUT /api/v1/projects/{project}/settings/result-protocol":               access.ActionConfigEdit,
	"POST /api/v1/projects/{project}/settings/result-protocol/instructions": access.ActionPreview,
	"PUT /api/v1/projects/{project}/settings/continuation":                  access.ActionConfigEdit,
	"PUT /api/v1/projects/{project}/settings/execution":                     access.ActionConfigEdit,
	"PUT /api/v1/projects/{project}/settings/engine":                        access.ActionConfigEdit,
	"PUT /api/v1/projects/{project}/settings/planning":                      access.ActionConfigEdit,
}

// actionFor returns the action of a route: READ for safe methods, the
// table entry for unsafe ones (ok=false when missing).
func actionFor(method, pattern string) (access.Action, bool) {
	switch method {
	case "GET", "HEAD", "OPTIONS":
		return access.ActionRead, true
	}
	a, ok := apiActions[method+" "+pattern]
	return a, ok
}

// RouteActions returns a copy of the unsafe-route action table (audit,
// tests).
func RouteActions() map[string]access.Action {
	out := make(map[string]access.Action, len(apiActions))
	for k, v := range apiActions {
		out[k] = v
	}
	return out
}

// resourceFor derives the authorization resource from a route pattern and
// request path values: the project scope and the innermost resource id.
func resourceFor(pattern string, pathValue func(string) string) access.Resource {
	segs := strings.Split(strings.Trim(pattern, "/"), "/")
	res := access.Resource{}
	for i, s := range segs {
		if !strings.HasPrefix(s, "{") {
			if i >= 2 {
				res.Kind = s
			}
			continue
		}
		name := strings.Trim(s, "{}")
		v := pathValue(name)
		if i == 3 && (name == "id" || name == "project") {
			res.ProjectID = v
			res.Kind = "project"
		}
		res.ID = v
	}
	if res.Kind == "" {
		res.Kind = "global"
	}
	return res
}
