package handlers

import (
	"net/http"
	"strings"

	"overseer/internal/web/viewmodels"
)

// readiness — GET /projects/{id}/readiness (WBS 17.64 / 17.65 / 17.166):
// backend-derived, every failure listed.
func (c *Console) readiness(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var rep struct {
		View map[string]any `json:"view"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/execution-readiness", &rep) {
		return
	}
	c.render(w, r, http.StatusOK, "readiness", viewmodels.Page{Shell: c.projectShell(r, "Readiness", ov), Data: rep})
}

type startData struct {
	Preview    map[string]any
	Ready      bool
	Worker     string
	Model      string
	Checkpoint string
	Planning   string
}

// startData assembles the confirmation from backend reads only
// (WBS 17.66 / 17.68).
func (c *Console) startData(w http.ResponseWriter, r *http.Request) (startData, bool) {
	var prev struct {
		Preview   map[string]any `json:"preview"`
		Readiness struct {
			View struct {
				Ready bool `json:"ready"`
			} `json:"view"`
		} `json:"readiness"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/bootstrap-preview", &prev) {
		return startData{}, false
	}
	d := startData{Preview: prev.Preview, Ready: prev.Readiness.View.Ready}
	var st struct {
		WorkerProvider string `json:"worker_provider"`
		WorkerModel    string `json:"worker_model"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings", &st)
	d.Worker, d.Model = st.WorkerProvider, st.WorkerModel
	var eng struct {
		Settings struct {
			CheckpointPolicy string `json:"checkpoint_policy"`
		} `json:"settings"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings/engine", &eng)
	d.Checkpoint = eng.Settings.CheckpointPolicy
	var pl struct {
		Mode         string `json:"mode"`
		CustomPolicy string `json:"custom_policy"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings/planning", &pl)
	d.Planning = pl.Mode
	if pl.CustomPolicy != "" {
		d.Planning += ": " + pl.CustomPolicy
	}
	return d, true
}

// startPage — GET /projects/{id}/start: bootstrap preview + confirmation;
// creates nothing (WBS 17.66).
func (c *Console) startPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d, ok := c.startData(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "start", viewmodels.Page{Shell: c.projectShell(r, "Start", ov), Data: d})
}

// start — POST /projects/{id}/start: Phase 15 bootstrap / activation only
// (never a status write). A repeated submit reconciles to the one active
// execution (WBS 17.67 / 17.69).
func (c *Console) start(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checked(r, "confirm") {
		c.htmlError(w, r, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Starting a project requires explicit confirmation.")
		return
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/bootstrap", nil, "")
	if resp.OK() {
		redirect(w, r, "/projects/"+pid(r)+"/execution", "Project started.")
		return
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Execution map[string]any `json:"execution"`
	}
	_ = resp.Decode(&body)
	if body.Error.Code == "CONCURRENT_BOOTSTRAP" || (resp.Status == http.StatusConflict && body.Execution != nil) {
		redirectKind(w, r, "/projects/"+pid(r)+"/execution", "An execution is already active.", "info")
		return
	}
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d, ok := c.startData(w, r)
	if !ok {
		return
	}
	msg := body.Error.Message
	if msg == "" {
		_, msg, _ = resp.Error()
	}
	c.render(w, r, resp.Status, "start", viewmodels.Page{Shell: c.projectShell(r, "Start", ov), Data: d, Message: msg})
}

// lifecycle — POST /projects/{id}/{pause,resume}: lifecycle service via the
// API; the browser never alters runtime state (WBS 17.70 / 17.71).
func (c *Console) lifecycle(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/"+op, nil, r.PostFormValue("reason"))
		if !resp.OK() {
			c.apiError(w, r, resp)
			return
		}
		redirect(w, r, "/projects/"+pid(r), "Project "+strings.TrimSuffix(op, "e")+"ed.")
	}
}

// pauseAfterTask — POST /projects/{id}/pause-after-task[/cancel]: pause
// once the next task is loaded, before work on it begins (or withdraw it).
func (c *Console) pauseAfterTask(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		if ov.Execution == nil {
			c.htmlError(w, r, http.StatusConflict, "NO_EXECUTION", "The project has no execution.")
			return
		}
		path, msg := "/pause-after-task", "Pause after task requested: the project pauses once the next task is loaded, before work on it begins."
		if !on {
			path, msg = "/pause-after-task/cancel", "Pause after task cancelled."
		}
		resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/executions/"+ov.Execution.ID+path, nil, "")
		if !resp.OK() {
			c.apiError(w, r, resp)
			return
		}
		redirect(w, r, "/projects/"+pid(r), msg)
	}
}

// cancelPage — GET /projects/{id}/cancel: explicit confirmation (WBS 17.72).
func (c *Console) cancelPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "cancel", viewmodels.Page{Shell: c.projectShell(r, "Cancel", ov)})
}

// cancel — POST /projects/{id}/cancel with confirmation + reason
// (WBS 17.72 / 17.73 / 17.100).
func (c *Console) cancel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checked(r, "confirm") || strings.TrimSpace(r.PostFormValue("reason")) == "" {
		c.htmlError(w, r, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Cancelling requires explicit confirmation and a reason.")
		return
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/cancel", nil, r.PostFormValue("reason"))
	if !resp.OK() {
		c.apiError(w, r, resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r), "Project cancelled.")
}

// requeuePage — GET /projects/{id}/requeue: confirmation for restarting
// the current task from the beginning.
func (c *Console) requeuePage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "requeue", viewmodels.Page{Shell: c.projectShell(r, "Requeue task", ov), Data: ov})
}

// requeue — POST /projects/{id}/requeue with confirmation + reason: the
// current execution's task restarts from the beginning.
func (c *Console) requeue(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checked(r, "confirm") || strings.TrimSpace(r.PostFormValue("reason")) == "" {
		c.htmlError(w, r, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Requeueing requires explicit confirmation and a reason.")
		return
	}
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	if ov.Execution == nil {
		c.htmlError(w, r, http.StatusConflict, "NOT_REQUEUEABLE", "The project has no execution.")
		return
	}
	var res struct {
		Status string `json:"status"`
		Task   string `json:"task"`
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/executions/"+ov.Execution.ID+"/requeue", nil, r.PostFormValue("reason"))
	if !resp.OK() {
		c.apiError(w, r, resp)
		return
	}
	_ = resp.Decode(&res)
	msg := "Task " + res.Task + " requeued: it restarts from the beginning."
	if res.Status == "REQUESTED" {
		msg = "Requeue of task " + res.Task + " requested: it restarts from the beginning once the running step finishes."
	}
	redirect(w, r, "/projects/"+pid(r), msg)
}

// markReady — POST /projects/{id}/ready: the Phase 3 readiness operation
// (DRAFT → READY when the server-side checks pass).
func (c *Console) markReady(w http.ResponseWriter, r *http.Request) {
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/ready", nil, "")
	if !resp.OK() {
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		var rep struct {
			View map[string]any `json:"view"`
		}
		c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/execution-readiness", &rep)
		_, msg, fields := resp.Error()
		for k, v := range fields {
			msg += " " + k + ": " + v + "."
		}
		c.render(w, r, resp.Status, "readiness", viewmodels.Page{Shell: c.projectShell(r, "Readiness", ov), Data: rep, Message: msg})
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/readiness", "Project marked READY.")
}
