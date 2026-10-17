package web

// Phase 17 console API (WBS 17.66–17.71, 17.75–17.79, 17.98, 17.103):
//
//	GET  /api/v1/dashboard
//	GET  /api/v1/console/projects                                   (filters + pagination)
//	POST /api/v1/console/projects                                   (create + configure)
//	GET  /api/v1/projects/{project}/overview
//	GET  /api/v1/projects/{project}/console/phases[/{phase}[/work-units|/edges]]
//	GET  /api/v1/projects/{project}/console/work-units/{unit}
//	GET  /api/v1/projects/{project}/console/{flows,worker,dispatches,continuations,repository,fix-attempts,metrics}
//	GET  /api/v1/projects/{project}/console/results/{result}
//	GET  /api/v1/projects/{project}/console/validations[/{run}]
//	GET  /api/v1/projects/{project}/console/checkpoints[/{checkpoint}]
//	GET  /api/v1/projects/{project}/console/executions/{execution}/timeline
//	GET  /api/v1/projects/{project}/validation-results/{result}/output
//	GET  /api/v1/projects/{project}/executions/{execution}/recovery
//	POST /api/v1/projects/{project}/executions/{execution}/recovery/actions
//	GET  /api/v1/projects/{project}/events/stream                   (SSE)
//	GET  /api/v1/audit, /api/v1/audit/{event}
//	GET  /api/v1/projects/{project}/state-files/{file}/drift-status
//	POST /api/v1/projects/{project}/state-files/{file}/drift/{restore,import,ignore}
//	GET  /api/v1/projects/{project}/prompt-templates[/{template}[/versions]]
//	POST /api/v1/projects/{project}/prompt-templates/{template}/versions
//	GET  /api/v1/projects/{project}/settings/planning, PUT (same)
//	GET  /api/v1/system/status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/access"
	"overseer/internal/audit"
	"overseer/internal/persistence"
	"overseer/internal/readmodel"
	"overseer/internal/service"
	"overseer/internal/wbs"
	"overseer/internal/web/middleware"
	"overseer/internal/worker"
)

// ClassifyError maps service / repository errors onto the API error model
// (WBS 17.131–17.135). Page handlers use it too.
func ClassifyError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := middleware.AsRejection(err); ok {
		return err
	}
	var ae *appError
	if errors.As(err, &ae) {
		return err
	}
	var tv *service.TemplateValidationError
	if errors.As(err, &tv) {
		return NewValidationFields("invalid_template", "Template content is invalid.", map[string]string{"content": strings.Join(tv.Issues, "; ")})
	}
	if f := service.ErrFieldsOf(err); f != nil {
		return NewValidationFields(CodeInvalidInput, "Request is invalid.", f)
	}
	switch {
	case errors.Is(err, service.ErrActionNotAvailable):
		return NewConflictCode("ACTION_NOT_AVAILABLE", "The requested action is not available in the current state.")
	case errors.Is(err, service.ErrDerivedNotImportable):
		return NewConflictCode("DERIVED_NOT_IMPORTABLE", "A derived state file cannot import filesystem content.")
	case errors.Is(err, service.ErrInvalidInput):
		msg := "Request is invalid."
		if s := strings.TrimPrefix(err.Error(), "service: invalid input: "); s != err.Error() {
			msg = s
		}
		return NewValidationCode(CodeInvalidInput, msg)
	case errors.Is(err, service.ErrNotFound), errors.Is(err, persistence.ErrNotFound), errors.Is(err, service.ErrInvalidReference):
		return NewNotFoundError("resource")
	case errors.Is(err, service.ErrConflict), errors.Is(err, persistence.ErrConflict):
		msg := "The resource changed or is in a conflicting state; reload and retry."
		if s := strings.TrimPrefix(err.Error(), "service: conflict: "); s != err.Error() {
			msg = s
		}
		return NewConflictCode(CodeConflict, msg)
	}
	return NewInternalError("request failed")
}

