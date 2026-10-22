package handlers

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"overseer/internal/bounds"
	"overseer/internal/readmodel"
	"overseer/internal/web/viewmodels"
)

type phasesData struct {
	Phases []readmodel.PhaseRow
	// UploadError is a multi-file upload's problem (empty = none).
	UploadError string
	// OrderError is a rejected reorder's problem (empty = none).
	OrderError string
	// Reorderable is set when at least two phases can move.
	Reorderable bool
}

func (c *Console) phaseList(r *http.Request) ([]readmodel.PhaseRow, bool) {
	var l struct {
		Phases []readmodel.PhaseRow `json:"phases"`
	}
	resp := c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/phases", &l)
	return l.Phases, resp.OK()
}

// phases — GET /projects/{id}/phases (WBS 17.15–17.17).
func (c *Console) phases(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	list, _ := c.phaseList(r)
	c.render(w, r, http.StatusOK, "phases", viewmodels.Page{Shell: c.projectShell(r, "Phases", ov), Data: phasesData{Phases: list, Reorderable: reorderable(list)}})
}

// reorderable reports whether at least two phases can move.
func reorderable(list []readmodel.PhaseRow) bool {
	n := 0
	for _, p := range list {
		if p.Movable {
			n++
		}
	}
	return n >= 2
}

