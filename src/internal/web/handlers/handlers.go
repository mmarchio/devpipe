package handlers

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"overseer/internal/access"
	"overseer/internal/readmodel"
	"overseer/internal/web"
	"overseer/internal/web/static"
	"overseer/internal/web/templates"
	"overseer/internal/web/viewmodels"
)

// API is the in-process backend API the pages consume (implemented by
// web.InternalClient). Pages never call services directly (T17.1.2).
type API interface {
	Do(parent *http.Request, method, path string, body any, reason string) web.APIResponse
	Get(parent *http.Request, path string, v any) web.APIResponse
}

// Deps wires the page handlers.
type Deps struct {
	API     API
	CSRF    func(*http.Request) string
	Version string
	Now     func() time.Time
	// Logger receives rendering failures (nil = discarded).
	Logger *slog.Logger
}

// Console serves the server-rendered operations console.
type Console struct {
	d      Deps
	rnd    *templates.Renderer
	assets *static.Assets
}

// New parses the templates.
func New(d Deps) (*Console, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	assets := static.New()
	rnd, err := templates.New(template.FuncMap{"asset": assets.URL})
	if err != nil {
		return nil, err
	}
	return &Console{d: d, rnd: rnd, assets: assets}, nil
}

// Renderer exposes the renderer (tests).
func (c *Console) Renderer() *templates.Renderer { return c.rnd }

// Assets exposes the static assets.
func (c *Console) Assets() *static.Assets { return c.assets }

// route is one page route.
type route struct {
	method  string
	pattern string
	action  access.Action
	h       http.HandlerFunc
	// delegated: the handler's mutation is performed by an /api/v1 route
	// (audited there).
	delegated bool
}