// ConsoleAPI bundles the Phase 17 services.
type ConsoleAPI struct {
	Console   *service.ConsoleReadService
	Feed      *service.LiveEventFeed
	Audit     *service.AuditService
	Recovery  *service.RecoveryActionService
	Drift     *service.DriftResolutionService
	Templates *service.PromptTemplateService
	Planning  *service.PlanningSettingsService
	Setup     *service.ProjectSetupService
	Metrics   *service.MetricsService
	System    *service.SystemStatusService
	// ConfigDrift reports configuration drift of active executions
	// (WBS 18.4).
	ConfigDrift *service.ConfigDriftService
	// Integrity runs the read-only invariant scan (WBS 18.39).
	Integrity *service.IntegrityService
	// AuditVerify verifies the audit hash chains (WBS 18.108–18.110).
	AuditVerify *service.AuditVerificationService
	// Restore is the restored-database startup gate (WBS 18.46).
	Restore *service.RestoreGateService
	// Health reports per-subsystem health (WBS 18.117).
	Health *service.SystemHealthService
	// Alerts evaluates the alert conditions (WBS 18.118).
	Alerts *service.AlertService
	// GC runs the garbage collection on demand (decision U1).
	GC *service.GCService
	// Lineage reads a work unit's end-to-end lineage (WBS 18.205).
	Lineage *service.LineageService
	// Diagnostics assembles diagnostic bundles (WBS 18.185–18.187).
	Diagnostics *service.DiagnosticsService
	// Readiness is the production readiness report (WBS 18.207).
	Readiness *service.ProductionReadinessService
	// Providers lists the configured providers (secret-free).
	Providers func() []readmodel.ProviderInfo
	// ProviderModels lists the models a configured provider serves (kind
	// "worker" / "compression"); found=false for an unknown provider.
	ProviderModels func(ctx context.Context, kind, name string) (models []string, found bool, err error)
	// ProviderTest sends the connection-test prompt to a configured
	// provider's model; found=false for an unknown provider.
	ProviderTest func(ctx context.Context, kind, name, model string) (res readmodel.ProviderTest, found bool)
	// Workers / Roles back the worker-policy and role-reset endpoints.
	Workers *service.WorkerService
	Roles   *service.RoleService
	// Heartbeat is the SSE keep-alive interval (default 15 s).
	Heartbeat time.Duration
}

// RegisterConsoleRoutes registers the console API.
func RegisterConsoleRoutes(r *Router, a *ConsoleAPI) error {
	p := "/api/v1/projects/{project}"
	c := p + "/console"
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/dashboard", a.dashboard},
		{http.MethodGet, "/api/v1/console/projects", a.projects},
		{http.MethodPost, "/api/v1/console/projects", a.createProject},
		{http.MethodGet, p + "/overview", a.overview},
		{http.MethodGet, p + "/config-drift", a.configDrift},
		{http.MethodGet, c + "/phases", a.phases},
		{http.MethodGet, c + "/phases/{phase}", a.phase},
		{http.MethodGet, c + "/phases/{phase}/work-units", a.units},
		{http.MethodGet, c + "/phases/{phase}/edges", a.edges},
		{http.MethodGet, c + "/work-units/{unit}", a.unit},
		{http.MethodGet, c + "/flows", a.flows},
		{http.MethodGet, c + "/worker", a.worker},
		{http.MethodGet, c + "/dispatches", a.dispatches},
		{http.MethodGet, c + "/results/{result}", a.result},
		{http.MethodGet, c + "/continuations", a.continuations},
		{http.MethodGet, c + "/validations", a.validations},
		{http.MethodGet, c + "/validations/{run}", a.validation},
		{http.MethodGet, c + "/checkpoints", a.checkpoints},
		{http.MethodGet, c + "/checkpoints/{checkpoint}", a.checkpoint},
		{http.MethodGet, c + "/repository", a.repository},
		{http.MethodGet, c + "/fix-attempts", a.fixAttempts},
		{http.MethodGet, c + "/metrics", a.metrics},
		{http.MethodGet, c + "/executions/{execution}/timeline", a.timeline},
		{http.MethodGet, p + "/validation-results/{result}/output", a.output},
		{http.MethodGet, p + "/executions/{execution}/recovery", a.recovery},
		{http.MethodPost, p + "/executions/{execution}/recovery/actions", a.recoveryAction},
		{http.MethodGet, p + "/executions/{execution}/recovery/plan", a.recoveryPlan},
		{http.MethodGet, p + "/events/stream", a.stream},
		{http.MethodGet, "/api/v1/audit", a.auditList},
		{http.MethodGet, "/api/v1/audit/verify", a.auditVerify},
		{http.MethodPost, "/api/v1/audit/verify/acknowledge", a.auditAcknowledge},
		{http.MethodGet, "/api/v1/audit/{event}", a.auditGet},
		{http.MethodGet, p + "/state-files/{file}/drift-status", a.driftStatus},
		{http.MethodPost, p + "/state-files/{file}/drift/restore", a.drift(service.DriftRestore)},
		{http.MethodPost, p + "/state-files/{file}/drift/import", a.drift(service.DriftImport)},
		{http.MethodPost, p + "/state-files/{file}/drift/ignore", a.drift(service.DriftIgnore)},
		{http.MethodGet, p + "/prompt-templates", a.templates},
		{http.MethodGet, p + "/prompt-templates/{template}", a.template},
		{http.MethodGet, p + "/prompt-templates/{template}/versions", a.templateVersions},
		{http.MethodPost, p + "/prompt-templates/{template}/versions", a.templateCreateVersion},
		{http.MethodGet, p + "/settings/planning", a.planningGet},
		{http.MethodPut, p + "/settings/planning", a.planningPut},
		{http.MethodGet, "/api/v1/system/status", a.systemStatus},
		{http.MethodGet, "/api/v1/system/integrity", a.integrity},
		{http.MethodGet, "/api/v1/system/restore", a.restoreStatus},
		{http.MethodGet, "/api/v1/system/health", a.systemHealth},
		{http.MethodGet, "/api/v1/system/alerts", a.systemAlerts},
		{http.MethodGet, "/api/v1/projects/{project}/work-units/{unit}/lineage", a.unitLineage},
		{http.MethodGet, "/api/v1/system/diagnostics", a.systemDiagnostics},
		{http.MethodGet, "/api/v1/system/readiness", a.productionReadiness},
		{http.MethodGet, "/api/v1/projects/{project}/diagnostics", a.projectDiagnostics},
		{http.MethodPost, "/api/v1/system/gc", a.runGC},
		{http.MethodPost, "/api/v1/system/restore/enable", a.restoreEnable},
		{http.MethodGet, "/api/v1/providers", a.providers},
		{http.MethodGet, "/api/v1/providers/{kind}/{name}/models", a.providerModels},
		{http.MethodPost, "/api/v1/providers/{kind}/{name}/test", a.providerTest},
		{http.MethodGet, p + "/settings/worker", a.workerSettingsGet},
		{http.MethodPut, p + "/settings/worker", a.workerSettingsPut},
		{http.MethodPost, p + "/roles/{role}/reset", a.roleReset},
		{http.MethodPost, p + "/roles/{role}/set-default", a.roleSetDefault},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func pathID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, NewNotFoundError("resource")
	}
	return id, nil
}

