package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"overseer/internal/access"
	"overseer/internal/web/middleware"
)

// ServerConfig carries the HTTP server timeout settings.
type ServerConfig struct {
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	// MaxHeaderBytes bounds request headers (0 = net/http default).
	MaxHeaderBytes int
}

// DefaultServerConfig returns finite production timeouts (T1.7.4).
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// RouterConfig configures the router. Zero values select the v1 defaults
// (local authentication, LocalPolicy authorization, per-process CSRF key,
// default size and rate limits).
type RouterConfig struct {
	StaticDir string

	// Authenticator resolves request principals (WBS 17.126).
	Authenticator middleware.Authenticator
	// Authorizer decides route actions (WBS 17.128).
	Authorizer access.Authorizer
	// Sessions is the browser session store (WBS 17.130).
	Sessions *middleware.SessionManager
	// CSRFKey keys CSRF tokens (WBS 17.118); random when empty.
	CSRFKey []byte
	// Limits bounds request sizes (WBS 17.120).
	Limits middleware.LimitConfig
	// Rate limits expensive routes (WBS 17.154).
	Rate middleware.RateConfig
}

// Router is the application HTTP handler. It serves the web routes,
// the reserved /api/v1/* namespace, and /static/*.
type Router struct {
	mux       *http.ServeMux
	apiMux    *http.ServeMux
	staticDir string
	renderer  *Renderer

	authz    access.Authorizer
	sessions *middleware.SessionManager
	csrf     *middleware.CSRF
	chain    http.Handler
	// audit wraps every unsafe route (P17-40); nil = no audit.
	audit func(action access.Action, pattern string, h http.Handler) http.Handler
	// static serves the embedded assets when set.
	static http.Handler
	// configGuard protects settings routes (P17-46); nil = no guard.
	configGuard ConfigVersioner
	// routes lists every registered "METHOD pattern" → action (P17-10).
	routes map[string]access.Action

	initialized bool
	checks      map[string]ReadinessCheck
	// writeGuard, when set, may refuse mutation routes at request time
	// (read-only instance after a restore, decision U4).
	writeGuard atomic.Pointer[WriteGuard]
	// observe receives every request's method / status (metrics).
	observe atomic.Pointer[RequestObserver]
	// draining refuses unsafe methods (shutdown, WBS 18.125).
	draining atomic.Bool
}

// SetDraining makes every unsafe request fail with 503 SHUTTING_DOWN.
func (r *Router) SetDraining(v bool) { r.draining.Store(v) }

// CodeShuttingDown: the instance is shutting down (503).
const CodeShuttingDown = "SHUTTING_DOWN"

// WriteGuard decides at request time whether a mutation action may run;
// a non-nil error refuses it (409 INSTANCE_READ_ONLY).
type WriteGuard func(action access.Action) error

// CodeInstanceReadOnly: the instance refuses mutations (WBS 18.46).
const CodeInstanceReadOnly = "INSTANCE_READ_ONLY"

// SetWriteGuard installs (nil removes) the request-time mutation guard. It
// applies to every mutation route, including those registered earlier.
func (r *Router) SetWriteGuard(g WriteGuard) {
	if g == nil {
		r.writeGuard.Store(nil)
		return
	}
	r.writeGuard.Store(&g)
}

func (r *Router) guardWrites(action access.Action, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if g := r.writeGuard.Load(); g != nil {
			if err := (*g)(action); err != nil {
				WriteError(w, req, NewConflictCode(CodeInstanceReadOnly, err.Error()))
				return
			}
		}
		h.ServeHTTP(w, req)
	})
}

// ReadinessCheck reports the state of one dependency.
type ReadinessCheck func(ctx context.Context) (ok bool, err error)

