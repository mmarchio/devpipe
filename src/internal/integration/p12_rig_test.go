package integration

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/compression"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/worker"
)

// Phase 12 integration rig: the Phase 11 project (runtime at IMPLEMENT
// 11.3, real worker prompts from the Phase 9/10 pipeline) plus a fake
// Hermes Gateway configured as the project's worker provider, reached
// through a stoppable TCP proxy (deterministic "gateway down").

type p12Proxy struct {
	mu     sync.Mutex
	addr   string
	target *url.URL
	srv    *http.Server
	ln     net.Listener
}

func newP12Proxy(t *testing.T, target string) *p12Proxy {
	t.Helper()
	u, _ := url.Parse(target)
	p := &p12Proxy{target: u}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.Stop)
	return p
}

func (p *p12Proxy) URL() string { return "http://" + p.addr }

func (p *p12Proxy) serve(ln net.Listener) {
	rp := httputil.NewSingleHostReverseProxy(p.target)
	rp.FlushInterval = -1
	srv := &http.Server{Handler: rp}
	p.mu.Lock()
	p.srv, p.ln = srv, ln
	p.mu.Unlock()
	go srv.Serve(ln)
}

// Stop makes the gateway unreachable (connection refused).
func (p *p12Proxy) Stop() {
	p.mu.Lock()
	srv := p.srv
	p.srv = nil
	p.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

// Start makes the gateway reachable again on the same address.
func (p *p12Proxy) Start(t *testing.T) {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("proxy restart: %v", err)
	}
	p.serve(ln)
}

type p12Rig struct {
	*p11Rig
	g       *hermestest.Gateway
	proxy   *p12Proxy
	runtime uuid.UUID
}

func p12Mutate(srv *providertest.Server, endpoint string) func(*config.Config) {
	return func(c *config.Config) {
		c.CompressionProviders = map[string]config.CompressionProviderConfig{
			"fake": {Kind: config.CompressionKindOpenAICompatible, BaseURL: srv.URL + "/v1", APIKeyEnv: "P9_FAKE_KEY"},
		}
		c.WorkerProviders = map[string]config.WorkerProviderConfig{
			"hermes": {Kind: config.WorkerKindHermesGateway, Endpoint: endpoint, ConnectTimeout: 2 * time.Second, AuthSecretEnv: "P12_GATEWAY_TOKEN",
				Reconnect: config.WorkerReconnectConfig{MaxAttempts: 2, InitialDelay: 5 * time.Millisecond, MaxDelay: 20 * time.Millisecond}},
		}
	}
}

const p12Token = "tok-P12-GATEWAY-SENTINEL"

func p12Gateway(t *testing.T) (*hermestest.Gateway, *p12Proxy) {
	t.Helper()
	g := hermestest.New()
	t.Cleanup(g.Close)
	g.Token = p12Token
	t.Setenv("P12_GATEWAY_TOKEN", p12Token)
	t.Setenv("P9_FAKE_KEY", p9SentinelKey)
	proxy := newP12Proxy(t, g.URL)
	p12Proxies.Store(g, proxy)
	t.Cleanup(func() { p12Proxies.Delete(g) })
	return g, proxy
}

// p12Proxies maps a fake gateway to its transport proxy (tests that cut
// the connection, e.g. a definitely-unsent dispatch).
var p12Proxies sync.Map

// proxyOf returns the proxy in front of g.
func proxyOf(t *testing.T, g *hermestest.Gateway) *p12Proxy {
	t.Helper()
	v, ok := p12Proxies.Load(g)
	if !ok {
		t.Fatal("no proxy for gateway")
	}
	return v.(*p12Proxy)
}

// p12Configure points a p11 project at the worker provider.
func p12Configure(t *testing.T, r *p11Rig, cwd string) *p12Rig {
	t.Helper()
	ctx := context.Background()
	pid := r.pos.ProjectID.String()
	if _, err := r.h.DBExec(ctx, "UPDATE project_settings SET worker_provider = 'hermes', worker_model = 'hermes-worker', worker_timeout = 10000000000, "+
		"context_window = 64000, working_directory = '"+cwd+"' WHERE project_id = '"+pid+"'"); err != nil {
		t.Fatal(err)
	}
	var rt uuid.UUID
	if err := r.h.DBScan(ctx, "SELECT id FROM runtime_state WHERE project_id = '"+pid+"'", &rt); err != nil {
		t.Fatal(err)
	}
	return &p12Rig{p11Rig: r, runtime: rt}
}