// Mount registers every page route, the static assets and the HTML error
// renderer on the router.
func (c *Console) Mount(r *web.Router) error {
	r.SetStatic(c.assets)
	web.SetHTMLErrors(c.htmlError)
	for _, rt := range c.routes() {
		var err error
		if rt.delegated {
			err = r.RegisterDelegatedPage(rt.method, rt.pattern, rt.action, rt.h)
		} else {
			err = r.RegisterPage(rt.method, rt.pattern, rt.action, rt.h)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func get(p string, h http.HandlerFunc) route {
	return route{method: http.MethodGet, pattern: p, action: access.ActionRead, h: h}
}

func post(p string, a access.Action, h http.HandlerFunc) route {
	return route{method: http.MethodPost, pattern: p, action: a, h: h, delegated: true}
}

// shell builds the shell for a page.
func (c *Console) shell(r *http.Request, title, active string) viewmodels.Shell {
	return viewmodels.Shell{Title: title, Nav: viewmodels.Primary(active), CSRF: c.d.CSRF(r), Version: c.d.Version, Now: c.d.Now().UTC(),
		Flash: flashFrom(r), FlashKind: flashKindFrom(r)}
}

// projectShell adds the current project context (and live stream) from the
// overview read model.
func (c *Console) projectShell(r *http.Request, title string, ov *readmodel.Overview) viewmodels.Shell {
	s := c.shell(r, title, "Projects")
	if ov != nil {
		pc := &viewmodels.ProjectContext{ID: ov.ProjectID, Name: ov.Name, Status: ov.Status, Recovery: ov.Recovery}
		if ov.Execution != nil {
			pc.ExecutionID, pc.ExecutionStatus = ov.Execution.ID, ov.Execution.Status
		}
		s.Project = pc
		s.LiveProject = ov.ProjectID
		s.LiveAfter = ov.LatestEventSeq
	}
	return s
}

// flashFrom reads a post-redirect notice (?notice=...).
func flashFrom(r *http.Request) string {
	n := r.URL.Query().Get("notice")
	if len(n) > 200 {
		n = n[:200]
	}
	return n
}

// flashKindFrom reads the notice's banner style (?notice_kind=...); only
// known kinds are honoured.
func flashKindFrom(r *http.Request) string {
	switch k := r.URL.Query().Get("notice_kind"); k {
	case "ok", "info", "warn":
		return k
	}
	return ""
}

// render writes a page (200) or a clean error page.
func (c *Console) render(w http.ResponseWriter, r *http.Request, status int, name string, page viewmodels.Page) {
	var buf bytes.Buffer
	if err := c.rnd.Page(&buf, name, page); err != nil {
		if c.d.Logger != nil {
			c.d.Logger.Error("page render failed", "page", name, "path", r.URL.Path, "error", err.Error())
		}
		c.htmlError(w, r, http.StatusInternalServerError, "internal_error", "The page could not be rendered.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// fragment writes a live-update region.
func (c *Console) fragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := c.rnd.Fragment(&buf, name, data); err != nil {
		if c.d.Logger != nil {
			c.d.Logger.Error("fragment render failed", "fragment", name, "error", err.Error())
		}
		http.Error(w, "fragment unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// errorData is the error page model.
type errorData struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Reload    bool
}

// htmlError renders the human-friendly error page (WBS 17.132): status,
// code, safe message and request id — never internals.
func (c *Console) htmlError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if message == "" {
		message = http.StatusText(status)
	}
	page := viewmodels.Page{Shell: c.shell(r, "Error", ""), Data: errorData{Status: status, Code: code, Message: message,
		RequestID: web.RequestIDFromContext(r.Context()), Reload: status == http.StatusConflict}}
	var buf bytes.Buffer
	if err := c.rnd.Page(&buf, "error", page); err != nil {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// apiError renders an API failure as an error page.
func (c *Console) apiError(w http.ResponseWriter, r *http.Request, resp web.APIResponse) {
	code, msg, _ := resp.Error()
	status := resp.Status
	if status < 400 {
		status = http.StatusInternalServerError
	}
	c.htmlError(w, r, status, code, msg)
}

// load GETs an API path into v, rendering the error page on failure; it
// returns false when the response was an error.
func (c *Console) load(w http.ResponseWriter, r *http.Request, path string, v any) bool {
	resp := c.d.API.Get(r, path, v)
	if !resp.OK() {
		c.apiError(w, r, resp)
		return false
	}
	return true
}

// overview loads the project overview (shell context of project pages).
func (c *Console) overview(w http.ResponseWriter, r *http.Request) (*readmodel.Overview, bool) {
	var ov readmodel.Overview
	if !c.load(w, r, "/api/v1/projects/"+url.PathEscape(r.PathValue("id"))+"/overview", &ov) {
		return nil, false
	}
	return &ov, true
}

// redirect performs post-redirect-get with an optional notice (WBS 17.165).
func redirect(w http.ResponseWriter, r *http.Request, to, notice string) {
	redirectKind(w, r, to, notice, "ok")
}

// redirectKind is redirect with an explicit notice banner style ("ok" =
// operation succeeded, "info" = informational).
func redirectKind(w http.ResponseWriter, r *http.Request, to, notice, kind string) {
	if notice != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + "notice=" + url.QueryEscape(notice) + "&notice_kind=" + url.QueryEscape(kind)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// formValues snapshots submitted form values (for re-rendering).
func formValues(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, vs := range r.PostForm {
		if k == "csrf_token" || len(vs) == 0 {
			continue
		}
		out[k] = vs[0]
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func checked(r *http.Request, name string) bool {
	v := r.PostFormValue(name)
	return v == "on" || v == "true" || v == "yes" || v == "1"
}

func pid(r *http.Request) string { return url.PathEscape(r.PathValue("id")) }

// qs keeps only the given query parameters (server-side filters).
func qs(r *http.Request, keys ...string) string {
	v := url.Values{}
	for _, k := range keys {
		if x := r.URL.Query().Get(k); x != "" {
			v.Set(k, x)
		}
	}
	return v.Encode()
}