// NewRouter builds the base router with the reserved namespaces.
func NewRouter(cfg RouterConfig) *Router {
	r := &Router{
		mux:       http.NewServeMux(),
		apiMux:    http.NewServeMux(),
		staticDir: cfg.StaticDir,
		checks:    map[string]ReadinessCheck{},
		routes:    map[string]access.Action{},
	}
	authn := cfg.Authenticator
	if authn == nil {
		authn = middleware.LocalAuthenticator{}
	}
	r.authz = cfg.Authorizer
	if r.authz == nil {
		r.authz = access.LocalPolicy{}
	}
	r.sessions = cfg.Sessions
	if r.sessions == nil {
		r.sessions = middleware.NewSessionManager(middleware.SessionConfig{Secure: true})
	}
	r.csrf = middleware.NewCSRF(cfg.CSRFKey, WriteError)
	// Security chain (outermost first): headers → session → authenticate
	// → CSRF → size limits → rate limit → mux (per-route authorize +
	// audit).
	var h http.Handler = r.mux
	h = middleware.NewRateLimiter(cfg.Rate, WriteError).Middleware(h)
	h = middleware.BodyLimit(cfg.Limits, WriteError)(h)
	h = r.csrf.Middleware(h)
	h = middleware.Authenticate(authn, WriteError)(h)
	h = r.sessions.Middleware(h)
	r.chain = middleware.SecurityHeaders(h)
	// Web routes.
	r.mux.HandleFunc("/", r.handleRoot)
	r.mux.HandleFunc("/health", r.handleHealth)
	r.mux.HandleFunc("/ready", r.handleReady)
	// Reserved namespaces.
	r.mux.HandleFunc("/api/v1/", r.handleAPI)
	r.mux.HandleFunc("/static/", func(w http.ResponseWriter, req *http.Request) {
		switch {
		case r.static != nil:
			if req.Method != http.MethodGet && req.Method != http.MethodHead {
				WriteError(w, req, methodNotAllowed())
				return
			}
			r.static.ServeHTTP(w, req)
		case r.staticDir != "":
			r.handleStatic(w, req)
		default:
			WriteError(w, req, NewNotFoundError("static file"))
		}
	})
	return r
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.draining.Load() {
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			// Shutting down (WBS 18.125): no new state change is accepted.
			w.Header().Set("Retry-After", "5")
			WriteError(w, req, &appError{sentinel: ErrUnavailable, code: CodeShuttingDown, msg: "the overseer is shutting down"})
			return
		}
	}
	obs := r.observe.Load()
	if obs == nil {
		r.chain.ServeHTTP(w, req)
		return
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	r.chain.ServeHTTP(sw, req)
	(*obs)(req.Method, sw.status)
}

// RequestObserver receives every request's method and response status
// (HTTP metrics, WBS 18.113).
type RequestObserver func(method string, status int)