func p12NewRig(t *testing.T) *p12Rig {
	t.Helper()
	srv := providertest.New()
	t.Cleanup(srv.Close)
	g, proxy := p12Gateway(t)
	h := NewHarnessWith(t, p12Mutate(srv, proxy.URL()))
	r := p12Configure(t, p11Build(t, h, srv, true), "/repos/project-a")
	r.g, r.proxy = g, proxy
	return r
}

// p12RestartRig boots an app on a dedicated database; boot() starts a new
// overseer instance (restart or second instance) on the same database and
// gateway.
func p12RestartRig(t *testing.T) (*p12Rig, func() *Harness) {
	t.Helper()
	srv := providertest.New()
	t.Cleanup(srv.Close)
	g, proxy := p12Gateway(t)
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })
	boot := func() *Harness {
		h := NewHarnessOnWith(t, base, dbName, p12Mutate(srv, proxy.URL()))
		t.Cleanup(h.Close)
		return h
	}
	h := boot()
	r := p12Configure(t, p11Build(t, h, srv, true), "/repos/a")
	r.g, r.proxy = g, proxy
	return r, boot
}

func (r *p12Rig) ws() *service.WorkerService { return r.h.App.Worker() }

// prompt prepares the worker prompt of the current position (Phase 10/11
// pipeline); a non-empty tag first changes ARCHITECTURE so the prompt is
// new.
func (r *p12Rig) prompt(t *testing.T, tag string) *compression.WorkerPrompt {
	t.Helper()
	if tag != "" {
		(&p10Env{h: r.h, pos: r.pos}).writeFile(t, "ARCHITECTURE", "ARCHITECTURE "+tag+"\n"+p9Background("ARCH-"+tag, 40))
	}
	res := r.prepareNext(t, service.NextRequest{})
	if res.WorkerPrompt == nil {
		t.Fatal("no worker prompt prepared")
	}
	return res.WorkerPrompt
}

func (r *p12Rig) req(wp *compression.WorkerPrompt, attempt int) service.DispatchRequest {
	u, ph := r.units["11.3"], r.pos.PhaseID
	return service.DispatchRequest{ProjectID: r.pos.ProjectID, PhaseID: &ph, WorkUnitID: &u, WorkerPromptID: wp.ID, RuntimeStateID: r.runtime,
		StateKey: "IMPLEMENT", RoleKey: "ENGINEER", Attempt: attempt}
}

func (r *p12Rig) dispatch(t *testing.T, wp *compression.WorkerPrompt, attempt int) *service.DispatchResult {
	t.Helper()
	res, err := r.ws().Dispatch(context.Background(), r.req(wp, attempt))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return res
}

func (r *p12Rig) newSession(t *testing.T) *persistence.WorkerSessionRecord {
	t.Helper()
	s, err := r.ws().NewSession(context.Background(), r.pos.ProjectID)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	return s
}

func (r *p12Rig) cd(t *testing.T, path string) *persistence.WorkerSessionRecord {
	t.Helper()
	s, err := r.ws().ChangeDirectory(context.Background(), r.pos.ProjectID, path)
	if err != nil {
		t.Fatalf("cd %s: %v", path, err)
	}
	return s
}

func (r *p12Rig) session(t *testing.T, id uuid.UUID) *persistence.WorkerSessionRecord {
	t.Helper()
	s, err := r.ws().Session(context.Background(), r.pos.ProjectID, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (r *p12Rig) events(t *testing.T) []worker.EventType {
	t.Helper()
	evs, err := r.ws().Events(context.Background(), persistence.WorkerEventFilter{ProjectID: r.pos.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]worker.EventType, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func hasEvent(evs []worker.EventType, want worker.EventType) bool {
	for _, e := range evs {
		if e == want {
			return true
		}
	}
	return false
}

// p12Messages counts gateway message sends carrying a request id.
func p12Sends(g *hermestest.Gateway, requestID uuid.UUID) int {
	n := 0
	for _, m := range g.Messages() {
		if m.RequestID == requestID.String() {
			n++
		}
	}
	return n
}

func p12Code(err error) worker.WorkerErrorCode {
	var we *worker.WorkerError
	if errors.As(err, &we) {
		return we.Code
	}
	return ""
}

func p12NoSecret(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, p12Token) {
		t.Fatalf("%s contains the gateway token", where)
	}
}
