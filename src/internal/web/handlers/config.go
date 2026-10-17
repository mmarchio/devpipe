package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"overseer/internal/readmodel"
	"overseer/internal/wbs"
	"overseer/internal/web"
	"overseer/internal/web/viewmodels"
)

func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Roles (WBS 17.32–17.35).

func (c *Console) roles(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var l struct {
		Roles []map[string]any `json:"roles"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/roles", &l) {
		return
	}
	c.render(w, r, http.StatusOK, "roles", viewmodels.Page{Shell: c.projectShell(r, "Roles", ov), Data: l.Roles})
}

type roleData struct {
	Role          map[string]any
	Prompt        map[string]any
	Versions      []map[string]any
	Instructions  []map[string]any
	AllowedStates []string
	StateFiles    []string
}

func (c *Console) roleData(w http.ResponseWriter, r *http.Request) (*roleData, bool) {
	base := "/api/v1/projects/" + pid(r) + "/roles/" + url.PathEscape(r.PathValue("role"))
	d := &roleData{}
	if !c.load(w, r, base, &d.Role) {
		return nil, false
	}
	if p, _ := d.Role["project_id"].(string); p != r.PathValue("id") {
		c.htmlError(w, r, http.StatusNotFound, "not_found", "role not found")
		return nil, false
	}
	c.d.API.Get(r, base+"/system-prompt", &d.Prompt)
	var v struct {
		Versions []map[string]any `json:"versions"`
	}
	c.d.API.Get(r, base+"/system-prompt/versions", &v)
	d.Versions = v.Versions
	var in struct {
		Instructions []map[string]any `json:"instructions"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/roles/"+url.PathEscape(r.PathValue("role"))+"/instructions", &in)
	d.Instructions = in.Instructions
	var pv struct {
		AllowedStates []string `json:"allowed_states"`
		StateFiles    []string `json:"state_files"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/roles/"+url.PathEscape(r.PathValue("role"))+"/preview", &pv)
	d.AllowedStates, d.StateFiles = pv.AllowedStates, pv.StateFiles
	return d, true
}

func (c *Console) role(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d, ok := c.roleData(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "role", viewmodels.Page{Shell: c.projectShell(r, "Role", ov), Data: d})
}

// roleSave — POST: each changed part goes through its role API (versioned,
// audited there); a stale prompt version is a conflict (WBS 17.33 /
// 17.140).
func (c *Console) roleSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	base := "/api/v1/projects/" + pid(r) + "/roles/" + url.PathEscape(r.PathValue("role"))
	fail := func(resp web.APIResponse) {
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		d, ok := c.roleData(w, r)
		if !ok {
			return
		}
		_, msg, fields := resp.Error()
		c.render(w, r, resp.Status, "role", viewmodels.Page{Shell: c.projectShell(r, "Role", ov), Data: d, Form: formValues(r), Errors: fields,
			Message: msg, Conflict: resp.Status == http.StatusConflict})
	}
	name, desc := r.PostFormValue("name"), r.PostFormValue("description")
	enabled := checked(r, "enabled")
	if resp := c.d.API.Do(r, http.MethodPatch, base, map[string]any{"name": name, "description": desc, "enabled": enabled}, ""); !resp.OK() {
		fail(resp)
		return
	}
	var cur struct {
		Content string `json:"content"`
	}
	c.d.API.Get(r, base+"/system-prompt", &cur)
	if prompt := r.PostFormValue("system_prompt"); prompt != cur.Content {
		resp := c.d.API.Do(r, http.MethodPut, base+"/system-prompt", map[string]any{"content": prompt, "expected_version": atoi(r.PostFormValue("prompt_version"))}, "")
		if !resp.OK() {
			fail(resp)
			return
		}
	}
	var pv struct {
		AllowedStates []string `json:"allowed_states"`
		StateFiles    []string `json:"state_files"`
	}
	c.d.API.Get(r, base+"/preview", &pv)
	if states := splitList(r.PostFormValue("allowed_states")); strings.Join(states, ",") != strings.Join(pv.AllowedStates, ",") {
		if resp := c.d.API.Do(r, http.MethodPut, base+"/states", map[string]any{"states": states}, ""); !resp.OK() {
			fail(resp)
			return
		}
	}
	if files := splitList(r.PostFormValue("state_files")); strings.Join(files, ",") != strings.Join(pv.StateFiles, ",") {
		if resp := c.d.API.Do(r, http.MethodPut, base+"/state-files", map[string]any{"state_files": files}, ""); !resp.OK() {
			fail(resp)
			return
		}
	}
	redirect(w, r, "/projects/"+pid(r)+"/roles/"+url.PathEscape(r.PathValue("role")), "Role saved.")
}

func (c *Console) rolePreview(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var p map[string]any
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/roles/"+url.PathEscape(r.PathValue("role"))+"/preview", &p) {
		return
	}
	c.render(w, r, http.StatusOK, "role_preview", viewmodels.Page{Shell: c.projectShell(r, "Role preview", ov), Data: p})
}

// roleSetDefault — POST: overwrites the global default role (used by every
// project initialized afterwards), so it needs confirmation + a reason.
func (c *Console) roleSetDefault(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checked(r, "confirm") || strings.TrimSpace(r.PostFormValue("reason")) == "" {
		c.htmlError(w, r, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Setting a role as the default requires confirmation and a reason.")
		return
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/roles/"+url.PathEscape(r.PathValue("role"))+"/set-default", nil, r.PostFormValue("reason"))
	if !resp.OK() {
		c.apiError(w, r, resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/roles/"+url.PathEscape(r.PathValue("role")), "Role saved as the global default.")
}

// roleReset — POST: dangerous action, confirmation + reason (WBS 17.73).
func (c *Console) roleReset(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !checked(r, "confirm") || strings.TrimSpace(r.PostFormValue("reason")) == "" {
		c.htmlError(w, r, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Resetting a role requires confirmation and a reason.")
		return
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/roles/"+url.PathEscape(r.PathValue("role"))+"/reset", nil, r.PostFormValue("reason"))
	if !resp.OK() {
		c.apiError(w, r, resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/roles/"+url.PathEscape(r.PathValue("role")), "Role reset to the global default.")
}

// ---------------------------------------------------------------------------
// Workflow (WBS 17.36–17.41).

type stateView struct {
	Key            string   `json:"key"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	PromptTemplate string   `json:"prompt_template"`
	Kind           string   `json:"kind"`
	Terminal       bool     `json:"terminal"`
	Enabled        bool     `json:"enabled"`
	Roles          []string `json:"roles"`
	StateFiles     []string `json:"state_files"`
}

type conditionView struct {
	Field        string `json:"field"`
	Operator     string `json:"operator"`
	Literal      any    `json:"literal"`
	ContextField string `json:"context_field"`
}

type transitionView struct {
	ID        string         `json:"id"`
	From      string         `json:"from"`
	To        string         `json:"to"`
	Outcome   string         `json:"outcome"`
	Priority  int            `json:"priority"`
	Enabled   bool           `json:"enabled"`
	Condition *conditionView `json:"condition"`
}

type adjacency struct {
	From string
	To   []string
}

type workflowData struct {
	Machine     map[string]any
	States      []stateView
	Transitions []transitionView
	Terminal    []string
	Versions    []map[string]any
	Validation  map[string]any
	Graph       *graphView
	Adjacency   []adjacency
}

func (c *Console) workflowData(r *http.Request) workflowData {
	base := "/api/v1/projects/" + pid(r) + "/state-machine"
	d := workflowData{}
	c.d.API.Get(r, base, &d.Machine)
	var s struct {
		States []stateView `json:"states"`
	}
	c.d.API.Get(r, base+"/states", &s)
	d.States = s.States
	var t struct {
		Transitions []transitionView `json:"transitions"`
	}
	c.d.API.Get(r, base+"/transitions", &t)
	d.Transitions = t.Transitions
	var v struct {
		Versions []map[string]any `json:"versions"`
	}
	c.d.API.Get(r, base+"/versions", &v)
	d.Versions = v.Versions
	for _, st := range d.States {
		if st.Terminal {
			d.Terminal = append(d.Terminal, st.Key)
		}
	}
	// Diagram from the persisted records (WBS 17.37).
	adj := map[string][]string{}
	var order []string
	var edges []readmodel.Edge
	for _, tr := range d.Transitions {
		if _, ok := adj[tr.From]; !ok {
			order = append(order, tr.From)
		}
		adj[tr.From] = append(adj[tr.From], tr.To+" ["+tr.Outcome+"]")
		edges = append(edges, readmodel.Edge{FromKey: tr.From, ToKey: tr.To})
	}
	for _, f := range order {
		d.Adjacency = append(d.Adjacency, adjacency{From: f, To: adj[f]})
	}
	units := make([]readmodel.UnitRow, 0, len(d.States))
	for _, st := range d.States {
		units = append(units, readmodel.UnitRow{Key: st.Key})
	}
	d.Graph = layout(units, edges)
	return d
}

func (c *Console) workflow(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "workflow", viewmodels.Page{Shell: c.projectShell(r, "Workflow", ov), Data: c.workflowData(r)})
}

// workflowValidate — POST: runs Phase 6 validation (read-only) and shows
// every issue (WBS 17.40).
func (c *Console) workflowValidate(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d := c.workflowData(r)
	var v map[string]any
	resp := c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/state-machine/validation", &v)
	if !resp.OK() {
		c.apiError(w, r, resp)
		return
	}
	d.Validation = v
	c.render(w, r, http.StatusOK, "workflow", viewmodels.Page{Shell: c.projectShell(r, "Workflow", ov), Data: d})
}

type stateEditData struct {
	State       stateView
	Instruction string
	Kinds       []string
	Roles       []map[string]any
}

func (c *Console) stateEditData(w http.ResponseWriter, r *http.Request) (*stateEditData, bool) {
	var s struct {
		State       stateView `json:"state"`
		Instruction string    `json:"instruction"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/state-machine/states/"+url.PathEscape(r.PathValue("key")), &s) {
		return nil, false
	}
	var l struct {
		Roles []map[string]any `json:"roles"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/roles", &l)
	return &stateEditData{State: s.State, Instruction: s.Instruction, Kinds: []string{"ACTION", "DECISION", "VALIDATION", "CONTROL", "TERMINAL"}, Roles: l.Roles}, true
}

func (c *Console) stateEdit(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d, ok := c.stateEditData(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "state_edit", viewmodels.Page{Shell: c.projectShell(r, "State", ov), Data: d})
}

// stateSave — POST: each part through its state-config API; the backend
// rejects invalid configuration (WBS 17.38).
func (c *Console) stateSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	key := url.PathEscape(r.PathValue("key"))
	base := "/api/v1/projects/" + pid(r) + "/state-machine/states/" + key
	fail := func(resp web.APIResponse) {
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		d, ok := c.stateEditData(w, r)
		if !ok {
			return
		}
		_, msg, fields := resp.Error()
		c.render(w, r, resp.Status, "state_edit", viewmodels.Page{Shell: c.projectShell(r, "State", ov), Data: d, Form: formValues(r), Errors: fields, Message: msg})
	}
	cur, ok := c.stateEditData(w, r)
	if !ok {
		return
	}
	upd := map[string]any{"name": r.PostFormValue("name"), "kind": r.PostFormValue("kind")}
	if resp := c.d.API.Do(r, http.MethodPut, base, upd, ""); !resp.OK() {
		fail(resp)
		return
	}
	if t := r.PostFormValue("prompt_template"); t != cur.State.PromptTemplate {
		if resp := c.d.API.Do(r, http.MethodPut, base+"/prompt-template", map[string]any{"template": t}, ""); !resp.OK() {
			fail(resp)
			return
		}
	}
	if in := r.PostFormValue("instruction"); in != cur.Instruction {
		if resp := c.d.API.Do(r, http.MethodPut, base+"/instructions", map[string]any{"content": in}, ""); !resp.OK() {
			fail(resp)
			return
		}
	}
	if files := splitList(r.PostFormValue("state_files")); strings.Join(files, ",") != strings.Join(cur.State.StateFiles, ",") {
		if resp := c.d.API.Do(r, http.MethodPut, base+"/state-files", map[string]any{"categories": files}, ""); !resp.OK() {
			fail(resp)
			return
		}
	}
	if role := r.PostFormValue("role"); role != "" {
		if resp := c.d.API.Do(r, http.MethodPost, base+"/roles/"+url.PathEscape(role), nil, ""); !resp.OK() {
			fail(resp)
			return
		}
	}
	redirect(w, r, "/projects/"+pid(r)+"/workflow", "State saved.")
}

func (c *Console) transitionByID(r *http.Request) *transitionView {
	var t struct {
		Transitions []transitionView `json:"transitions"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/state-machine/transitions", &t)
	for i := range t.Transitions {
		if t.Transitions[i].ID == r.PathValue("tid") {
			return &t.Transitions[i]
		}
	}
	return nil
}

func (c *Console) transitionEdit(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	tr := c.transitionByID(r)
	if tr == nil {
		c.htmlError(w, r, http.StatusNotFound, "not_found", "transition not found")
		return
	}
	c.render(w, r, http.StatusOK, "transition_edit", viewmodels.Page{Shell: c.projectShell(r, "Transition", ov), Data: map[string]any{"Transition": tr}})
}

// transitionSave — POST: ambiguity / invalid references are rejected by
// the backend (WBS 17.39).
func (c *Console) transitionSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	base := "/api/v1/projects/" + pid(r) + "/state-machine/transitions/" + url.PathEscape(r.PathValue("tid"))
	fail := func(resp web.APIResponse) {
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		_, msg, fields := resp.Error()
		if fields == nil {
			fields = map[string]string{}
		}
		fields["transition"] = msg
		c.render(w, r, resp.Status, "transition_edit", viewmodels.Page{Shell: c.projectShell(r, "Transition", ov),
			Data: map[string]any{"Transition": c.transitionByID(r)}, Errors: fields, Message: msg})
	}
	body := map[string]any{"from": r.PostFormValue("from"), "to": r.PostFormValue("to")}
	if resp := c.d.API.Do(r, http.MethodPut, base, body, ""); !resp.OK() {
		fail(resp)
		return
	}
	if resp := c.d.API.Do(r, http.MethodPut, base+"/priority", map[string]any{"priority": atoi(r.PostFormValue("priority"))}, ""); !resp.OK() {
		fail(resp)
		return
	}
	if f := strings.TrimSpace(r.PostFormValue("condition_field")); f != "" {
		cond := map[string]any{"field": f, "operator": r.PostFormValue("condition_operator"), "literal": r.PostFormValue("condition_literal")}
		if resp := c.d.API.Do(r, http.MethodPut, base+"/condition", cond, ""); !resp.OK() {
			fail(resp)
			return
		}
	}
	redirect(w, r, "/projects/"+pid(r)+"/workflow", "Transition saved.")
}

// ---------------------------------------------------------------------------
// State files (WBS 17.42–17.47).

type stateFileEntry struct {
	ID             string `json:"id"`
	TypeKey        string `json:"type_key"`
	Source         string `json:"source"`
	Scope          string `json:"scope"`
	CurrentVersion int    `json:"current_version"`
	Drift          string `json:"-"`
}

func (c *Console) stateFiles(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var l struct {
		Files []stateFileEntry `json:"files"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/state-files", &l) {
		return
	}
	var drift struct {
		Files []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"files"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/state-files/drift", &drift)
	st := map[string]string{}
	for _, f := range drift.Files {
		st[f.ID] = f.Status
	}
	for i := range l.Files {
		l.Files[i].Drift = st[l.Files[i].ID]
	}
	c.render(w, r, http.StatusOK, "state_files", viewmodels.Page{Shell: c.projectShell(r, "State files", ov), Data: map[string]any{"Files": l.Files}})
}

type stateFileCurrent struct {
	Version       int    `json:"version"`
	Content       string `json:"content"`
	Hash          string `json:"hash"`
	Size          int    `json:"size"`
	TokenEstimate int    `json:"token_estimate"`
}

type stateFileData struct {
	Entry    stateFileEntry
	Current  stateFileCurrent
	Versions []map[string]any
	Drift    map[string]any
	Editable bool
}

func (c *Console) stateFileData(w http.ResponseWriter, r *http.Request) (*stateFileData, bool) {
	var l struct {
		Files []stateFileEntry `json:"files"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/state-files", &l)
	d := &stateFileData{}
	found := false
	for _, f := range l.Files {
		if f.ID == r.PathValue("file") {
			d.Entry, found = f, true
		}
	}
	if !found {
		c.htmlError(w, r, http.StatusNotFound, "not_found", "state file not found")
		return nil, false
	}
	var cur struct {
		Version *stateFileCurrent `json:"version"`
	}
	base := "/api/v1/projects/" + pid(r) + "/state-files/" + url.PathEscape(r.PathValue("file"))
	c.d.API.Get(r, base, &cur)
	if cur.Version != nil {
		d.Current = *cur.Version
	}
	var v struct {
		Versions []map[string]any `json:"versions"`
	}
	c.d.API.Get(r, base+"/versions", &v)
	d.Versions = v.Versions
	c.d.API.Get(r, base+"/drift-status", &d.Drift)
	// Editability is the source class policy; the backend enforces it.
	d.Editable = d.Entry.Source == "MANAGED" || d.Entry.Source == "USER"
	return d, true
}

func (c *Console) stateFile(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d, ok := c.stateFileData(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "state_file", viewmodels.Page{Shell: c.projectShell(r, "State file", ov), Data: d})
}

// stateFileSave — POST: new version with expected version; DERIVED files
// are rejected server-side (WBS 17.44).
func (c *Console) stateFileSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	exp := atoi(r.PostFormValue("expected_version"))
	resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/state-files/"+url.PathEscape(r.PathValue("file")),
		map[string]any{"content": r.PostFormValue("content"), "expected_version": exp}, "")
	if !resp.OK() {
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		d, ok := c.stateFileData(w, r)
		if !ok {
			return
		}
		_, msg, fields := resp.Error()
		c.render(w, r, resp.Status, "state_file", viewmodels.Page{Shell: c.projectShell(r, "State file", ov), Data: d, Form: formValues(r),
			Errors: fields, Message: msg, Conflict: resp.Status == http.StatusConflict})
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/state-files/"+url.PathEscape(r.PathValue("file")), "State file saved as a new version.")
}

// stateFileDrift — POST: one offered drift resolution (WBS 17.47).
func (c *Console) stateFileDrift(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	opt := strings.ToLower(r.PostFormValue("option"))
	if opt != "restore" && opt != "import" && opt != "ignore" {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "Unknown drift resolution.")
		return
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/state-files/"+url.PathEscape(r.PathValue("file"))+"/drift/"+opt,
		map[string]any{"reason": r.PostFormValue("reason"), "confirm": checked(r, "confirm")}, r.PostFormValue("reason"))
	if !resp.OK() {
		c.apiError(w, r, resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/state-files/"+url.PathEscape(r.PathValue("file")), "Drift resolution applied.")
}

// ---------------------------------------------------------------------------
// Prompt templates (WBS 17.48 / 17.49).

func (c *Console) templatesPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var l struct {
		Templates []map[string]any `json:"templates"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/prompt-templates", &l) {
		return
	}
	c.render(w, r, http.StatusOK, "templates_list", viewmodels.Page{Shell: c.projectShell(r, "Templates", ov), Data: l.Templates})
}

type templateData struct {
	Template  map[string]any
	Versions  []map[string]any
	Variables []string
}

var templateVariables = []string{"role.system_prompt", "role.instructions", "state.instruction", "project.context", "phase.context",
	"work_unit.task", "runtime.tests", "continuation.status", "response_protocol.schema"}

func (c *Console) templateData(w http.ResponseWriter, r *http.Request) (*templateData, bool) {
	base := "/api/v1/projects/" + pid(r) + "/prompt-templates/" + url.PathEscape(r.PathValue("template"))
	d := &templateData{Variables: templateVariables}
	if !c.load(w, r, base, &d.Template) {
		return nil, false
	}
	var v struct {
		Versions []map[string]any `json:"versions"`
	}
	c.d.API.Get(r, base+"/versions", &v)
	d.Versions = v.Versions
	return d, true
}

func (c *Console) templatePage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d, ok := c.templateData(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "template_edit", viewmodels.Page{Shell: c.projectShell(r, "Template", ov), Data: d})
}

func (c *Console) templateSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/prompt-templates/"+url.PathEscape(r.PathValue("template"))+"/versions",
		map[string]any{"content": r.PostFormValue("content"), "expected_version": atoi(r.PostFormValue("expected_version"))}, "")
	if !resp.OK() {
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		d, ok := c.templateData(w, r)
		if !ok {
			return
		}
		_, msg, fields := resp.Error()
		c.render(w, r, resp.Status, "template_edit", viewmodels.Page{Shell: c.projectShell(r, "Template", ov), Data: d, Form: formValues(r),
			Errors: fields, Message: msg, Conflict: resp.Status == http.StatusConflict})
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/templates/"+url.PathEscape(r.PathValue("template")), "Template version saved.")
}