// appendPhase — POST /projects/{id}/phases: name + description only; the
// server assigns the order (append-only, WBS 17.17).
func (c *Console) appendPhase(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	body := map[string]any{"name": r.PostFormValue("name"), "description": r.PostFormValue("description")}
	if wbs := r.PostFormValue("wbs"); strings.TrimSpace(wbs) != "" {
		body["wbs"] = wbs
	}
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/phases", body, "")
	if !resp.OK() {
		if resp.Status == http.StatusBadRequest {
			ov, ok := c.overview(w, r)
			if !ok {
				return
			}
			list, _ := c.phaseList(r)
			_, msg, fields := resp.Error()
			c.render(w, r, http.StatusUnprocessableEntity, "phases", viewmodels.Page{Shell: c.projectShell(r, "Phases", ov),
				Data: phasesData{Phases: list, Reorderable: reorderable(list)}, Form: formValues(r), Errors: fields, Message: msg})
			return
		}
		c.apiError(w, r, resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/phases", "Phase appended.")
}

// reorderPhases — POST /projects/{id}/phases/order: either the full order
// from drag and drop (repeated phase_ids, first to last) or one keyboard /
// no-script move ("move" = "<phase id>:up|down", swapping with the nearest
// movable phase in that direction). The server enforces which phases can
// move.
func (c *Console) reorderPhases(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	ids := r.PostForm["phase_ids"]
	if mv := r.PostFormValue("move"); mv != "" {
		list, ok := c.phaseList(r)
		if !ok {
			c.orderError(w, r, "The phases could not be loaded.")
			return
		}
		var err error
		if ids, err = movePhase(list, mv); err != nil {
			c.orderError(w, r, err.Error())
			return
		}
	}
	if len(ids) == 0 {
		c.orderError(w, r, "No phase order was submitted.")
		return
	}
	resp := c.d.API.Do(r, http.MethodPut, "/api/v1/projects/"+pid(r)+"/phases/order", map[string]any{"phase_ids": ids}, "")
	if !resp.OK() {
		if resp.Status == http.StatusConflict || resp.Status == http.StatusBadRequest {
			_, msg, _ := resp.Error()
			c.orderError(w, r, "The phase order was not changed: "+msg)
			return
		}
		c.apiError(w, r, resp)
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/phases", "Phase order saved.")
}

// movePhase returns the phase order with the phase in mv ("<id>:up|down")
// swapped with the nearest movable phase in that direction.
func movePhase(list []readmodel.PhaseRow, mv string) ([]string, error) {
	id, dir, _ := strings.Cut(mv, ":")
	step := map[string]int{"up": -1, "down": 1}[dir]
	at := -1
	for i, p := range list {
		if p.ID == id {
			at = i
		}
	}
	if step == 0 || at < 0 {
		return nil, errors.New("Unknown move; reload the page.")
	}
	if !list[at].Movable {
		return nil, errors.New("Phase " + strconv.Itoa(list[at].Order) + " — " + list[at].Name + " cannot move: " + list[at].LockReason + ".")
	}
	to := -1
	for j := at + step; j >= 0 && j < len(list); j += step {
		if list[j].Movable {
			to = j
			break
		}
	}
	if to < 0 {
		return nil, errors.New("Phase " + strconv.Itoa(list[at].Order) + " — " + list[at].Name + " cannot move " + dir + " any further.")
	}
	ids := make([]string, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	ids[at], ids[to] = ids[to], ids[at]
	return ids, nil
}

// orderError re-renders the phases page with a reorder problem.
func (c *Console) orderError(w http.ResponseWriter, r *http.Request, msg string) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	list, _ := c.phaseList(r)
	c.render(w, r, http.StatusUnprocessableEntity, "phases", viewmodels.Page{Shell: c.projectShell(r, "Phases", ov),
		Data: phasesData{Phases: list, OrderError: msg, Reorderable: reorderable(list)}})
}

// maxPhaseNameLen matches the append form's name limit.
const maxPhaseNameLen = 200

// uploadedPhase is one file of a multi-file phase upload.
type uploadedPhase struct {
	file, name, wbs string
}

// uploadPhases — POST /projects/{id}/phases/wbs-upload (multipart, field
// "files"): every .md / .txt file becomes an appended phase, in file-name
// order — the name without its extension is the phase name, the contents
// its initial WBS. All files are checked before any phase is appended; an
// append the server rejects stops the upload and the page reports which
// phases were appended.
func (c *Console) uploadPhases(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		c.uploadError(w, r, "The upload could not be read: "+err.Error())
		return
	}
	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["files"]
	}
	if len(files) == 0 {
		c.uploadError(w, r, "Choose or drop at least one .md or .txt file.")
		return
	}
	var phases []uploadedPhase
	var problems []string
	for _, fh := range files {
		p, err := readUploadedPhase(fh)
		if err != nil {
			problems = append(problems, fh.Filename+": "+err.Error())
			continue
		}
		phases = append(phases, p)
	}
	if len(problems) > 0 {
		c.uploadError(w, r, "No phases were appended. "+strings.Join(problems, "; "))
		return
	}
	sort.SliceStable(phases, func(i, j int) bool { return phases[i].file < phases[j].file })
	var done []string
	for _, p := range phases {
		resp := c.d.API.Do(r, http.MethodPost, "/api/v1/projects/"+pid(r)+"/phases", map[string]any{"name": p.name, "description": "", "wbs": p.wbs}, "")
		if !resp.OK() {
			_, msg, _ := resp.Error()
			appended := "No phases were appended."
			if len(done) > 0 {
				appended = "Appended " + strconv.Itoa(len(done)) + ": " + strings.Join(done, ", ") + "."
			}
			c.uploadError(w, r, appended+" "+p.file+" was not appended: "+msg+" The remaining files were not uploaded.")
			return
		}
		done = append(done, p.name)
	}
	noun := "phases"
	if len(done) == 1 {
		noun = "phase"
	}
	redirect(w, r, "/projects/"+pid(r)+"/phases", "Appended "+strconv.Itoa(len(done))+" "+noun+": "+strings.Join(done, ", ")+".")
}

// readUploadedPhase checks one uploaded file and derives its phase.
func readUploadedPhase(fh *multipart.FileHeader) (uploadedPhase, error) {
	base := filepath.Base(fh.Filename)
	ext := strings.ToLower(filepath.Ext(base))
	if ext != ".md" && ext != ".txt" {
		return uploadedPhase{}, errors.New("only .md or .txt files can be uploaded")
	}
	name := strings.TrimSpace(strings.TrimSuffix(base, filepath.Ext(base)))
	if name == "" {
		return uploadedPhase{}, errors.New("the file name gives no phase name")
	}
	if utf8.RuneCountInString(name) > maxPhaseNameLen {
		name = string([]rune(name)[:maxPhaseNameLen])
	}
	f, err := fh.Open()
	if err != nil {
		return uploadedPhase{}, errors.New("the file could not be read")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(bounds.Limit(bounds.WBS))+1))
	if err != nil {
		return uploadedPhase{}, errors.New("the file could not be read")
	}
	content := string(b)
	switch {
	case strings.TrimSpace(content) == "":
		return uploadedPhase{}, errors.New("the file is empty")
	case !utf8.ValidString(content):
		return uploadedPhase{}, errors.New("the file is not UTF-8 text")
	}
	if err := bounds.Check(bounds.WBS, content); err != nil {
		return uploadedPhase{}, err
	}
	return uploadedPhase{file: base, name: name, wbs: content}, nil
}

