package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"overseer/internal/access"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compressionprovider"
	"overseer/internal/config"
	"overseer/internal/dbrepo"
	"overseer/internal/gateway/hermes"
	"overseer/internal/providermodels"
	"overseer/internal/readmodel"
	"overseer/internal/service"
	"overseer/internal/web"
	"overseer/internal/web/handlers"
	"overseer/internal/web/middleware"
)

// Version is the application version shown in the console shell and the
// system page (WBS 17.3, 17.107).
var Version = "0.18.0"

// secretValues returns the configured provider secret VALUES (read from
// the environment variables the configuration references) so responses,
// audit metadata and events can be scrubbed of them (WBS 17.122).
func (a *App) secretValues() []string {
	var out []string
	for _, p := range a.cfg.WorkerProviders {
		if ref := workerSecretRef(p); ref != "" {
			v, _ := a.secrets.Resolve(ref)
			out = append(out, v)
		}
	}
	for _, p := range a.cfg.CompressionProviders {
		if ref := compressionSecretRef(p); ref != "" {
			v, _ := a.secrets.Resolve(ref)
			out = append(out, v)
		}
	}
	if a.cfg.Web.CSRFKeyEnv != "" {
		out = append(out, os.Getenv(a.cfg.Web.CSRFKeyEnv))
	}
	if a.cfg.Database.Password != "" {
		out = append(out, a.cfg.Database.Password)
	}
	return out
}

// routerConfig builds the web boundary configuration from config.Web.
func (a *App) routerConfig() web.RouterConfig {
	w := a.cfg.Web
	var key []byte
	if w.CSRFKeyEnv != "" {
		key = []byte(os.Getenv(w.CSRFKeyEnv))
	}
	return web.RouterConfig{
		StaticDir:     a.opts.StaticDir,
		Authenticator: middleware.LocalAuthenticator{},
		Sessions: middleware.NewSessionManager(middleware.SessionConfig{TTL: w.SessionTTL, Rotate: w.SessionRotate,
			Secure: !w.CookieInsecure}),
		CSRFKey: key,
		Limits:  middleware.LimitConfig{DefaultBody: w.MaxBody, LargeBody: w.MaxDocumentBody},
		Rate:    middleware.RateConfig{PerSecond: w.RatePerSecond, Burst: w.RateBurst},
	}
}

// providerChecks builds reachability probes for the configured providers;
// endpoints stay inside the composition root (WBS 17.108).
func (a *App) providerChecks() []service.ProviderCheck {
	dial := func(raw string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			host := u.Host
			if u.Port() == "" {
				switch u.Scheme {
				case "https", "wss":
					host = net.JoinHostPort(u.Hostname(), "443")
				default:
					host = net.JoinHostPort(u.Hostname(), "80")
				}
			}
			var d net.Dialer
			c, err := d.DialContext(ctx, "tcp", host)
			if err != nil {
				return err
			}
			return c.Close()
		}
	}
	var out []service.ProviderCheck
	names := make([]string, 0, len(a.cfg.WorkerProviders))
	for n := range a.cfg.WorkerProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, service.ProviderCheck{Kind: "worker", Name: n, Check: dial(a.cfg.WorkerProviders[n].Endpoint)})
	}
	names = names[:0]
	for n := range a.cfg.CompressionProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, service.ProviderCheck{Kind: "compression", Name: n, Check: dial(a.cfg.CompressionProviders[n].BaseURL)})
	}
	return out
}