// ---------------------------------------------------------------------------
// Prompts / compression / cache / precompute (WBS 17.50–17.58).

func (c *Console) promptsPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var l struct {
		Prompts []map[string]any `json:"prompts"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/prompts?limit=100", &l)
	// The list never carries prompt content (loaded on demand, 17.152).
	for _, p := range l.Prompts {
		delete(p, "content")
	}
	c.render(w, r, http.StatusOK, "prompts", viewmodels.Page{Shell: c.projectShell(r, "Prompts", ov), Data: l.Prompts})
}

type promptData struct {
	Prompt     map[string]any
	Provenance []map[string]any
	Attempts   []map[string]any
}

func (c *Console) promptPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	base := "/api/v1/projects/" + pid(r) + "/prompts/" + url.PathEscape(r.PathValue("prompt"))
	d := promptData{}
	if !c.load(w, r, base, &d.Prompt) {
		return
	}
	var pv struct {
		Entries []map[string]any `json:"entries"`
	}
	c.d.API.Get(r, base+"/provenance", &pv)
	d.Provenance = pv.Entries
	var at struct {
		Attempts []map[string]any `json:"attempts"`
	}
	c.d.API.Get(r, base+"/compression-attempts", &at)
	d.Attempts = at.Attempts
	c.render(w, r, http.StatusOK, "prompt", viewmodels.Page{Shell: c.projectShell(r, "Prompt", ov), Data: d})
}

func (c *Console) promptDiff(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var d map[string]any
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/prompts/"+url.PathEscape(r.PathValue("prompt"))+"/compression-attempts/"+
		url.PathEscape(r.PathValue("attempt"))+"/diff", &d) {
		return
	}
	c.render(w, r, http.StatusOK, "prompt_diff", viewmodels.Page{Shell: c.projectShell(r, "Diff", ov), Data: d})
}

// workerPrompt — GET /projects/{id}/prompts/worker/{worker}: lineage of a
// dispatched worker prompt (deep link from dispatch history).
func (c *Console) workerPrompt(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var d map[string]any
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/worker-prompts/"+url.PathEscape(r.PathValue("worker"))+"/lineage", &d) {
		return
	}
	c.render(w, r, http.StatusOK, "prompt_diff", viewmodels.Page{Shell: c.projectShell(r, "Worker prompt", ov), Data: d})
}

func (c *Console) promptPreviewPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "prompt_preview", viewmodels.Page{Shell: c.projectShell(r, "Prompt preview", ov)})
}

// promptPreview — POST: a preview never creates a dispatch (WBS 17.50).
func (c *Console) promptPreview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	body := map[string]any{}
	if t := strings.TrimSpace(r.PostFormValue("template_id")); t != "" {
		body["template_id"] = t
	}
	if v := atoi(r.PostFormValue("template_version")); v > 0 {
		body["template_version"] = v
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/prompts/preview", body, "")
	page := viewmodels.Page{Shell: c.projectShell(r, "Prompt preview", ov), Form: formValues(r)}
	if !resp.OK() {
		_, page.Message, _ = resp.Error()
		c.render(w, r, resp.Status, "prompt_preview", page)
		return
	}
	var p map[string]any
	_ = resp.Decode(&p)
	page.Data = p
	c.render(w, r, http.StatusOK, "prompt_preview", page)
}

type cacheData struct {
	Stats     map[string]any
	Entries   []map[string]any
	Evictions int
}

func (c *Console) cachePage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d := cacheData{}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/prompt-cache/stats", &d.Stats)
	var l struct {
		Entries []map[string]any `json:"entries"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/prompt-cache?limit=100", &l)
	d.Entries = l.Entries
	c.render(w, r, http.StatusOK, "cache", viewmodels.Page{Shell: c.projectShell(r, "Prompt cache", ov), Data: d})
}