// uploadError re-renders the phases page with the upload's problem.
func (c *Console) uploadError(w http.ResponseWriter, r *http.Request, msg string) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	list, _ := c.phaseList(r)
	c.render(w, r, http.StatusUnprocessableEntity, "phases", viewmodels.Page{Shell: c.projectShell(r, "Phases", ov),
		Data: phasesData{Phases: list, UploadError: msg, Reorderable: reorderable(list)}})
}

type phaseData struct {
	Detail readmodel.PhaseDetail
	Flow   readmodel.Flow
}

// phaseDetail — GET /projects/{id}/phases/{phase} (WBS 17.18 / 17.24).
func (c *Console) phaseDetail(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var d readmodel.PhaseDetail
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/phases/"+url.PathEscape(r.PathValue("phase")), &d) {
		return
	}
	latest := ""
	if len(d.Validations) > 0 {
		latest = d.Validations[0].Status
	}
	c.render(w, r, http.StatusOK, "phase", viewmodels.Page{Shell: c.projectShell(r, "Phase", ov),
		Data: phaseData{Detail: d, Flow: phaseFlow(d.Phase, latest)}})
}

// phaseFlow mirrors the backend phase-completion flow from persisted
// statuses (WBS 17.170); 100 % never renders as DONE.
func phaseFlow(p readmodel.PhaseRow, latest string) readmodel.Flow {
	f := readmodel.Flow{Name: "Phase " + strconv.Itoa(p.Order)}
	units := "PENDING"
	if p.Progress.Total > 0 && p.Progress.Done == p.Progress.Total {
		units = "DONE"
	} else if p.Progress.Done > 0 {
		units = "ACTIVE"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: "Work Units " + strconv.FormatFloat(p.Progress.Percent, 'f', 1, 64) + "%", State: units})
	val := "PENDING"
	switch {
	case p.Status == "DONE":
		val = "DONE"
	case latest == "RUNNING" || (units == "DONE" && latest != "FAIL"):
		val = "ACTIVE"
	case latest == "FAIL":
		val = "FAILED"
	}
	label := "Phase Validation"
	if latest != "" {
		label += " " + latest
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: label, State: val})
	done := "PENDING"
	if p.Status == "DONE" {
		done = "DONE"
	}
	f.Steps = append(f.Steps, readmodel.FlowStep{Label: "Phase DONE", State: done})
	return f
}

type wbsData struct {
	Phase    readmodel.PhaseRow
	Version  int
	Hash     string
	Content  string
	Created  string
	Modified string
}

func (c *Console) phaseRow(w http.ResponseWriter, r *http.Request) (*readmodel.PhaseRow, bool) {
	var d readmodel.PhaseDetail
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/phases/"+url.PathEscape(r.PathValue("phase")), &d) {
		return nil, false
	}
	return &d.Phase, true
}

type wbsVersion struct {
	Version     int    `json:"version"`
	ContentHash string `json:"content_hash"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
}

func (c *Console) wbsVersions(r *http.Request) []wbsVersion {
	var v struct {
		Versions []wbsVersion `json:"versions"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/phases/"+url.PathEscape(r.PathValue("phase"))+"/wbs/versions", &v)
	sort.Slice(v.Versions, func(i, j int) bool { return v.Versions[i].Version > v.Versions[j].Version })
	return v.Versions
}