func pathIDs(r *http.Request, names ...string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(names))
	for _, n := range names {
		id, err := pathID(r, n)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func queryInt(r *http.Request, name string, def int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, NewValidationFields(CodeInvalidInput, "Query is invalid.", map[string]string{name: "must be an integer"})
	}
	return n, nil
}

func queryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, NewValidationFields(CodeInvalidInput, "Query is invalid.", map[string]string{name: "must be a UUID"})
	}
	return &id, nil
}

// respond writes v or the classified error.
func respond(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *ConsoleAPI) dashboard(w http.ResponseWriter, r *http.Request) {
	d, err := a.Console.Dashboard(r.Context())
	respond(w, r, d, err)
}

// ProjectQueryFrom parses the list query parameters (WBS 17.6).
func ProjectQueryFrom(r *http.Request) (service.ProjectQuery, error) {
	limit, err := queryInt(r, "limit", 25)
	if err != nil {
		return service.ProjectQuery{}, err
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		return service.ProjectQuery{}, err
	}
	q := r.URL.Query()
	return service.ProjectQuery{Status: q.Get("status"), Name: q.Get("name"), Active: q.Get("active"),
		Recovery: q.Get("recovery") == "true" || q.Get("recovery") == "1" || q.Get("recovery") == "on", Limit: limit, Offset: offset}, nil
}

func (a *ConsoleAPI) projects(w http.ResponseWriter, r *http.Request) {
	q, err := ProjectQueryFrom(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Console.Projects(r.Context(), q)
	respond(w, r, l, err)
}

var setupFields = map[string]bool{"name": true, "description": true, "working_directory": true, "worker_provider": true,
	"worker_model": true, "compression_provider": true, "compression_model": true, "context_window": true,
	"context_threshold": true, "planning": true, "checkpoint_policy": true, "use_git": true}

func (a *ConsoleAPI) createProject(w http.ResponseWriter, r *http.Request) {
	var in service.ProjectSetup
	if err := decodeStrictJSON(r, "invalid_project", setupFields, []string{"name"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := a.Setup.Create(r.Context(), in)
	if err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	w.Header().Set("Location", "/api/v1/projects/"+p.ID.String())
	writeJSON(w, http.StatusCreated, toResource(p))
}

func (a *ConsoleAPI) configDrift(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if a.ConfigDrift == nil {
		writeJSON(w, http.StatusOK, readmodel.ConfigDrift{Items: []readmodel.DriftItem{}})
		return
	}
	d, err := a.ConfigDrift.ForProject(r.Context(), pid)
	respond(w, r, d, err)
}

func (a *ConsoleAPI) overview(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	o, err := a.Console.Overview(r.Context(), pid)
	respond(w, r, o, err)
}

func (a *ConsoleAPI) phases(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Console.Phases(r.Context(), pid)
	respond(w, r, map[string]any{"phases": l}, err)
}

func (a *ConsoleAPI) phase(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "phase")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := a.Console.Phase(r.Context(), ids[0], ids[1])
	respond(w, r, d, err)
}

// UnitQueryFrom parses the work-unit filters (WBS 17.26).
func UnitQueryFrom(r *http.Request) (service.UnitQuery, error) {
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		return service.UnitQuery{}, err
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		return service.UnitQuery{}, err
	}
	q := r.URL.Query()
	return service.UnitQuery{Status: q.Get("status"), Kind: q.Get("kind"), Readiness: q.Get("readiness"), Validation: q.Get("validation"),
		Limit: limit, Offset: offset}, nil
}

func (a *ConsoleAPI) units(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "phase")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q, err := UnitQueryFrom(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := a.Console.Units(r.Context(), ids[0], ids[1], q)
	respond(w, r, p, err)
}

func (a *ConsoleAPI) edges(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "phase")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit", 500)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	edges, page, err := a.Console.Edges(r.Context(), ids[0], ids[1], limit, offset)
	respond(w, r, map[string]any{"edges": edges, "page": page}, err)
}