func (c *Console) cacheEntry(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var e map[string]any
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/prompt-cache/"+url.PathEscape(r.PathValue("entry")), &e) {
		return
	}
	c.render(w, r, http.StatusOK, "cache_entry", viewmodels.Page{Shell: c.projectShell(r, "Cache entry", ov), Data: e})
}

type statusCount struct {
	Status string
	Count  int
}

func (c *Console) precomputePage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var jobs struct {
		Jobs []map[string]any `json:"jobs"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/precompute/jobs?limit=100", &jobs)
	var m struct {
		Overall struct {
			Created int `json:"created"`
			Used    int `json:"used"`
		} `json:"overall"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/precompute/metrics", &m)
	counts := map[string]int{}
	for _, j := range jobs.Jobs {
		if s, ok := j["status"].(string); ok {
			counts[s]++
		}
	}
	d := map[string]any{"Jobs": jobs.Jobs, "Counts": []statusCount{
		{"Ready", counts["COMPLETED"] + counts["READY"]}, {"Generating", counts["RUNNING"] + counts["PENDING"]},
		{"Invalidated", counts["INVALIDATED"] + counts["CANCELLED"]}, {"Failed", counts["FAILED"]},
		{"Used", m.Overall.Used}, {"Unused", m.Overall.Created - m.Overall.Used}}}
	c.render(w, r, http.StatusOK, "precompute", viewmodels.Page{Shell: c.projectShell(r, "Precomputation", ov), Data: d})
}

