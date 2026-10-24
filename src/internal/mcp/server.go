// Package mcp is the overseer MCP server (Phase 19): Hermes connects to it
// as an MCP client over Streamable HTTP to read its working context and
// submit structured progress, blockers and results.
//
// Two credentials guard it. The bearer token (mcp.auth_secret_env)
// admits the Hermes container to the listener. The capability token the
// engine writes into each command binds every tool call to that command's
// in-flight dispatch; an execution id in the arguments is never trusted.
// The server is stateless — every call is bound by its token, so an
// overseer restart loses no MCP session state.
package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"overseer/internal/config"
	"overseer/internal/service"
)

// Path is the MCP endpoint.
const Path = "/mcp"

// Version is the server version reported in the handshake.
const Version = "1.0.0"

// Server is the MCP listener.
type Server struct {
	cfg    config.MCPConfig
	svc    *service.MCPService
	logger *slog.Logger
	secret func() string

	mu   sync.Mutex
	srv  *http.Server
	ln   net.Listener
	done chan struct{}
}

// New builds the server; secret returns the bearer token (re-read per
// request, so rotation needs no restart). A nil secret reads
// cfg.AuthSecretEnv.
func New(cfg config.MCPConfig, svc *service.MCPService, logger *slog.Logger, secret func() string) *Server {
	cfg = cfg.Effective()
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if secret == nil {
		env := cfg.AuthSecretEnv
		secret = func() string { return os.Getenv(env) }
	}
	return &Server{cfg: cfg, svc: svc, logger: logger.With("component", "mcp"), secret: secret}
}

// Handler is the HTTP surface: GET /healthz (no auth) and the MCP
// endpoint behind the bearer check.
func (s *Server) Handler() http.Handler {
	ms := sdk.NewServer(&sdk.Implementation{Name: "overseer", Version: Version}, &sdk.ServerOptions{
		Instructions: "Overseer workflow tools. Pass the token from the overseer command to every tool. " +
			"Read context with get_current_state, get_task, get_context, get_role and get_execution_history; " +
			"report with report_progress and report_blocker; finish with submit_result. The overseer decides every transition.",
	})
	registerTools(ms, s)
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return ms }, &sdk.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, Logger: s.logger,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle(Path, s.authenticate(h))
	return mux
}

// authenticate requires "Authorization: Bearer <secret>"; with no secret
// configured every request is refused.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := s.secret()
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if want == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			s.logger.WarnContext(r.Context(), "mcp request rejected", "reason", "bearer token", "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="overseer-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Start listens on cfg.Addr and serves until Shutdown.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) }}
	s.mu.Lock()
	s.srv, s.ln, s.done = srv, ln, make(chan struct{})
	s.mu.Unlock()
	go func() {
		defer close(s.done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("mcp server stopped", "error", err.Error())
		}
	}()
	s.logger.InfoContext(ctx, "mcp server listening", "addr", ln.Addr().String(), "path", Path)
	return nil
}

// Addr is the bound listener address ("" before Start).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Shutdown stops accepting requests and waits (bounded by ctx) for the
// tool calls in progress to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv, done := s.srv, s.done
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	<-done
	s.logger.InfoContext(ctx, "mcp server stopped")
	return err
}