func (a *ConsoleAPI) unit(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "unit")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := a.Console.Unit(r.Context(), ids[0], ids[1])
	respond(w, r, d, err)
}

func (a *ConsoleAPI) flows(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	f, err := a.Console.Flows(r.Context(), pid)
	respond(w, r, map[string]any{"flows": f}, err)
}

func (a *ConsoleAPI) worker(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := a.Console.WorkerPanel(r.Context(), pid)
	respond(w, r, p, err)
}

func (a *ConsoleAPI) dispatches(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows, page, err := a.Console.Dispatches(r.Context(), pid, limit, offset)
	respond(w, r, map[string]any{"dispatches": rows, "page": page}, err)
}

func (a *ConsoleAPI) result(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "result")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Console.Result(r.Context(), ids[0], ids[1])
	respond(w, r, res, err)
}

func (a *ConsoleAPI) continuations(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Console.ContinuationList(r.Context(), pid, 100)
	respond(w, r, map[string]any{"continuations": l}, err)
}

func (a *ConsoleAPI) validations(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	exec, err := queryUUID(r, "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	unit, err := queryUUID(r, "unit")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Console.ValidationRuns(r.Context(), pid, exec, unit, 200)
	respond(w, r, map[string]any{"validations": l}, err)
}

func (a *ConsoleAPI) validation(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "run")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := a.Console.Validation(r.Context(), ids[0], ids[1])
	respond(w, r, d, err)
}

func (a *ConsoleAPI) output(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "result")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	length, err := queryInt(r, "length", 16384)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "stdout"
	}
	o, err := a.Console.Output(r.Context(), ids[0], ids[1], stream, offset, length)
	if err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stream": o.Stream, "offset": o.Offset, "text": o.Text, "total": o.Total,
		"next_offset": o.Offset + len([]rune(o.Text)), "complete": o.Offset+len([]rune(o.Text)) >= o.Total})
}

func (a *ConsoleAPI) checkpoints(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	exec, err := queryUUID(r, "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Console.Checkpoints(r.Context(), pid, exec, 200)
	respond(w, r, map[string]any{"checkpoints": l}, err)
}

func (a *ConsoleAPI) checkpoint(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "checkpoint")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := a.Console.Checkpoint(r.Context(), ids[0], ids[1])
	respond(w, r, c, err)
}

func (a *ConsoleAPI) repository(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.Console.Repository(r.Context(), pid)
	respond(w, r, s, err)
}

func (a *ConsoleAPI) fixAttempts(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	unit, err := queryUUID(r, "unit")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Console.FixAttempts(r.Context(), pid, nil, unit)
	respond(w, r, map[string]any{"fix_attempts": l}, err)
}

func (a *ConsoleAPI) metrics(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	m, err := a.Metrics.Project(r.Context(), pid)
	respond(w, r, m, err)
}

// TimelineQueryFrom parses the timeline query (WBS 17.81 / 17.151).
func TimelineQueryFrom(r *http.Request) (service.TimelineQuery, error) {
	before, err := queryInt(r, "before", 0)
	if err != nil {
		return service.TimelineQuery{}, err
	}
	limit, err := queryInt(r, "limit", 100)
	if err != nil {
		return service.TimelineQuery{}, err
	}
	var cats []string
	for _, v := range r.URL.Query()["category"] {
		cats = append(cats, strings.Split(v, ",")...)
	}
	return service.TimelineQuery{Categories: cats, Before: int64(before), Limit: limit}, nil
}