// ---------------------------------------------------------------------------
// Project configuration (WBS 17.59–17.63).

func (c *Console) configIndex(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "config", viewmodels.Page{Shell: c.projectShell(r, "Configuration", ov)})
}

// configVersion reads the configuration version from a guarded GET.
func configVersion(resp web.APIResponse) int {
	v, _ := strconv.Atoi(resp.Header.Get(web.ConfigVersionHeader))
	return v
}

type workerConfigData struct {
	Settings      map[string]any
	Worker        map[string]any
	Workers       []readmodel.ProviderInfo
	Policies      []string
	SendPolicies  []string
	ConfigVersion int
}

func (c *Console) workerConfigData(r *http.Request) workerConfigData {
	d := workerConfigData{Policies: []string{"SINGLE_ACTIVE_PER_PROJECT", "MULTIPLE_ACTIVE"}, SendPolicies: []string{"REJECT", "WAIT"}}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings", &d.Settings)
	d.ConfigVersion = configVersion(c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings/worker", &d.Worker))
	d.Workers, _ = c.providerLists(r)
	return d
}

func (c *Console) configWorker(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "config_worker", viewmodels.Page{Shell: c.projectShell(r, "Worker configuration", ov), Data: c.workerConfigData(r)})
}

// renderConfigError re-renders a configuration page with the backend's
// field errors or conflict (WBS 17.133 / 17.134 / 17.140).
func (c *Console) renderConfigError(w http.ResponseWriter, r *http.Request, page string, data any, resp web.APIResponse) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	_, msg, fields := resp.Error()
	c.render(w, r, resp.Status, page, viewmodels.Page{Shell: c.projectShell(r, "Configuration", ov), Data: data, Form: formValues(r),
		Errors: fields, Message: msg, Conflict: resp.Status == http.StatusConflict})
}