func (c *Console) wbsData(w http.ResponseWriter, r *http.Request) (wbsData, bool) {
	ph, ok := c.phaseRow(w, r)
	if !ok {
		return wbsData{}, false
	}
	d := wbsData{Phase: *ph}
	vs := c.wbsVersions(r)
	if len(vs) > 0 {
		d.Version, d.Hash, d.Content, d.Modified = vs[0].Version, vs[0].ContentHash, vs[0].Content, vs[0].CreatedAt
		d.Created = vs[len(vs)-1].CreatedAt
	}
	return d, true
}

// wbsEditor — GET /projects/{id}/phases/{phase}/wbs (WBS 17.19).
func (c *Console) wbsEditor(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d, ok := c.wbsData(w, r)
	if !ok {
		return
	}
	c.render(w, r, http.StatusOK, "wbs", viewmodels.Page{Shell: c.projectShell(r, "WBS", ov), Data: d})
}

// wbsSave — POST: saves a NEW version with the expected version; a stale
// version is a 409 conflict page keeping the submitted text (WBS 17.20 /
// 17.133).
func (c *Console) wbsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	expected := atoi(r.PostFormValue("expected_version"))
	body := map[string]any{"content": r.PostFormValue("content"), "expected_version": expected}
	path := "/api/v1/projects/" + pid(r) + "/phases/" + url.PathEscape(r.PathValue("phase")) + "/wbs"
	resp := c.d.API.Do(r, http.MethodPut, path, body, "")
	if resp.OK() {
		redirect(w, r, "/projects/"+pid(r)+"/phases/"+url.PathEscape(r.PathValue("phase"))+"/wbs", "WBS saved as a new version.")
		return
	}
	if resp.Status != http.StatusConflict && resp.Status != http.StatusBadRequest {
		c.apiError(w, r, resp)
		return
	}
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d, ok := c.wbsData(w, r)
	if !ok {
		return
	}
	_, msg, fields := resp.Error()
	c.render(w, r, resp.Status, "wbs", viewmodels.Page{Shell: c.projectShell(r, "WBS", ov), Data: d, Form: formValues(r), Errors: fields,
		Message: msg, Conflict: resp.Status == http.StatusConflict})
}

type wbsHistoryData struct {
	Phase     readmodel.PhaseRow
	Versions  []map[string]any
	From, To  int
	Diff      map[string]any
	DiffError string
}

// wbsHistory — GET .../wbs/history[?from&to] (WBS 17.21 / 17.22).
func (c *Console) wbsHistory(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	ph, ok := c.phaseRow(w, r)
	if !ok {
		return
	}
	d := wbsHistoryData{Phase: *ph}
	var v struct {
		Versions []map[string]any `json:"versions"`
	}
	base := "/api/v1/projects/" + pid(r) + "/phases/" + url.PathEscape(r.PathValue("phase")) + "/wbs"
	c.d.API.Get(r, base+"/versions", &v)
	sort.Slice(v.Versions, func(i, j int) bool {
		a, _ := v.Versions[i]["version"].(float64)
		b, _ := v.Versions[j]["version"].(float64)
		return a > b
	})
	d.Versions = v.Versions
	d.From, d.To = atoi(r.URL.Query().Get("from")), atoi(r.URL.Query().Get("to"))
	if d.From == 0 && d.To == 0 && len(v.Versions) >= 2 {
		a, _ := v.Versions[1]["version"].(float64)
		b, _ := v.Versions[0]["version"].(float64)
		d.From, d.To = int(a), int(b)
	}
	if d.From > 0 && d.To > 0 {
		var diff map[string]any
		resp := c.d.API.Get(r, base+"/diff?from="+strconv.Itoa(d.From)+"&to="+strconv.Itoa(d.To), &diff)
		if resp.OK() {
			d.Diff = diff
		} else {
			_, d.DiffError, _ = resp.Error()
		}
	}
	c.render(w, r, http.StatusOK, "wbs_history", viewmodels.Page{Shell: c.projectShell(r, "WBS history", ov), Data: d})
}

type unitsData struct {
	Phase       readmodel.PhaseRow
	Units       readmodel.UnitPage
	Statuses    []string
	Kinds       []string
	Validations []string
	Status      string
	Kind        string
	Readiness   string
	Validation  string
	// FragmentURL is the live-refresh URL of the units table, filters and
	// page included (built here: a query string interpolated into a
	// template URL would be escaped as one value).
	FragmentURL string
	PagerBase   string
}