func (a *ConsoleAPI) timeline(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q, err := TimelineQueryFrom(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Console.Timeline(r.Context(), ids[0], ids[1], q)
	respond(w, r, t, err)
}

func (a *ConsoleAPI) recovery(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rec, err := a.Console.Recovery(r.Context(), ids[0], ids[1])
	respond(w, r, rec, err)
}

var recoveryActionFields = map[string]bool{"action": true, "reason": true, "confirm": true, "check_indexes": true,
	"idempotency_key": true, "evidence_hash": true, "dry_run": true, "resolution": true}

func (a *ConsoleAPI) recoveryAction(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req service.RecoveryRequest
	if err := decodeStrictJSON(r, "invalid_recovery_action", recoveryActionFields, []string{"action"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p := middleware.PrincipalFromContext(r.Context())
	req.By, req.ByName, req.CorrelationID = p.ID, p.Name, RequestIDFromContext(r.Context())
	out, err := a.Recovery.Perform(r.Context(), ids[0], ids[1], req)
	// Operator recovery audit (WBS 18.182): action, evidence viewed,
	// idempotency identity and result travel with the audit row.
	AddAuditDetail(r.Context(), "recovery_action", string(req.Action))
	AddAuditDetail(r.Context(), "evidence_hash", req.EvidenceHash)
	AddAuditDetail(r.Context(), "idempotency_key", req.IdempotencyKey)
	AddAuditDetail(r.Context(), "dry_run", req.DryRun)
	if out != nil {
		AddAuditDetail(r.Context(), "plan_hash", out.PlanHash)
		AddAuditDetail(r.Context(), "result_execution_status", out.ExecutionStatus)
		AddAuditDetail(r.Context(), "replayed", out.Replayed)
	}
	if err != nil {
		AddAuditDetail(r.Context(), "error", err.Error())
	}
	respond(w, r, out, err)
}

// recoveryPlan — GET .../executions/{execution}/recovery/plan: the
// deterministic recovery plan (read-only, WBS 18.8 / 18.10) plus the
// recorded plans and actions.
func (a *ConsoleAPI) recoveryPlan(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "execution")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Recovery.Plans(r.Context(), ids[0], ids[1])
	respond(w, r, v, err)
}

// ---------------------------------------------------------------------------
// SSE (P17-66).

// LastEventID resolves the resume cursor: the Last-Event-ID header (sent
// by EventSource on reconnect) or ?after.
func LastEventID(r *http.Request) int64 {
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")} {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

func (a *ConsoleAPI) stream(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if a.Feed == nil {
		WriteError(w, r, NewInternalError("live updates unavailable"))
		return
	}
	if _, err := a.Console.Overview(r.Context(), pid); err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	after := LastEventID(r)
	sub, err := a.Feed.Subscribe(r.Context(), pid, after)
	if err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	defer sub.Close()
	rc := http.NewResponseController(w)
	// The stream outlives the server's WriteTimeout (per-route deadline).
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 3000\n: connected after=%d\n\n", after)
	_ = rc.Flush()
	hb := a.Heartbeat
	if hb <= 0 {
		hb = 15 * time.Second
	}
	tick := time.NewTicker(hb)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case e, ok := <-sub.C:
			if !ok {
				fmt.Fprintf(w, "event: reconnect\ndata: {\"reason\":%q}\n\n", sub.Reason())
				_ = rc.Flush()
				return
			}
			if err := WriteSSE(w, e); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

// WriteSSE writes one minimized event frame (id = project_seq).
func WriteSSE(w interface{ Write([]byte) (int, error) }, e readmodel.LiveEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: execution\ndata: %s\n\n", e.ProjectSeq, b)
	return err
}

// ---------------------------------------------------------------------------
// Audit (P17-67).

// AuditFilterFrom parses audit filters (WBS 17.104).
func AuditFilterFrom(r *http.Request) (audit.Filter, error) {
	q := r.URL.Query()
	var f audit.Filter
	fields := map[string]string{}
	for name, dst := range map[string]**uuid.UUID{"project": &f.ProjectID, "execution": &f.ExecutionID, "phase": &f.PhaseID, "unit": &f.WorkUnitID} {
		if v := q.Get(name); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				fields[name] = "must be a UUID"
				continue
			}
			*dst = &id
		}
	}
	f.Action = access.Action(q.Get("action"))
	f.Result = audit.Result(q.Get("result"))
	for name, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				if t2, err2 := time.Parse(time.RFC3339, v); err2 == nil {
					t = t2
				} else {
					fields[name] = "must be YYYY-MM-DD or RFC 3339"
					continue
				}
			} else if name == "to" {
				t = t.Add(24 * time.Hour)
			}
			*dst = &t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fields["limit"] = "must be an integer"
		}
		f.Limit = n
	}
	seq, err := audit.DecodeCursor(q.Get("cursor"))
	if err != nil {
		fields["cursor"] = "invalid cursor"
	}
	f.BeforeSeq = seq
	if len(fields) > 0 {
		return f, NewValidationFields(CodeInvalidInput, "Query is invalid.", fields)
	}
	if err := f.Validate(); err != nil {
		return f, NewValidationCode(CodeInvalidInput, err.Error())
	}
	return f, nil
}