// configWorkerSave — POST: project settings (full state, project version)
// + worker session policy (configuration version). No secret field exists.
func (c *Console) configWorkerSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	var cur map[string]any
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings", &cur)
	cur["worker_provider"] = r.PostFormValue("worker_provider")
	cur["worker_model"] = r.PostFormValue("worker_model")
	cur["context_window"] = atoi(r.PostFormValue("context_window"))
	cur["worker_timeout"] = r.PostFormValue("worker_timeout")
	cur["version"] = atoi(r.PostFormValue("project_version"))
	if resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/settings", cur, ""); !resp.OK() {
		c.renderConfigError(w, r, "config_worker", c.workerConfigData(r), resp)
		return
	}
	body := map[string]any{"session_policy": r.PostFormValue("session_policy"), "concurrent_send": r.PostFormValue("concurrent_send"),
		"version": atoi(r.PostFormValue("config_version"))}
	if resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/settings/worker", body, ""); !resp.OK() {
		c.renderConfigError(w, r, "config_worker", c.workerConfigData(r), resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/config/worker", "Worker configuration saved.")
}

type compressionConfigData struct {
	Settings        map[string]any
	Modes           []string
	ValidationModes []string
	ConfigVersion   int
}

func (c *Console) compressionConfigData(r *http.Request) compressionConfigData {
	d := compressionConfigData{Modes: []string{"NEVER", "AUTO", "ALWAYS"}, ValidationModes: []string{"DETERMINISTIC", "HYBRID"}}
	d.ConfigVersion = configVersion(c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings/compression", &d.Settings))
	return d
}

func (c *Console) configCompression(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "config_compression", viewmodels.Page{Shell: c.projectShell(r, "Compression", ov), Data: c.compressionConfigData(r)})
}

