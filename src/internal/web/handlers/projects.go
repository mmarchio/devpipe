package handlers

import (
	"net/http"
	"net/url"
	"strings"

	"overseer/internal/readmodel"
	"overseer/internal/wbs"
	"overseer/internal/web/viewmodels"
)

var projectStatuses = []string{"DRAFT", "READY", "RUNNING", "PAUSED", "COMPLETED", "FAILED", "CANCELLED"}

// dashboard — GET / (WBS 17.4).
func (c *Console) dashboard(w http.ResponseWriter, r *http.Request) {
	var d readmodel.Dashboard
	if !c.load(w, r, "/api/v1/dashboard", &d) {
		return
	}
	c.render(w, r, http.StatusOK, "dashboard", viewmodels.Page{Shell: c.shell(r, "Dashboard", ""), Data: d})
}

type projectsData struct {
	List      readmodel.ProjectList
	Statuses  []string
	Status    string
	Name      string
	Active    string
	Recovery  bool
	PagerBase string
}

// projects — GET /projects with server-side filters + pagination
// (WBS 17.5–17.7).
func (c *Console) projects(w http.ResponseWriter, r *http.Request) {
	q := qs(r, "status", "name", "active", "recovery", "limit", "offset")
	var l readmodel.ProjectList
	if !c.load(w, r, "/api/v1/console/projects?"+q, &l) {
		return
	}
	base := "/projects?" + qs(r, "status", "name", "active", "recovery")
	if !strings.HasSuffix(base, "?") {
		base += "&"
	}
	rec := r.URL.Query().Get("recovery")
	c.render(w, r, http.StatusOK, "projects", viewmodels.Page{Shell: c.shell(r, "Projects", "Projects"), Data: projectsData{
		List: l, Statuses: projectStatuses, Status: r.URL.Query().Get("status"), Name: r.URL.Query().Get("name"),
		Active: r.URL.Query().Get("active"), Recovery: rec == "true" || rec == "1" || rec == "on", PagerBase: base}})
}

type newProjectData struct {
	Workers     []readmodel.ProviderInfo
	Compressors []readmodel.ProviderInfo
	Modes       []string
}

func (c *Console) providerLists(r *http.Request) (workers, comps []readmodel.ProviderInfo) {
	var p struct {
		Providers []readmodel.ProviderInfo `json:"providers"`
	}
	c.d.API.Get(r, "/api/v1/providers", &p)
	for _, x := range p.Providers {
		if x.Kind == "worker" {
			workers = append(workers, x)
		} else {
			comps = append(comps, x)
		}
	}
	return
}

func (c *Console) newProjectData(r *http.Request) newProjectData {
	workers, comps := c.providerLists(r)
	modes := []string{}
	for _, m := range wbs.PlanningModes() {
		if m != wbs.PlanningCustom {
			modes = append(modes, string(m))
		}
	}
	return newProjectData{Workers: workers, Compressors: comps, Modes: modes}
}

// newProject — GET /projects/new (WBS 17.8).
func (c *Console) newProject(w http.ResponseWriter, r *http.Request) {
	c.render(w, r, http.StatusOK, "project_new", viewmodels.Page{Shell: c.shell(r, "Create project", "Projects"), Data: c.newProjectData(r),
		Form: map[string]string{"context_window": "64000", "context_threshold": "80", "planning_mode": "ATOMIC"}})
}

// deleteProjectPage — GET /projects/{id}/delete: confirmation, or the
// reason the project's status does not permit deletion.
func (c *Console) deleteProjectPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "project_delete", viewmodels.Page{Shell: c.projectShell(r, "Delete", ov), Data: ov})
}

// deleteProject — POST /projects/{id}/delete with confirmation + reason:
// the API deletes the project and cascades to everything it owns.
func (c *Console) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checked(r, "confirm") || strings.TrimSpace(r.PostFormValue("reason")) == "" {
		c.htmlError(w, r, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Deleting a project requires explicit confirmation and a reason.")
		return
	}
	resp := c.d.API.Do(r, http.MethodDelete, "/api/v1/projects/"+pid(r), nil, r.PostFormValue("reason"))
	if !resp.OK() {
		c.apiError(w, r, resp)
		return
	}
	redirect(w, r, "/projects", "Project deleted.")
}