func (a *ConsoleAPI) auditList(w http.ResponseWriter, r *http.Request) {
	f, err := AuditFilterFrom(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := a.Audit.List(r.Context(), f)
	respond(w, r, p, err)
}

func (a *ConsoleAPI) auditGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "event")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	e, err := a.Audit.Get(r.Context(), id)
	respond(w, r, e, err)
}

// ---------------------------------------------------------------------------
// Drift (P17-69).

func (a *ConsoleAPI) driftStatus(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "file")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Drift.Status(r.Context(), ids[0], ids[1])
	respond(w, r, v, err)
}

var driftFields = map[string]bool{"reason": true, "confirm": true}

func (a *ConsoleAPI) drift(option string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids, err := pathIDs(r, "project", "file")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		var in struct {
			Reason  string `json:"reason"`
			Confirm bool   `json:"confirm"`
		}
		if err := decodeStrictOptionalJSON(r, "invalid_drift_resolution", driftFields, &in); err != nil {
			WriteError(w, r, err)
			return
		}
		if option == service.DriftImport && (!in.Confirm || strings.TrimSpace(in.Reason) == "") {
			WriteError(w, r, NewValidationFields(CodeInvalidInput, "Importing a drifted file requires confirmation and a reason.",
				map[string]string{"confirm": "required", "reason": "required"}))
			return
		}
		v, err := a.Drift.Resolve(r.Context(), ids[0], ids[1], option, middleware.PrincipalFromContext(r.Context()).ID, in.Reason)
		respond(w, r, v, err)
	}
}

// ---------------------------------------------------------------------------
// Prompt templates (P17-70).

func (a *ConsoleAPI) templates(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Templates.List(r.Context(), pid)
	respond(w, r, map[string]any{"templates": l}, err)
}

func (a *ConsoleAPI) template(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "template")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Templates.Get(r.Context(), ids[0], ids[1])
	respond(w, r, t, err)
}

func (a *ConsoleAPI) templateVersions(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "template")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Templates.Versions(r.Context(), ids[0], ids[1])
	respond(w, r, map[string]any{"versions": l}, err)
}

var templateVersionFields = map[string]bool{"content": true, "expected_version": true, "reason": true}

func (a *ConsoleAPI) templateCreateVersion(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "project", "template")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var in struct {
		Content         string `json:"content"`
		ExpectedVersion *int   `json:"expected_version"`
	}
	if err := decodeStrictJSON(r, "invalid_template", templateVersionFields, []string{"content", "expected_version"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Templates.CreateVersion(r.Context(), ids[0], ids[1], in.Content, *in.ExpectedVersion)
	if err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// ---------------------------------------------------------------------------
// Planning settings (WBS 17.61).

func (a *ConsoleAPI) planningGet(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	c, err := a.Planning.Get(r.Context(), pid)
	respond(w, r, c, err)
}

var planningFields = map[string]bool{"mode": true, "atomic": true, "prefer_small_functions": true, "custom_policy": true}

func (a *ConsoleAPI) planningPut(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var c wbs.PlanningConfiguration
	if err := decodeStrictJSON(r, "invalid_planning_settings", planningFields, []string{"mode"}, &c); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Planning.Update(r.Context(), pid, c)
	respond(w, r, out, err)
}

// integrity reports the read-only invariant scan (WBS 18.39 / 18.40):
// ?project= and ?execution= narrow the scope. Nothing is repaired.
func (a *ConsoleAPI) integrity(w http.ResponseWriter, r *http.Request) {
	if a.Integrity == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	var pid, eid *uuid.UUID
	for key, dst := range map[string]**uuid.UUID{"project": &pid, "execution": &eid} {
		if v := r.URL.Query().Get(key); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				WriteError(w, r, NewValidationError(key+" must be a UUID"))
				return
			}
			*dst = &id
		}
	}
	rep, err := a.Integrity.Scan(r.Context(), pid, eid)
	respond(w, r, rep, err)
}

// auditVerify verifies every chain or one project's (?project=) in a
// read-only snapshot (WBS 18.108–18.110).
func (a *ConsoleAPI) auditVerify(w http.ResponseWriter, r *http.Request) {
	if a.AuditVerify == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	var pid *uuid.UUID
	if v := r.URL.Query().Get("project"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			WriteError(w, r, NewValidationError("project must be a UUID"))
			return
		}
		pid = &id
	}
	v, err := a.AuditVerify.Verify(r.Context(), pid)
	respond(w, r, v, err)
}