func (c *Console) configCompressionSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	body := map[string]any{"mode": r.PostFormValue("mode"), "target_tokens": atoi(r.PostFormValue("target_tokens")),
		"target_ratio": atof(r.PostFormValue("target_ratio")), "max_attempts": atoi(r.PostFormValue("max_attempts")),
		"validation_mode": r.PostFormValue("validation_mode"), "version": atoi(r.PostFormValue("config_version"))}
	if resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/settings/compression", body, ""); !resp.OK() {
		c.renderConfigError(w, r, "config_compression", c.compressionConfigData(r), resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/config/compression", "Compression configuration saved.")
}

type planningConfigData struct {
	Config        wbs.PlanningConfiguration
	Modes         []string
	ConfigVersion int
}

func (c *Console) planningConfigData(r *http.Request) planningConfigData {
	d := planningConfigData{}
	for _, m := range wbs.PlanningModes() {
		d.Modes = append(d.Modes, string(m))
	}
	d.ConfigVersion = configVersion(c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings/planning", &d.Config))
	return d
}

func (c *Console) configPlanning(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "config_planning", viewmodels.Page{Shell: c.projectShell(r, "Planning", ov), Data: c.planningConfigData(r)})
}

// configPlanningSave — POST: the configuration persists exactly
// (WBS 17.61).
func (c *Console) configPlanningSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	mode := wbs.PlanningMode(r.PostFormValue("mode"))
	cfg := wbs.PlanningConfiguration{Mode: mode, PreferSmallFunctions: checked(r, "small_functions")}
	if mode == wbs.PlanningAtomic || mode == wbs.PlanningCustom {
		cfg.Atomic = wbs.AtomicPolicy{RequireIndependentlyTestable: checked(r, "independently_testable"),
			RequireExplicitDependencies: checked(r, "dependencies"), RequireAcceptanceCriteria: checked(r, "acceptance_criteria"),
			SeparateComposition: checked(r, "separate_composition"), SeparateIntegration: checked(r, "separate_integration"),
			RequireSingleInvocation: checked(r, "single_invocation")}
	}
	if mode == wbs.PlanningCustom {
		cfg.CustomPolicy = r.PostFormValue("custom_policy")
	}
	body := map[string]any{"mode": cfg.Mode, "atomic": cfg.Atomic, "prefer_small_functions": cfg.PreferSmallFunctions,
		"custom_policy": cfg.CustomPolicy, "version": atoi(r.PostFormValue("config_version"))}
	if resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/settings/planning", body, ""); !resp.OK() {
		c.renderConfigError(w, r, "config_planning", c.planningConfigData(r), resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/config/planning", "Planning policy saved.")
}