// SetRequestObserver installs (nil removes) the request observer.
func (r *Router) SetRequestObserver(o RequestObserver) {
	if o == nil {
		r.observe.Store(nil)
		return
	}
	r.observe.Store(&o)
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (SSE) working.
func (s *statusWriter) Flush() {
	// Through the controller: the wrapped writer may itself be a wrapper
	// that only exposes Unwrap.
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// RegisterAPI adds a route to the /api/v1/ namespace. method is the HTTP
// method; pattern is the full path (e.g. "/api/v1/projects"). Safe methods
// are READ; unsafe methods take their action from the route action table
// and fail registration when unlisted (P17-05).
func (r *Router) RegisterAPI(method, pattern string, h http.HandlerFunc) error {
	action, ok := actionFor(method, pattern)
	if !ok {
		return fmt.Errorf("web: unsafe route %s %s has no authorization action", method, pattern)
	}
	return r.RegisterAPIAction(method, pattern, action, h)
}

// RegisterAPIAction adds an API route with an explicit action. A READ (or
// PREVIEW) action on an unsafe method that mutates is the caller's
// responsibility; a mutation action on a safe method is rejected
// (WBS 17.119).
func (r *Router) RegisterAPIAction(method, pattern string, action access.Action, h http.HandlerFunc) error {
	if !strings.HasPrefix(pattern, "/api/v1/") {
		return NewValidationError("API route must be under /api/v1/")
	}
	if err := r.checkAction(method, pattern, action); err != nil {
		return err
	}
	r.apiMux.Handle(method+" "+pattern, r.wrap(method, pattern, action, h))
	return nil
}

// RegisterPage adds a server-rendered page route (outside /api/v1/).
func (r *Router) RegisterPage(method, pattern string, action access.Action, h http.Handler) error {
	if strings.HasPrefix(pattern, "/api/") || strings.HasPrefix(pattern, "/static/") {
		return fmt.Errorf("web: page route %s collides with a reserved namespace", pattern)
	}
	if err := r.checkAction(method, pattern, action); err != nil {
		return err
	}
	r.mux.Handle(method+" "+pattern, r.wrap(method, pattern, action, h))
	return nil
}

// RegisterDelegatedPage adds a page route whose mutations are delegated to
// /api/v1 routes through the InternalClient: it is authorized here, and
// audited once by the API route it calls (no duplicate audit row).
func (r *Router) RegisterDelegatedPage(method, pattern string, action access.Action, h http.Handler) error {
	if strings.HasPrefix(pattern, "/api/") || strings.HasPrefix(pattern, "/static/") {
		return fmt.Errorf("web: page route %s collides with a reserved namespace", pattern)
	}
	if err := r.checkAction(method, pattern, action); err != nil {
		return err
	}
	r.mux.Handle(method+" "+pattern, middleware.Authorize(r.authz, action, func(req *http.Request) access.Resource {
		return resourceFor(pattern, req.PathValue)
	}, WriteError, h))
	return nil
}

// SetStatic mounts the embedded static assets at /static/ (replacing the
// directory handler).
func (r *Router) SetStatic(h http.Handler) {
	r.static = h
}

// SetHTMLErrors installs the HTML error renderer used for page requests
// (WBS 17.132); API requests keep the JSON error model.
func SetHTMLErrors(fn func(w http.ResponseWriter, req *http.Request, status int, code, message string)) {
	htmlErrors = fn
}

func (r *Router) checkAction(method, pattern string, action access.Action) error {
	if !action.Valid() {
		return fmt.Errorf("web: route %s %s has invalid action %q", method, pattern, action)
	}
	safe := method == http.MethodGet || method == http.MethodHead
	if safe && action.IsMutation() {
		return fmt.Errorf("web: safe route %s %s cannot carry mutation action %s", method, pattern, action)
	}
	if !safe && action == access.ActionRead {
		return fmt.Errorf("web: unsafe route %s %s cannot be READ", method, pattern)
	}
	r.routes[method+" "+pattern] = action
	return nil
}

// wrap applies per-route authorization (before the handler) and audit.
func (r *Router) wrap(method, pattern string, action access.Action, h http.Handler) http.Handler {
	if r.configGuard != nil && configGuarded(pattern) {
		h = configGuardHandler(r.configGuard, h)
	}
	if action.IsMutation() {
		h = r.guardWrites(action, h)
	}
	if r.audit != nil && action.IsMutation() {
		h = r.audit(action, pattern, h)
	}
	return middleware.Authorize(r.authz, action, func(req *http.Request) access.Resource {
		return resourceFor(pattern, req.PathValue)
	}, WriteError, h)
}

// Routes returns every registered "METHOD pattern" with its action.
func (r *Router) Routes() map[string]access.Action {
	out := make(map[string]access.Action, len(r.routes))
	for k, v := range r.routes {
		out[k] = v
	}
	return out
}

// SetAudit installs the audit wrapper for mutation routes registered
// afterwards (P17-40).
func (r *Router) SetAudit(fn func(action access.Action, pattern string, h http.Handler) http.Handler) {
	r.audit = fn
}

// CSRFToken returns the CSRF token of the request's browser session ("" when
// it has none) for rendering into pages.
func (r *Router) CSRFToken(req *http.Request) string {
	return r.csrf.Token(middleware.SessionFromContext(req.Context()))
}

func (r *Router) handleRoot(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		// Sub-paths under / that are not /static, /api, /health, /ready → 404.
		WriteError(w, req, NewNotFoundError("page"))
		return
	}
	if req.Method != http.MethodGet {
		WriteError(w, req, methodNotAllowed())
		return
	}
	if r.renderer != nil {
		_ = r.renderer.Render(w, req, "home", map[string]any{})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><title>Overseer</title>"))
}

func (r *Router) handleAPI(w http.ResponseWriter, req *http.Request) {
	r.apiMux.ServeHTTP(w, req)
}

func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		WriteError(w, req, methodNotAllowed())
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (r *Router) handleReady(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		WriteError(w, req, methodNotAllowed())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !r.initialized {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "not_ready", "checks": map[string]any{"application": "not_initialized"}})
		return
	}
	failures := map[string]any{}
	for name, check := range r.checks {
		ok, _ := check(req.Context())
		if !ok {
			failures[name] = "unavailable"
		}
	}
	if len(failures) == 0 {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "not_ready", "checks": failures})
}

// SetRenderer installs the page renderer; when set, GET / renders the home
// page through the base layout (WBS 1.10).
func (r *Router) SetRenderer(rnd *Renderer) {
	r.renderer = rnd
}

// SetReady marks the application as initialized (readiness gate).
func (r *Router) SetReady(v bool) { r.initialized = v }

// RegisterReadinessCheck adds a dependency check reported by /ready.
func (r *Router) RegisterReadinessCheck(name string, check ReadinessCheck) {
	r.checks[name] = check
}

func (r *Router) handleStatic(w http.ResponseWriter, req *http.Request) {
	rel := strings.TrimPrefix(req.URL.Path, "/static/")
	clean := path.Clean("/" + rel)
	full := path.Join(r.staticDir, clean)
	// Prevent path traversal outside staticDir.
	if !strings.HasPrefix(full, path.Clean(r.staticDir)) {
		WriteError(w, req, NewNotFoundError("static file"))
		return
	}
	f, err := os.Open(full)
	if err != nil {
		WriteError(w, req, NewNotFoundError("static file"))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		WriteError(w, req, NewNotFoundError("static file"))
		return
	}
	http.ServeContent(w, req, st.Name(), st.ModTime(), f)
}

func methodNotAllowed() error {
	return ErrMethodNotAllowed
}