// createProject — POST /projects: the backend validates every field and
// the errors are shown beside their fields (WBS 17.8 / 17.9).
func (c *Console) createProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	mode := wbs.PlanningMode(r.PostFormValue("planning_mode"))
	planning := wbs.PlanningConfiguration{Mode: mode}
	if mode == wbs.PlanningAtomic {
		planning.Atomic = wbs.DefaultAtomicPolicy()
		planning.PreferSmallFunctions = true
	}
	threshold := atof(r.PostFormValue("context_threshold"))
	if threshold > 1 {
		threshold /= 100
	}
	body := map[string]any{
		"name": r.PostFormValue("name"), "description": r.PostFormValue("description"), "working_directory": r.PostFormValue("working_directory"),
		"worker_provider": r.PostFormValue("worker_provider"), "worker_model": r.PostFormValue("worker_model"),
		"compression_provider": r.PostFormValue("compression_provider"), "compression_model": r.PostFormValue("compression_model"),
		"context_window": atoi(r.PostFormValue("context_window")), "context_threshold": threshold, "planning": planning,
		"use_git": checked(r, "use_git"),
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/console/projects", body, "")
	if !resp.OK() {
		code, msg, fields := resp.Error()
		if resp.Status == http.StatusBadRequest {
			c.render(w, r, http.StatusUnprocessableEntity, "project_new", viewmodels.Page{Shell: c.shell(r, "Create project", "Projects"),
				Data: c.newProjectData(r), Form: formValues(r), Errors: fields, Message: msg})
			return
		}
		c.htmlError(w, r, resp.Status, code, msg)
		return
	}
	var p struct {
		ID string `json:"id"`
	}
	_ = resp.Decode(&p)
	redirect(w, r, "/projects/"+url.PathEscape(p.ID), "Project created.")
}

type actionsData struct {
	ProjectID   string
	ExecutionID string
	Actions     []string
	CSRF        string
}

type overviewData struct {
	Overview *readmodel.Overview
	Flows    []readmodel.Flow
	Actions  actionsData
	Drift    readmodel.ConfigDrift
}

func (c *Console) actionsOf(r *http.Request, ov *readmodel.Overview) actionsData {
	a := actionsData{ProjectID: ov.ProjectID, Actions: ov.Actions, CSRF: c.d.CSRF(r)}
	if ov.Execution != nil {
		a.ExecutionID = ov.Execution.ID
	}
	return a
}

func (c *Console) flows(r *http.Request) []readmodel.Flow {
	var f struct {
		Flows []readmodel.Flow `json:"flows"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/flows", &f)
	return f.Flows
}

// overviewPage — GET /projects/{id} (WBS 17.10–17.14, 17.23, 17.31).
func (c *Console) overviewPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	shell := c.projectShell(r, ov.Name, ov)
	shell.LiveAfter = ov.LatestEventSeq
	var drift readmodel.ConfigDrift
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/config-drift", &drift)
	c.render(w, r, http.StatusOK, "overview", viewmodels.Page{Shell: shell, Data: overviewData{Overview: ov, Flows: c.flows(r), Actions: c.actionsOf(r, ov), Drift: drift}})
}

// fragment handlers (WBS 17.74 / 17.79): the same read models as the page
// regions, re-rendered for live updates.
func (c *Console) fragOverview(w http.ResponseWriter, r *http.Request) {
	var ov readmodel.Overview
	if resp := c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/overview", &ov); !resp.OK() {
		http.Error(w, "unavailable", resp.Status)
		return
	}
	c.fragment(w, r, "frag-overview", &ov)
}

func (c *Console) fragCurrentTask(w http.ResponseWriter, r *http.Request) {
	var ov readmodel.Overview
	if resp := c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/overview", &ov); !resp.OK() {
		http.Error(w, "unavailable", resp.Status)
		return
	}
	c.fragment(w, r, "frag-current-task", ov.CurrentTask)
}

func (c *Console) fragActions(w http.ResponseWriter, r *http.Request) {
	var ov readmodel.Overview
	if resp := c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/overview", &ov); !resp.OK() {
		http.Error(w, "unavailable", resp.Status)
		return
	}
	c.fragment(w, r, "frag-actions", c.actionsOf(r, &ov))
}

func (c *Console) fragFlows(w http.ResponseWriter, r *http.Request) {
	c.fragment(w, r, "frag-flows", c.flows(r))
}