type continuationConfigData struct {
	Settings         map[string]any
	Project          map[string]any
	ThresholdPercent float64
	UnknownPolicies  []string
	ConfigVersion    int
}

func (c *Console) continuationConfigData(r *http.Request) continuationConfigData {
	d := continuationConfigData{UnknownPolicies: []string{"same_session", "fresh_session"}}
	var s struct {
		Settings         map[string]any `json:"settings"`
		ContextThreshold float64        `json:"context_threshold"`
	}
	d.ConfigVersion = configVersion(c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings/continuation", &s))
	d.Settings = s.Settings
	th := s.ContextThreshold
	if th <= 1 {
		th *= 100
	}
	d.ThresholdPercent = float64(int(th*10+0.5)) / 10
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings", &d.Project)
	return d
}

func (c *Console) configContinuation(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "config_continuation", viewmodels.Page{Shell: c.projectShell(r, "Continuation", ov), Data: c.continuationConfigData(r)})
}

// configContinuationSave — POST: continuation settings (validated by
// Phase 14), the context threshold and auto-continue (project settings).
func (c *Console) configContinuationSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	body := map[string]any{"max_continuations_per_work_unit": atoi(r.PostFormValue("max_continuations_per_work_unit")),
		"max_session_resets_per_work_unit": atoi(r.PostFormValue("max_session_resets_per_work_unit")),
		"max_no_progress_continuations":    atoi(r.PostFormValue("max_no_progress_continuations")),
		"unknown_context_policy":           r.PostFormValue("unknown_context_policy"), "version": atoi(r.PostFormValue("config_version"))}
	if resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/settings/continuation", body, ""); !resp.OK() {
		c.renderConfigError(w, r, "config_continuation", c.continuationConfigData(r), resp)
		return
	}
	th := atof(r.PostFormValue("context_threshold"))
	if th > 1 {
		th /= 100
	}
	auto := checked(r, "auto_continue")
	var cur map[string]any
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings", &cur)
	f, _ := cur["context_threshold"].(float64)
	thChanged := th > 0 && f != th
	if a, _ := cur["auto_continue"].(bool); thChanged || a != auto {
		if thChanged {
			cur["context_threshold"] = th
		}
		cur["auto_continue"] = auto
		cur["version"] = atoi(r.PostFormValue("project_version"))
		if resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/settings", cur, ""); !resp.OK() {
			c.renderConfigError(w, r, "config_continuation", c.continuationConfigData(r), resp)
			return
		}
	}
	redirect(w, r, "/projects/"+pid(r)+"/config/continuation", "Continuation configuration saved.")
}