var auditAckFields = map[string]bool{"reason": true, "fingerprint": true}

// auditAcknowledge records the operator's acknowledgement of the current
// audit verification failure (decision U9); the request is audited as
// ACKNOWLEDGE_AUDIT_FAILURE.
func (a *ConsoleAPI) auditAcknowledge(w http.ResponseWriter, r *http.Request) {
	if a.AuditVerify == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	var req service.AcknowledgeRequest
	if err := decodeStrictJSON(r, "invalid_audit_acknowledgement", auditAckFields, []string{"reason"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p := middleware.PrincipalFromContext(r.Context())
	req.By, req.ByName, req.RequestID = p.ID, p.Name, RequestIDFromContext(r.Context())
	out, err := a.AuditVerify.Acknowledge(r.Context(), req)
	if out != nil {
		AddAuditDetail(r.Context(), "fingerprint", out.Acknowledgement.Fingerprint)
		AddAuditDetail(r.Context(), "verification_status", string(out.Acknowledgement.Status))
	}
	respond(w, r, out, err)
}

// systemHealth reports per-subsystem health; it is always 200 — the
// body carries HEALTHY / DEGRADED / UNAVAILABLE (WBS 18.117).
func (a *ConsoleAPI) systemHealth(w http.ResponseWriter, r *http.Request) {
	if a.Health == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	respond(w, r, a.Health.Report(r.Context()), nil)
}

// systemAlerts evaluates every alert now (WBS 18.118).
func (a *ConsoleAPI) systemAlerts(w http.ResponseWriter, r *http.Request) {
	if a.Alerts == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	respond(w, r, map[string]any{"alerts": a.Alerts.Evaluate(r.Context())}, nil)
}

// unitLineage returns a work unit's lineage by ids and hashes (WBS 18.205).
func (a *ConsoleAPI) unitLineage(w http.ResponseWriter, r *http.Request) {
	if a.Lineage == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	ids, err := pathIDs(r, "project", "unit")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Lineage.UnitLineage(r.Context(), ids[0], ids[1])
	if errors.Is(err, persistence.ErrNotFound) {
		WriteError(w, r, NewNotFoundError("work unit"))
		return
	}
	respond(w, r, l, err)
}

// productionReadiness returns the production readiness report.
func (a *ConsoleAPI) productionReadiness(w http.ResponseWriter, r *http.Request) {
	if a.Readiness == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	respond(w, r, a.Readiness.Report(r.Context()), nil)
}

// systemDiagnostics returns the system diagnostic bundle.
func (a *ConsoleAPI) systemDiagnostics(w http.ResponseWriter, r *http.Request) {
	if a.Diagnostics == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	b, err := a.Diagnostics.System(r.Context())
	respond(w, r, b, err)
}

// projectDiagnostics returns one project's diagnostic bundle.
func (a *ConsoleAPI) projectDiagnostics(w http.ResponseWriter, r *http.Request) {
	if a.Diagnostics == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	ids, err := pathIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	b, err := a.Diagnostics.Project(r.Context(), ids[0])
	respond(w, r, b, err)
}

// runGC runs one bounded garbage collection (admin, on demand).
func (a *ConsoleAPI) runGC(w http.ResponseWriter, r *http.Request) {
	if a.GC == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	res, err := a.GC.Run(r.Context())
	respond(w, r, res, err)
}

// restoreStatus reports the restored-database gate and its check report.
func (a *ConsoleAPI) restoreStatus(w http.ResponseWriter, r *http.Request) {
	if a.Restore == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	st, err := a.Restore.Status(r.Context())
	respond(w, r, st, err)
}

var restoreEnableFields = map[string]bool{"reason": true}

// restoreEnable enables execution on a validated restore (decision U4);
// audited as RESTORE_ENABLE.
func (a *ConsoleAPI) restoreEnable(w http.ResponseWriter, r *http.Request) {
	if a.Restore == nil {
		WriteError(w, r, ErrNotFound)
		return
	}
	var req service.EnableRequest
	if err := decodeStrictJSON(r, "invalid_restore_enable", restoreEnableFields, []string{"reason"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	req.By = middleware.PrincipalFromContext(r.Context()).ID
	out, err := a.Restore.Enable(r.Context(), req)
	if out != nil && out.State != nil {
		AddAuditDetail(r.Context(), "backup_id", out.State.BackupID.String())
	}
	respond(w, r, out, err)
}

func (a *ConsoleAPI) systemStatus(w http.ResponseWriter, r *http.Request) {
	s, err := a.System.Status(r.Context())
	respond(w, r, s, err)
}

// ---------------------------------------------------------------------------
// Providers, worker policy, role reset.

func (a *ConsoleAPI) providers(w http.ResponseWriter, r *http.Request) {
	out := []readmodel.ProviderInfo{}
	if a.Providers != nil {
		out = a.Providers()
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// providerModels lists a provider's models (console model pickers). An
// unreachable provider is a 200 with available=false so the console can
// fall back to free-text entry.
func (a *ConsoleAPI) providerModels(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	if kind != "worker" && kind != "compression" {
		WriteError(w, r, NewValidationError("kind must be worker or compression"))
		return
	}
	if a.ProviderModels == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "models": []string{}})
		return
	}
	models, found, err := a.ProviderModels(r.Context(), kind, name)
	if !found {
		WriteError(w, r, NewNotFoundError("provider"))
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "models": []string{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "models": models})
}

// providerTest sends the connection-test prompt (body {"model": …}) to a
// configured provider. A provider that fails is a 200 with ok=false and the
// error, so the console can show it.
func (a *ConsoleAPI) providerTest(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	if kind != "worker" && kind != "compression" {
		WriteError(w, r, NewValidationError("kind must be worker or compression"))
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Model) == "" {
		WriteError(w, r, NewValidationFields(CodeInvalidInput, "Request is invalid.", map[string]string{"model": "required"}))
		return
	}
	if a.ProviderTest == nil {
		WriteError(w, r, NewNotFoundError("provider"))
		return
	}
	// The test may run up to 10 minutes — longer than the server's write
	// timeout — so the deadline is extended for this response.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(11 * time.Minute))
	res, found := a.ProviderTest(r.Context(), kind, name, strings.TrimSpace(body.Model))
	if !found {
		WriteError(w, r, NewNotFoundError("provider"))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type workerPolicyDTO struct {
	SessionPolicy  string `json:"session_policy"`
	ConcurrentSend string `json:"concurrent_send"`
	LeaseSeconds   int    `json:"lease_seconds"`
	WaitSeconds    int    `json:"wait_seconds"`
	Stored         bool   `json:"stored"`
}

func (a *ConsoleAPI) workerSettingsGet(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if _, err := a.Console.Overview(r.Context(), pid); err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	st, stored, err := a.Workers.WorkerSettings(r.Context(), pid)
	if err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	writeJSON(w, http.StatusOK, workerPolicyDTO{SessionPolicy: string(st.SessionPolicy), ConcurrentSend: string(st.ConcurrentSend),
		LeaseSeconds: int(st.Lease / time.Second), WaitSeconds: int(st.Wait / time.Second), Stored: stored})
}

var workerPolicyFields = map[string]bool{"session_policy": true, "concurrent_send": true, "lease_seconds": true, "wait_seconds": true}

func (a *ConsoleAPI) workerSettingsPut(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if _, err := a.Console.Overview(r.Context(), pid); err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	var in workerPolicyDTO
	if err := decodeStrictJSON(r, "invalid_worker_settings", workerPolicyFields, []string{"session_policy", "concurrent_send"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	cur, _, err := a.Workers.WorkerSettings(r.Context(), pid)
	if err != nil {
		WriteError(w, r, ClassifyError(err))
		return
	}
	cur.SessionPolicy, cur.ConcurrentSend = worker.SessionPolicy(in.SessionPolicy), worker.ConcurrentSendPolicy(in.ConcurrentSend)
	if in.LeaseSeconds > 0 {
		cur.Lease = time.Duration(in.LeaseSeconds) * time.Second
	}
	if in.WaitSeconds >= 0 {
		cur.Wait = time.Duration(in.WaitSeconds) * time.Second
	}
	if err := a.Workers.UpdateWorkerSettings(r.Context(), pid, cur); err != nil {
		WriteError(w, r, workerError(err))
		return
	}
	a.workerSettingsGet(w, r)
}

// roleSetDefault overwrites the role key's GLOBAL default with the project
// role's current name, description and system prompt; it returns the
// updated global default.
func (a *ConsoleAPI) roleSetDefault(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rec, err := a.Roles.GetRole(r.Context(), r.PathValue("role"))
	if err != nil || rec.ProjectID == nil || *rec.ProjectID != pid {
		WriteError(w, r, NewNotFoundError("role"))
		return
	}
	out, err := a.Roles.SetRoleAsDefault(r.Context(), rec.ID.String())
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toRoleResource(out))
}

func (a *ConsoleAPI) roleReset(w http.ResponseWriter, r *http.Request) {
	pid, err := pathID(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rec, err := a.Roles.GetRole(r.Context(), r.PathValue("role"))
	if err != nil || rec.ProjectID == nil || *rec.ProjectID != pid {
		WriteError(w, r, NewNotFoundError("role"))
		return
	}
	out, err := a.Roles.ResetRole(r.Context(), rec.ID.String())
	if err != nil {
		WriteError(w, r, roleServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toRoleResource(out))
}