// units — GET /projects/{id}/phases/{phase}/work-units (WBS 17.25–17.30,
// 17.150): server-side filters and pagination.
func (c *Console) units(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	ph, ok := c.phaseRow(w, r)
	if !ok {
		return
	}
	q := qs(r, "status", "kind", "readiness", "validation", "limit", "offset")
	var page readmodel.UnitPage
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/phases/"+url.PathEscape(r.PathValue("phase"))+"/work-units?"+q, &page) {
		return
	}
	frag := url.Values{}
	if fq, err := url.ParseQuery(q); err == nil {
		frag = fq
	}
	frag.Set("phase", r.PathValue("phase"))
	filters := qs(r, "status", "kind", "readiness", "validation")
	base := "/projects/" + pid(r) + "/phases/" + url.PathEscape(r.PathValue("phase")) + "/work-units?" + filters
	if filters != "" {
		base += "&"
	}
	c.render(w, r, http.StatusOK, "units", viewmodels.Page{Shell: c.projectShell(r, "Work units", ov), Data: unitsData{
		Phase: *ph, Units: page, Statuses: []string{"PENDING", "READY", "EXECUTING", "INCOMPLETE", "VALIDATING", "DONE", "FAILED", "BLOCKED"},
		Kinds: []string{"ATOMIC", "COMPOSITION", "INTEGRATION", "VALIDATION"}, Validations: []string{"RUNNING", "PASS", "FAIL", "INFRA_ERROR", "NOT_CONFIGURED", "ABANDONED"},
		Status: r.URL.Query().Get("status"), Kind: r.URL.Query().Get("kind"), Readiness: r.URL.Query().Get("readiness"),
		Validation: r.URL.Query().Get("validation"), FragmentURL: "/projects/" + pid(r) + "/fragments/units?" + frag.Encode(), PagerBase: base}})
}

// fragUnits — the units table region (live refresh).
func (c *Console) fragUnits(w http.ResponseWriter, r *http.Request) {
	phase := r.URL.Query().Get("phase")
	q := qs(r, "status", "kind", "readiness", "validation", "limit", "offset")
	var page readmodel.UnitPage
	if resp := c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/phases/"+url.PathEscape(phase)+"/work-units?"+q, &page); !resp.OK() {
		http.Error(w, "unavailable", resp.Status)
		return
	}
	c.fragment(w, r, "frag-units", page)
}

// unit — GET /projects/{id}/work-units/{unit} (WBS 17.27–17.29, 17.85).
func (c *Console) unit(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var d unitData
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/work-units/"+url.PathEscape(r.PathValue("unit")), &d.UnitDetail) {
		return
	}
	// Lineage (WBS 18.205): ids and hashes from the backend.
	d.LineagePath = "/api/v1/projects/" + pid(r) + "/work-units/" + url.PathEscape(r.PathValue("unit")) + "/lineage"
	var l unitLineage
	if resp := c.d.API.Get(r, d.LineagePath, &l); resp.OK() {
		d.Lineage = &l
	}
	c.render(w, r, http.StatusOK, "unit", viewmodels.Page{Shell: c.projectShell(r, d.Unit.Key, ov), Data: d})
}

type unitData struct {
	readmodel.UnitDetail
	Lineage     *unitLineage
	LineagePath string
}

type unitLineage struct {
	Complete    bool   `json:"complete"`
	WBSVersion  int    `json:"wbs_version"`
	WBSHash     string `json:"wbs_hash"`
	PlanVersion int    `json:"plan_version"`
	Dispatches  []struct {
		ID            string `json:"id"`
		Attempt       int    `json:"attempt"`
		Status        string `json:"status"`
		CorrelationID string `json:"correlation_id"`
		Prompt        struct {
			CanonicalPromptHash string `json:"canonical_prompt_hash"`
			WorkerPromptHash    string `json:"worker_prompt_hash"`
		} `json:"prompt"`
	} `json:"dispatches"`
	CorrelationIDs []string `json:"correlation_ids"`
}

