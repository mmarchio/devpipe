package web

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// Server is the application HTTP server with finite timeouts and a graceful
// shutdown seam (WBS 1.7 "graceful shutdown"). It owns the listener and the
// http.Server; the lifecycle bootstrap (P1-12/P1-13) drives Start/Stop.
type Server struct {
	httpSrv *http.Server
	cfg     ServerConfig
	handler http.Handler

	// bound is set once the listener is bound (empty before that) and is
	// guarded by boundMu because Start runs in a goroutine while the
	// bootstrap polls it.
	boundMu   sync.Mutex
	boundAddr string
}

// BoundAddr returns the bound address once Start has bound the listener
// (empty before that). It is safe for concurrent use.
func (s *Server) BoundAddr() string {
	s.boundMu.Lock()
	defer s.boundMu.Unlock()
	return s.boundAddr
}

// NewServer builds a Server serving handler with the given timeout config.
// If cfg is zero, DefaultServerConfig is used.
func NewServer(cfg ServerConfig, handler http.Handler) *Server {
	if cfg.ReadTimeout <= 0 {
		cfg = DefaultServerConfig()
	}
	return &Server{
		cfg:     cfg,
		handler: handler,
		httpSrv: &http.Server{
			Handler:           handler,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			MaxHeaderBytes:    cfg.MaxHeaderBytes,
		},
	}
}

// Start binds the listener, records the address in BoundAddr on success,
// and serves until the context is cancelled, Stop is called, or the
// listener dies. It blocks; callers run it in a goroutine.
func (s *Server) Start(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.boundMu.Lock()
	s.boundAddr = ln.Addr().String()
	s.boundMu.Unlock()
	if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop gracefully stops the server, draining in-flight requests within the
// provided context. It is safe to call more than once.
func (s *Server) Stop(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// Addr returns the configured timeout config (exposed for tests/lifecycle).
func (s *Server) Config() ServerConfig { return s.cfg }