type checkpointConfigData struct {
	Current       string
	UseGit        bool
	ConfigVersion int
}

func (c *Console) checkpointConfigData(r *http.Request) checkpointConfigData {
	var s struct {
		Settings struct {
			CheckpointPolicy string `json:"checkpoint_policy"`
			UseGit           bool   `json:"use_git"`
		} `json:"settings"`
	}
	var d checkpointConfigData
	d.ConfigVersion = configVersion(c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings/engine", &s))
	d.Current, d.UseGit = s.Settings.CheckpointPolicy, s.Settings.UseGit
	return d
}

func (c *Console) configCheckpoint(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "config_checkpoint", viewmodels.Page{Shell: c.projectShell(r, "Checkpoints", ov), Data: c.checkpointConfigData(r)})
}

// configCheckpointSave — POST: the "use git" option persists through the
// engine settings, which set both checkpoint policies from it (WBS 17.63).
func (c *Console) configCheckpointSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	body := map[string]any{"use_git": checked(r, "use_git"), "version": atoi(r.PostFormValue("config_version"))}
	if resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/settings/engine", body, ""); !resp.OK() {
		c.renderConfigError(w, r, "config_checkpoint", c.checkpointConfigData(r), resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/config/checkpoint", "Checkpoint settings saved.")
}