type graphNode struct {
	Label        string
	X, Y, W      int
	TX, TY       int
	depth, index int
}

type graphEdge struct{ X1, Y1, X2, Y2 int }

type graphView struct {
	Width, Height int
	Nodes         []graphNode
	Edges         []graphEdge
}

type graphData struct {
	Phase     readmodel.PhaseRow
	Edges     []readmodel.Edge
	EdgePage  readmodel.Page
	Graph     *graphView
	PagerBase string
}

// maxDrawnUnits bounds the server-drawn SVG; larger plans use the edge
// list only (WBS 17.28 / 17.150).
const maxDrawnUnits = 120

// graph — GET /projects/{id}/phases/{phase}/graph (WBS 17.28): a layered
// drawing generated from the persisted edges.
func (c *Console) graph(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	ph, ok := c.phaseRow(w, r)
	if !ok {
		return
	}
	q := qs(r, "limit", "offset")
	var e struct {
		Edges []readmodel.Edge `json:"edges"`
		Page  readmodel.Page   `json:"page"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/phases/"+url.PathEscape(r.PathValue("phase"))+"/edges?"+q, &e) {
		return
	}
	d := graphData{Phase: *ph, Edges: e.Edges, EdgePage: e.Page, PagerBase: "/projects/" + pid(r) + "/phases/" + url.PathEscape(r.PathValue("phase")) + "/graph?"}
	if ph.Progress.Total > 0 && ph.Progress.Total <= maxDrawnUnits && e.Page.Total <= len(e.Edges) {
		var units readmodel.UnitPage
		c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/phases/"+url.PathEscape(r.PathValue("phase"))+"/work-units?limit=200", &units)
		d.Graph = layout(units.Rows, e.Edges)
	}
	c.render(w, r, http.StatusOK, "graph", viewmodels.Page{Shell: c.projectShell(r, "Dependencies", ov), Data: d})
}

// layout places units in columns by dependency depth (longest path from a
// root) and draws one line per persisted edge.
func layout(units []readmodel.UnitRow, edges []readmodel.Edge) *graphView {
	if len(units) == 0 {
		return nil
	}
	deps := map[string][]string{}
	for _, e := range edges {
		deps[e.ToKey] = append(deps[e.ToKey], e.FromKey)
	}
	depth := map[string]int{}
	var visit func(k string, seen map[string]bool) int
	visit = func(k string, seen map[string]bool) int {
		if d, ok := depth[k]; ok {
			return d
		}
		if seen[k] {
			return 0
		}
		seen[k] = true
		d := 0
		for _, p := range deps[k] {
			if x := visit(p, seen) + 1; x > d {
				d = x
			}
		}
		depth[k] = d
		return d
	}
	cols := map[int][]string{}
	maxDepth := 0
	for _, u := range units {
		d := visit(u.Key, map[string]bool{})
		cols[d] = append(cols[d], u.Key)
		if d > maxDepth {
			maxDepth = d
		}
	}
	const colW, rowH, nodeW = 170, 40, 140
	pos := map[string][2]int{}
	g := &graphView{}
	maxRows := 0
	for d := 0; d <= maxDepth; d++ {
		for i, k := range cols[d] {
			x, y := 10+d*colW, 10+i*rowH
			pos[k] = [2]int{x, y}
			label := k
			if len(label) > 18 {
				label = label[:17] + "…"
			}
			g.Nodes = append(g.Nodes, graphNode{Label: label, X: x, Y: y, W: nodeW, TX: x + 6, TY: y + 17})
		}
		if len(cols[d]) > maxRows {
			maxRows = len(cols[d])
		}
	}
	for _, e := range edges {
		a, ok1 := pos[e.FromKey]
		b, ok2 := pos[e.ToKey]
		if ok1 && ok2 {
			g.Edges = append(g.Edges, graphEdge{X1: a[0] + nodeW, Y1: a[1] + 13, X2: b[0], Y2: b[1] + 13})
		}
	}
	g.Width, g.Height = 20+(maxDepth+1)*colW, 20+maxRows*rowH
	return g
}

var _ = strings.TrimSpace