// startConsole wires the Phase 17 read models, live feed and console API.
func (a *App) startConsole(router *web.Router, setSvc *service.SettingsService, projSvc *service.ProjectService,
	matSvc *service.StateFileMaterializeService, roleSvc *service.RoleService) error {
	pool := a.db.Raw()
	a.startedAt = time.Now()
	webRepo := dbrepo.NewWebReadRepository(pool)
	execRepo := dbrepo.NewExecutionRepository(pool)
	recovery := service.NewRecoveryActionService(execRepo, a.engine, a.workerSvc).WithStore(dbrepo.NewRecoveryStore(pool)).WithOps(a.engine)
	console := service.NewConsoleReadService(service.ConsoleDeps{
		Web: webRepo, Execs: execRepo, Workers: a.workerSvc, Results: a.resultSvc, Continuations: a.contSvc,
		Engine: a.engine, Recovery: recovery, Inspector: a.repoInspector(), Settings: setSvc,
	})
	a.feed = service.NewLiveEventFeed(webRepo, service.LiveFeedConfig{Interval: a.cfg.Web.FeedInterval, Buffer: a.cfg.Web.FeedBuffer})
	planning := service.NewPlanningSettingsService(setSvc, a.engine)
	workerNames := func() []string {
		out := make([]string, 0, len(a.cfg.WorkerProviders))
		for n := range a.cfg.WorkerProviders {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
	compNames := func() []string {
		out := make([]string, 0, len(a.cfg.CompressionProviders))
		for n := range a.cfg.CompressionProviders {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
	templates := service.NewPromptTemplateService(dbrepo.NewPromptTemplateRepository(pool), dbrepo.NewPromptTemplateVersionRepository(pool)).
		WithTxPool(pool)
	workers := a.cfg.Engine.Workers
	if workers <= 0 {
		workers = 1
	}
	a.console = &web.ConsoleAPI{
		Console:  console,
		Feed:     a.feed,
		Audit:    a.auditSvc,
		Recovery: recovery,
		Drift:    service.NewDriftResolutionService(matSvc, setSvc, dbrepo.NewDriftAckRepository(pool)),
		ConfigDrift: service.NewConfigDriftService(dbrepo.NewExecutionRepository(pool), dbrepo.NewAppConfigSnapshotRepository(pool),
			dbrepo.NewSettingsRepository(pool), a.engine, a.configSnapshot),
		Integrity: a.integrity(),
		Templates: templates,
		Planning:  planning,
		Setup: service.NewProjectSetupService(projSvc, setSvc, planning, a.engine, workerNames, compNames).WithInitializer(&service.ProjectInitializer{
			Roles: roleSvc,
			Templates: service.NewStateTemplateService(dbrepo.NewStateMachineRepository(pool), dbrepo.NewWorkflowStateRepository(pool),
				dbrepo.NewStateTransitionRepository(pool), dbrepo.NewProjectRepository(pool)).WithTxPool(pool),
			States: dbrepo.NewWorkflowStateRepository(pool),
			Config: service.NewStateConfigService(dbrepo.NewStateMachineRepository(pool), dbrepo.NewWorkflowStateRepository(pool),
				dbrepo.NewStateTransitionRepository(pool), dbrepo.NewStateInstructionRepository(pool), dbrepo.NewStateRoleAssignmentRepository(pool),
				dbrepo.NewStateFileRequirementRepository(pool), dbrepo.NewRoleRepository(pool), dbrepo.NewTransitionHistoryRepository(pool),
				dbrepo.NewRuntimeStateRepository(pool)).WithTxPool(pool),
			Results:   a.resultSvc,
			Protocols: dbrepo.NewResponseProtocolRepository(pool),
		}),
		Metrics: service.NewMetricsService(webRepo, a.promptCacheSvc, a.compressionSvc, a.precomputeSvc),
		System: service.NewSystemStatusService(webRepo, func(ctx context.Context) error {
			ok, err := a.db.Ping(ctx)
			if err == nil && !ok {
				return context.DeadlineExceeded
			}
			return err
		}, a.providerChecks(), service.SystemStatusConfig{Version: Version, Profile: a.cfg.EffectiveProfile(), SchemaVersion: a.schemaVersion, InstanceID: a.instanceID, StartedAt: a.startedAt,
			SchedulerEnabled: !a.cfg.Engine.SchedulerDisabled, SchedulerInterval: a.cfg.Engine.Interval, ValidationCapacity: workers}).WithAudit(a.auditVerifier(), a.workGate()),
		AuditVerify: a.auditVerifier(),
		Restore:     a.restoreGate(),
		Health:      a.systemHealth(),
		Alerts:      a.alerts(),
		GC:          a.gc(),
		Lineage:     service.NewLineageService(dbrepo.NewLineageRepository(pool)),
		Readiness:   a.productionReadiness(),
		Diagnostics: service.NewDiagnosticsService(service.DiagnosticsDeps{Store: dbrepo.NewDiagnosticsRepository(pool), Health: a.systemHealth(),
			Alerts: a.alerts(), Audit: a.auditVerifier(), Gate: a.workGate(), Disk: a.diskMonitor(), RecentErrors: a.lg.RecentErrors,
			Redact: a.redactor.Value, Version: Version, Profile: a.cfg.EffectiveProfile(), SchemaVersion: a.schemaVersion, InstanceID: a.instanceID,
			ConfigHash: a.configSnapshot.Hash, ConfigSections: a.configSnapshot.Sections}),
	}
	a.console.Providers = a.providerInfo
	a.console.ProviderModels = a.providerModels
	a.console.ProviderTest = a.providerTest
	a.console.Workers = a.workerSvc
	a.console.Roles = roleSvc
	// Prometheus exposition (WBS 18.113): READ-authorized.
	exporter := a.metricsExporter()
	if err := router.RegisterPage(http.MethodGet, "/metrics", access.ActionRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if err := exporter.Write(r.Context(), &buf); err != nil {
			web.WriteError(w, r, web.NewInfrastructureError(err))
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	})); err != nil {
		return err
	}
	if err := web.RegisterConsoleRoutes(router, a.console); err != nil {
		return fmt.Errorf("app: register console routes: %w", err)
	}
	// Server-rendered console pages (WBS 17.2): they consume the API above
	// in-process.
	pages, err := handlers.New(handlers.Deps{API: router.Internal(), CSRF: router.CSRFToken, Version: Version,
		Logger: a.lg.WithComponent("console").Logger})
	if err != nil {
		return fmt.Errorf("app: console templates: %w", err)
	}
	if err := pages.Mount(router); err != nil {
		return fmt.Errorf("app: register console pages: %w", err)
	}
	return nil
}

// providerModels asks a configured provider for its models (the
// configured models list or GET <endpoint>/v1/models for a worker gateway, <base_url>/models for an
// OpenAI-compatible compressor) with the provider's own credential.
func (a *App) providerModels(ctx context.Context, kind, name string) ([]string, bool, error) {
	var url, ref string
	var tlsCfg *tls.Config
	switch kind {
	case "worker":
		p, ok := a.cfg.WorkerProviders[name]
		if !ok {
			return nil, false, nil
		}
		if len(p.Models) > 0 {
			return slices.Clone(p.Models), true, nil
		}
		hc, err := hermes.ParseConfig(hermesProviderConfig(p))
		if err == nil {
			tlsCfg, err = hc.ClientTLS()
		}
		if err != nil {
			return nil, true, err
		}
		url, ref = strings.TrimRight(p.Endpoint, "/")+"/v1/models", workerSecretRef(p)
	case "compression":
		p, ok := a.cfg.CompressionProviders[name]
		if !ok {
			return nil, false, nil
		}
		url, ref = strings.TrimRight(p.BaseURL, "/")+"/models", compressionSecretRef(p)
	default:
		return nil, false, nil
	}
	var token string
	if ref != "" {
		token, _ = a.secrets.Resolve(ref)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}}
	models, err := providermodels.List(ctx, client, url, token)
	return models, true, err
}

// connectionTestPrompt is the prompt the console's "Test connection"
// sends to a provider's model.
const connectionTestPrompt = "list your capabilities"

// connectionTestTimeout bounds one connection test (connect, session and
// the model's reply).
const connectionTestTimeout = 10 * time.Minute

// providerTest sends connectionTestPrompt to model on a configured
// provider: a worker gateway gets a fresh throwaway session (the project's
// worker session is untouched); a compressor gets one chat completion.
func (a *App) providerTest(ctx context.Context, kind, name, model string) (readmodel.ProviderTest, bool) {
	res := readmodel.ProviderTest{Kind: kind, Provider: name, Model: model, Prompt: connectionTestPrompt}
	var reply string
	var err error
	start := time.Now()
	switch kind {
	case "worker":
		p, ok := a.cfg.WorkerProviders[name]
		if !ok {
			return res, false
		}
		reply, err = a.testWorker(ctx, p, model)
	case "compression":
		if _, ok := a.cfg.CompressionProviders[name]; !ok {
			return res, false
		}
		reply, err = a.testCompressor(ctx, name, model)
	default:
		return res, false
	}
	res.DurationMS = time.Since(start).Milliseconds()
	key := providerTestKey(kind, name, model)
	if err != nil {
		a.providerTestsOK.Delete(key)
		res.Error = err.Error()
		return res, true
	}
	a.providerTestsOK.Store(key, struct{}{})
	res.OK, res.Response = true, reply
	return res, true
}

func providerTestKey(kind, provider, model string) string {
	return kind + "\x00" + provider + "\x00" + model
}

// providerTestPassed reports whether the latest connection test of model
// on provider succeeded.
func (a *App) providerTestPassed(kind, provider, model string) bool {
	_, ok := a.providerTestsOK.Load(providerTestKey(kind, provider, model))
	return ok
}

func (a *App) testWorker(ctx context.Context, p config.WorkerProviderConfig, model string) (string, error) {
	hc, err := hermes.ParseConfig(hermesProviderConfig(p))
	if err != nil {
		return "", err
	}
	hc.DialControl = endpointDialControl(a.cfg)
	ref := workerSecretRef(p)
	client, err := hermes.NewHTTPClientWithToken(hc, func() string {
		if ref == "" {
			return ""
		}
		v, _ := a.secrets.Resolve(ref)
		return v
	})
	if err != nil {
		return "", err
	}
	defer client.Close(ctx)
	ctx, cancel := context.WithTimeout(ctx, connectionTestTimeout)
	defer cancel()
	if _, err := client.Connect(ctx); err != nil {
		return "", err
	}
	sess, err := client.New(ctx, "connection-test")
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	resp, _, err := client.Send(ctx, sess.SessionID, hermes.GatewayRequest{RequestID: id, Model: model, Content: connectionTestPrompt}, id)
	if err != nil {
		return "", err
	}
	if resp.Content == nil {
		return "", errors.New("gateway reply has no content")
	}
	return *resp.Content, nil
}

func (a *App) testCompressor(ctx context.Context, name, model string) (string, error) {
	client, err := compressionprovider.NewFactory(a.cfg.CompressionProviders).WithResolver(a.secrets.Resolve).
		WithDialControl(endpointDialControl(a.cfg)).Client(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, connectionTestTimeout)
	defer cancel()
	return client.Ask(ctx, model, connectionTestPrompt)
}

// compressionSessionProbe reports why the compression provider cannot
// serve model: unreachable / rejected credentials, or a listing that does
// not include the model. A provider that lists no models is accepted.
func (a *App) compressionSessionProbe(ctx context.Context, provider, model string) error {
	models, found, err := a.providerModels(ctx, "compression", provider)
	switch {
	case !found:
		return fmt.Errorf("compression provider %q is not configured on this server", provider)
	case err != nil:
		return fmt.Errorf("provider %q is not reachable: %w", provider, err)
	case len(models) > 0 && !slices.Contains(models, model):
		return fmt.Errorf("model %q is not served by provider %q (available: %s)", model, provider, strings.Join(models, ", "))
	}
	return nil
}

// providerInfo lists configured providers without secrets or full
// endpoints (WBS 17.59 / 17.108 / 17.122).
func (a *App) providerInfo() []readmodel.ProviderInfo {
	host := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Host
	}
	var out []readmodel.ProviderInfo
	names := make([]string, 0, len(a.cfg.WorkerProviders))
	for n := range a.cfg.WorkerProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := a.cfg.WorkerProviders[n]
		out = append(out, readmodel.ProviderInfo{Kind: "worker", Name: n, ProviderKind: p.Kind, EndpointHost: host(p.Endpoint),
			SecretRef: p.AuthSecretEnv, SecretConfigured: p.AuthSecretEnv != "" && os.Getenv(p.AuthSecretEnv) != "",
			Capabilities: []string{"sessions", "cwd", "usage"}})
	}
	names = names[:0]
	for n := range a.cfg.CompressionProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := a.cfg.CompressionProviders[n]
		out = append(out, readmodel.ProviderInfo{Kind: "compression", Name: n, ProviderKind: p.Kind, EndpointHost: host(p.BaseURL),
			SecretRef: p.APIKeyEnv, SecretConfigured: p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "", Capabilities: []string{}})
	}
	return out
}
