package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the application configuration.
type Config struct {
	// Profile is the runtime profile: development (default), test or
	// production (WBS 18.1). Production refuses development-only settings.
	Profile string `yaml:"profile"`

	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Logging  LoggingConfig  `yaml:"logging"`
	Shutdown ShutdownConfig `yaml:"shutdown"`

	// CompressionProviders are the named prompt-compression endpoints (WBS
	// 9.39/9.40). A project's compression_provider setting selects one by
	// name. Credentials are never stored here: APIKeyEnv names the
	// environment variable read at call time (WBS 9.41).
	CompressionProviders map[string]CompressionProviderConfig `yaml:"compression_providers"`

	// WorkerProviders are the named worker endpoints (WBS 12.3). A
	// project's worker_provider setting selects one by name. The gateway
	// token is never stored here: AuthSecretEnv names the environment
	// variable read at connect time (WBS 12.94).
	WorkerProviders map[string]WorkerProviderConfig `yaml:"worker_providers"`

	// Continuation configures the Phase 14 continuation runner.
	Continuation ContinuationConfig `yaml:"continuation"`

	// Bootstrap configures Phase 15 execution bootstrap.
	Bootstrap BootstrapConfig `yaml:"bootstrap"`

	// Engine configures the Phase 16 execution engine scheduler.
	Engine EngineConfig `yaml:"engine"`

	// Web configures the Phase 17 console boundary.
	Web WebConfig `yaml:"web"`

	// Security configures Phase 18 deployment policies.
	Security SecurityConfig `yaml:"security"`

	// Backup configures automatic logical backups (Phase 18).
	Backup BackupConfig `yaml:"backup"`

	// Disk configures the disk usage monitor (Phase 18).
	Disk DiskConfig `yaml:"disk"`

	// GC configures artifact retention (Phase 18).
	GC GCConfig `yaml:"gc"`

	// MCP configures the overseer MCP server Hermes connects to (Phase
	// 19).
	MCP MCPConfig `yaml:"mcp"`

	// Projects holds defaults applied to new projects.
	Projects ProjectDefaults `yaml:"projects"`

	// Faults is the test-only fault-injection spec (OVERSEER_FAULTS,
	// WBS 18.141); only the test profile accepts it.
	Faults string `yaml:"-"`
}

// Profiles (WBS 18.1).
const (
	ProfileDevelopment = "development"
	ProfileTest        = "test"
	ProfileProduction  = "production"
)

// BackupConfig configures automatic logical backups (WBS 18.140,
// decision U5).
type BackupConfig struct {
	// Dir receives the automatic pre-migration backups (one subdirectory
	// per backup). Production startup with pending migrations requires
	// it: the backup is taken before migrating, and a failure stops
	// startup. Development / test skip the automatic backup.
	Dir string `yaml:"dir"`
}

// DiskConfig configures disk usage monitoring (WBS 18.97, decision U7):
// at WarnPercent an alert is raised; at CriticalPercent executions WAIT
// before a new worker dispatch, validation or checkpoint, and resume by
// themselves once space is freed.
type DiskConfig struct {
	WarnPercent     float64       `yaml:"warn_percent"`     // 0 = 85
	CriticalPercent float64       `yaml:"critical_percent"` // 0 = 95
	Interval        time.Duration `yaml:"interval"`         // 0 = 1m
}

// Effective returns the thresholds and interval with defaults applied.
func (d DiskConfig) Effective() (warn, critical float64, interval time.Duration) {
	warn, critical, interval = d.WarnPercent, d.CriticalPercent, d.Interval
	if warn <= 0 {
		warn = 85
	}
	if critical <= 0 {
		critical = 95
	}
	if interval <= 0 {
		interval = time.Minute
	}
	return warn, critical, interval
}

// MCPConfig configures the overseer MCP server (Phase 19): Streamable
// HTTP on its own listener, separate from the console, so it can be
// exposed to the Hermes container only. Every request carries the bearer
// token read from AuthSecretEnv (never stored here); each tool call is
// further bound to a dispatch by the capability token in its command.
type MCPConfig struct {
	Enabled bool `yaml:"enabled"`
	// Addr is host:port of the listener (default 127.0.0.1:9142).
	Addr          string `yaml:"addr"`
	AuthSecretEnv string `yaml:"auth_secret_env"`
	// TokenTTL bounds a capability token's life (0 = 24h).
	TokenTTL time.Duration `yaml:"token_ttl"`
	// MaxResponseBytes caps a tool result (0 = 64 KiB).
	MaxResponseBytes int `yaml:"max_response_bytes"`
	// RequestTimeout bounds one tool call (0 = 30s).
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// Effective returns the settings with defaults applied.
func (m MCPConfig) Effective() MCPConfig {
	if m.Addr == "" {
		m.Addr = "127.0.0.1:9142"
	}
	if m.TokenTTL <= 0 {
		m.TokenTTL = 24 * time.Hour
	}
	if m.MaxResponseBytes <= 0 {
		m.MaxResponseBytes = 64 << 10
	}
	if m.RequestTimeout <= 0 {
		m.RequestTimeout = 30 * time.Second
	}
	return m
}

func (m MCPConfig) validate() []error {
	if !m.Enabled {
		return nil
	}
	var errs []error
	if _, port, err := net.SplitHostPort(m.Effective().Addr); err != nil || port == "" {
		errs = append(errs, fmt.Errorf("config: mcp.addr %q must be host:port", m.Addr))
	}
	if m.AuthSecretEnv == "" || !envNameRE.MatchString(m.AuthSecretEnv) {
		errs = append(errs, fmt.Errorf("config: mcp.auth_secret_env must name an environment variable (the MCP bearer token)"))
	}
	if m.TokenTTL < 0 || m.RequestTimeout < 0 || m.MaxResponseBytes < 0 {
		errs = append(errs, fmt.Errorf("config: mcp.token_ttl, mcp.request_timeout and mcp.max_response_bytes must not be negative"))
	}
	return errs
}

// GCConfig is the artifact retention policy (WBS 18.98–18.104, decision
// U2); 0 = default. Audit is never collected.
type GCConfig struct {
	CacheRetention         time.Duration `yaml:"cache_retention"`          // 7d
	PrecomputeRetention    time.Duration `yaml:"precompute_retention"`     // 24h
	ValidationLogRetention time.Duration `yaml:"validation_log_retention"` // 30d
}

// SecurityConfig holds deployment security policies (Phase 18).
type SecurityConfig struct {
	// ProviderEndpoints restricts provider endpoints (WBS 18.88).
	ProviderEndpoints EndpointPolicy `yaml:"provider_endpoints"`
}

// EndpointPolicy restricts where provider HTTP calls may go. Enabled
// turns the policy on (always on in production for link-local / cloud
// metadata addresses); AllowPrivate permits loopback / private ranges;
// AllowedHosts, when non-empty, is an allow-list of host names.
type EndpointPolicy struct {
	Enabled      bool     `yaml:"enabled"`
	AllowPrivate bool     `yaml:"allow_private"`
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// ProjectDefaults are applied when a project is created.
type ProjectDefaults struct {
	// ContextThreshold is the default context-usage reset threshold
	// (0 < x ≤ 1; 0 = 0.8).
	ContextThreshold float64 `yaml:"default_context_threshold"`
}

// WebConfig configures the Phase 17 web console (WBS 17.115–17.161). Zero
// values select the defaults noted per field.
type WebConfig struct {
	// AuthMode selects the authenticator: "local" (default) = every
	// request acts as the local administrator.
	AuthMode string `yaml:"auth_mode"`
	// CookieInsecure drops the session cookie Secure attribute (plain-http
	// local use only; default false = Secure).
	CookieInsecure bool `yaml:"cookie_insecure"`
	// SessionTTL (default 12h) / SessionRotate (default 1h).
	SessionTTL    time.Duration `yaml:"session_ttl"`
	SessionRotate time.Duration `yaml:"session_rotate"`
	// CSRFKeyEnv names an environment variable holding the CSRF key
	// (default: random per process).
	CSRFKeyEnv string `yaml:"csrf_key_env"`
	// MaxBody / MaxDocumentBody bound request bodies (defaults 1 MiB /
	// 2 MiB).
	MaxBody         int64 `yaml:"max_body"`
	MaxDocumentBody int64 `yaml:"max_document_body"`
	// RatePerSecond / RateBurst limit expensive endpoints (defaults 20 / 60).
	RatePerSecond float64 `yaml:"rate_per_second"`
	RateBurst     float64 `yaml:"rate_burst"`
	// FeedInterval (default 500ms) / FeedBuffer (default 256) tune the SSE
	// live feed.
	FeedInterval time.Duration `yaml:"feed_interval"`
	FeedBuffer   int           `yaml:"feed_buffer"`
	// TrustedLocal acknowledges the local (single-operator, no login)
	// authenticator in production; it additionally requires a loopback
	// bind (WBS 18.79).
	TrustedLocal bool `yaml:"trusted_local"`
}

// EngineConfig tunes the Phase 16 engine. SchedulerDisabled stops
// autonomous scheduling (executions still advance through explicit
// step / run requests); RecoveryDisabled skips the startup reconciliation.
type EngineConfig struct {
	SchedulerDisabled bool          `yaml:"scheduler_disabled"`
	RecoveryDisabled  bool          `yaml:"recovery_disabled"`
	Interval          time.Duration `yaml:"interval"`
	Workers           int           `yaml:"workers"`
	// MaxConcurrentDispatches bounds simultaneous worker dispatches across
	// all executions (WBS 18.92; 0 = unlimited).
	MaxConcurrentDispatches int `yaml:"max_concurrent_dispatches"`
	// MaxConcurrentValidations bounds simultaneous validation command runs
	// (WBS 18.93; 0 = default max(1, NumCPU/2), see EffectiveMaxValidations).
	MaxConcurrentValidations int `yaml:"max_concurrent_validations"`
	// MaxConcurrentCompressions bounds simultaneous compression provider
	// runs (WBS 18.94; 0 = unlimited). Speculative precompute work yields
	// to active execution (WBS 18.95).
	MaxConcurrentCompressions int `yaml:"max_concurrent_compressions"`
	// GitTimeout bounds every git invocation (WBS 18.60; 0 = 2m).
	GitTimeout time.Duration `yaml:"git_timeout"`
	// RunGitHooks runs the project repository's commit hooks for overseer
	// checkpoints (WBS 18.61). Off by default: hooks are repository-
	// controlled code, and a worker can edit the repository. When on, a
	// rejecting hook fails the checkpoint (REQUIRED then blocks DONE).
	RunGitHooks bool `yaml:"run_git_hooks"`
	// ValidationEnvAllow names extra environment variables validation
	// commands inherit beyond the built-in allow-list (WBS 18.63).
	ValidationEnvAllow []string `yaml:"validation_env_allow"`
	// StallThreshold: a runnable execution without step progress for
	// this long raises SCHEDULER_STALLED (WBS 18.119; 0 = 10m).
	StallThreshold time.Duration `yaml:"stall_threshold"`
	// AlertInterval is the periodic alert evaluation interval (WBS 18.118;
	// 0 = 30s).
	AlertInterval time.Duration `yaml:"alert_interval"`
	// IntegrityScanInterval is the periodic background integrity scan
	// interval (WBS 18.180; 0 = 15m) and IntegrityScanBatch the executions
	// checked per tick (0 = 50).
	IntegrityScanInterval time.Duration `yaml:"integrity_scan_interval"`
	IntegrityScanBatch    int           `yaml:"integrity_scan_batch"`
	// WorkerReservedPaths are project-relative paths the worker maintains
	// for itself (Hermes's self-learned role / skill definitions): never
	// committed by overseer checkpoints and ignored by repository
	// evidence and dirty checks. Unset = [".hermes"].
	WorkerReservedPaths []string `yaml:"worker_reserved_paths"`
}

// EffectiveMaxValidations is the validation concurrency limit (WBS
// 18.93): the configured value, or max(1, NumCPU/2) when unset.
func (e EngineConfig) EffectiveMaxValidations() int {
	if e.MaxConcurrentValidations > 0 {
		return e.MaxConcurrentValidations
	}
	return max(1, runtime.NumCPU()/2)
}

// DefaultWorkerReservedPath is the worker's reserved directory when none
// is configured.
const DefaultWorkerReservedPath = ".hermes"

// ReservedPaths returns the effective worker-reserved paths (cleaned).
func (e EngineConfig) ReservedPaths() []string {
	if e.WorkerReservedPaths == nil {
		return []string{DefaultWorkerReservedPath}
	}
	out := make([]string, 0, len(e.WorkerReservedPaths))
	for _, p := range e.WorkerReservedPaths {
		out = append(out, filepath.Clean(strings.TrimSpace(p)))
	}
	return out
}

// BootstrapConfig tunes Phase 15 bootstrap. RecoveryDisabled skips the
// startup resume of interrupted bootstraps (tests drive recovery
// explicitly).
type BootstrapConfig struct {
	RecoveryDisabled bool `yaml:"recovery_disabled"`
}

// ContinuationConfig tunes the Phase 14 continuation runner. Disabled
// stops automatic execution (continuations are still created and can be
// processed explicitly); PollInterval is how often claimable continuations
// are looked up.
type ContinuationConfig struct {
	Disabled     bool          `yaml:"disabled"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Workers      int           `yaml:"workers"`
}

// WorkerProviderConfig is one worker endpoint (Phase 12).
type WorkerProviderConfig struct {
	Kind          string `yaml:"kind"`
	Endpoint      string `yaml:"endpoint"`
	AuthSecretEnv string `yaml:"auth_secret_env"`
	// AuthSecretFile names a file holding the gateway token, re-read
	// when it changes (rotation, WBS 18.78). Mutually exclusive with
	// AuthSecretEnv.
	AuthSecretFile   string                 `yaml:"auth_secret_file"`
	ConnectTimeout   time.Duration          `yaml:"connect_timeout"`
	SessionTimeout   time.Duration          `yaml:"session_timeout"`
	MaxResponseBytes int64                  `yaml:"max_response_bytes"`
	Reconnect        WorkerReconnectConfig  `yaml:"reconnect"`
	TLS              *WorkerProviderTLSConf `yaml:"tls"`
	AllowedRoots     []string               `yaml:"allowed_roots"`
	// Models lists the models the console offers for this provider. When
	// set it replaces the gateway's own listing (GET /v1/models), which
	// does not report the models of the gateway's upstream providers.
	Models []string `yaml:"models"`
	// MaxConcurrent bounds simultaneous dispatches to this provider
	// (WBS 18.92; 0 = no per-provider limit).
	MaxConcurrent int `yaml:"max_concurrent"`
	// Circuit tunes the provider circuit breaker (WBS 18.27 / 18.28).
	Circuit CircuitConfig `yaml:"circuit"`
}

// CircuitConfig tunes a provider circuit breaker (zero = defaults: 5
// failures within 1m open it; probe after 30s; 1 success closes it).
type CircuitConfig struct {
	Failures  int           `yaml:"failures"`
	Window    time.Duration `yaml:"window"`
	Cooldown  time.Duration `yaml:"cooldown"`
	Successes int           `yaml:"successes"`
}

// WorkerReconnectConfig bounds transport reconnection (WBS 12.40).
type WorkerReconnectConfig struct {
	MaxAttempts  int           `yaml:"max_attempts"`
	InitialDelay time.Duration `yaml:"initial_delay"`
	MaxDelay     time.Duration `yaml:"max_delay"`
}

// WorkerProviderTLSConf is the gateway TLS configuration (WBS 12.95).
type WorkerProviderTLSConf struct {
	CAFile     string `yaml:"ca_file"`
	ServerName string `yaml:"server_name"`
	MinVersion string `yaml:"min_version"`
}

// WorkerKindHermesGateway is the supported worker provider kind.
const WorkerKindHermesGateway = "hermes_gateway"

// CompressionProviderConfig is one OpenAI-compatible compression endpoint
// (cloud, LAN or local — llama.cpp, Ollama, vLLM).
type CompressionProviderConfig struct {
	Kind      string `yaml:"kind"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	// APIKeyFile names a file holding the API key, re-read when it
	// changes (rotation, WBS 18.78). Mutually exclusive with APIKeyEnv.
	APIKeyFile string        `yaml:"api_key_file"`
	Timeout    time.Duration `yaml:"timeout"`
	// MaxOutputTokens caps the completion (0 = provider default).
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// Circuit tunes the provider circuit breaker (WBS 18.29 / 18.27).
	Circuit CircuitConfig `yaml:"circuit"`
}

// CompressionKindOpenAICompatible is the supported provider kind.
const CompressionKindOpenAICompatible = "openai_compatible"

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// HTTP timeouts (WBS 18.90; zero = defaults: read header 5s, read
	// 15s, write 30s, idle 60s). Streaming responses extend their own
	// write deadline.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"ssl_mode"`
	// Pool sizing (WBS 18.91; zero = defaults: max 10, min 0, lifetime
	// 1h, idle 30m, application_name "overseer"). Waiting for a
	// connection is bounded by each caller's context.
	MaxConns        int           `yaml:"max_conns"`
	MinConns        int           `yaml:"min_conns"`
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime"`
	MaxConnIdleTime time.Duration `yaml:"max_conn_idle_time"`
	ApplicationName string        `yaml:"application_name"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type ShutdownConfig struct {
	Timeout time.Duration `yaml:"timeout"`
	// WorkerDrain bounds how long shutdown waits for in-flight steps
	// (worker calls included) to finish and be recorded before cancelling
	// them; whatever is still running is left reconcilable (WBS 18.125 /
	// 18.126, decision U6; 0 = 2m).
	WorkerDrain time.Duration `yaml:"worker_drain"`
}

// EffectiveWorkerDrain is the drain bound with its default.
func (s ShutdownConfig) EffectiveWorkerDrain() time.Duration {
	if s.WorkerDrain <= 0 {
		return 2 * time.Minute
	}
	return s.WorkerDrain
}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
var validLogFormats = map[string]bool{"json": true, "text": true}

// Default returns the WBS default configuration.
func Default() Config {
	return Config{
		Server:       ServerConfig{Host: "0.0.0.0", Port: 8080},
		Database:     DatabaseConfig{Host: "172.17.0.1", Port: 5432, Name: "overseer", User: "overseer", SSLMode: "disable"},
		Logging:      LoggingConfig{Level: "info", Format: "json"},
		Shutdown:     ShutdownConfig{Timeout: 30 * time.Second},
		Continuation: ContinuationConfig{PollInterval: time.Second, Workers: 4},
	}
}

// Load builds configuration in priority order: defaults, then the YAML file
// (if it exists), then OVERSEER_* environment variables, and finally
// validation. Any error means the configuration is not usable and
// infrastructure initialization must not proceed (T1.2.8).
func Load(path string) (Config, error) {
	cfg := Default()

	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: parsing %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("config: reading %s: %w", path, err)
	}

	if envErrs := applyEnv(&cfg); len(envErrs) > 0 {
		errs := make([]error, 0, len(envErrs))
		for _, e := range envErrs {
			errs = append(errs, errors.New(e))
		}
		return Config{}, errors.Join(errs...)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) []string {
	var errs []string
	setStr := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = v
		}
	}
	setInt := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			} else {
				errs = append(errs, fmt.Sprintf("config: %s: %q is not a valid integer", key, v))
			}
		}
	}
	setDur := func(key string, dst *time.Duration) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*dst = rawDuration(v)
		}
	}

	setStr("OVERSEER_PROFILE", &cfg.Profile)
	if v, ok := os.LookupEnv("OVERSEER_FAULTS"); ok {
		cfg.Faults = v
	}
	setStr("OVERSEER_SERVER_HOST", &cfg.Server.Host)
	setStr("OVERSEER_BACKUP_DIR", &cfg.Backup.Dir)
	setInt("OVERSEER_SERVER_PORT", &cfg.Server.Port)
	if v, ok := os.LookupEnv("OVERSEER_MCP_ENABLED"); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.MCP.Enabled = b
		} else {
			errs = append(errs, fmt.Sprintf("config: OVERSEER_MCP_ENABLED: %q is not a valid boolean", v))
		}
	}
	setStr("OVERSEER_MCP_ADDR", &cfg.MCP.Addr)
	setStr("OVERSEER_MCP_AUTH_SECRET_ENV", &cfg.MCP.AuthSecretEnv)
	setDur("OVERSEER_MCP_TOKEN_TTL", &cfg.MCP.TokenTTL)
	setDur("OVERSEER_MCP_REQUEST_TIMEOUT", &cfg.MCP.RequestTimeout)
	setInt("OVERSEER_MCP_MAX_RESPONSE_BYTES", &cfg.MCP.MaxResponseBytes)
	setStr("OVERSEER_DATABASE_HOST", &cfg.Database.Host)
	setInt("OVERSEER_DATABASE_PORT", &cfg.Database.Port)
	setStr("OVERSEER_DATABASE_NAME", &cfg.Database.Name)
	setStr("OVERSEER_DATABASE_USER", &cfg.Database.User)
	setStr("OVERSEER_DATABASE_PASSWORD", &cfg.Database.Password)
	setStr("OVERSEER_DATABASE_SSL_MODE", &cfg.Database.SSLMode)
	setInt("OVERSEER_DATABASE_MAX_CONNS", &cfg.Database.MaxConns)
	setStr("OVERSEER_DATABASE_APPLICATION_NAME", &cfg.Database.ApplicationName)
	setStr("OVERSEER_LOGGING_LEVEL", &cfg.Logging.Level)
	setStr("OVERSEER_LOGGING_FORMAT", &cfg.Logging.Format)
	setDur("OVERSEER_SHUTDOWN_TIMEOUT", &cfg.Shutdown.Timeout)
	setDur("OVERSEER_CONTINUATION_POLL_INTERVAL", &cfg.Continuation.PollInterval)
	if v, ok := os.LookupEnv("OVERSEER_CONTINUATION_DISABLED"); ok && v != "" {
		cfg.Continuation.Disabled = v == "true" || v == "1"
	}
	if v, ok := os.LookupEnv("OVERSEER_BOOTSTRAP_RECOVERY_DISABLED"); ok && v != "" {
		cfg.Bootstrap.RecoveryDisabled = v == "true" || v == "1"
	}
	if v, ok := os.LookupEnv("OVERSEER_ENGINE_SCHEDULER_DISABLED"); ok && v != "" {
		cfg.Engine.SchedulerDisabled = v == "true" || v == "1"
	}
	if v, ok := os.LookupEnv("OVERSEER_ENGINE_RECOVERY_DISABLED"); ok && v != "" {
		cfg.Engine.RecoveryDisabled = v == "true" || v == "1"
	}
	setDur("OVERSEER_ENGINE_INTERVAL", &cfg.Engine.Interval)
	setStr("OVERSEER_WEB_AUTH_MODE", &cfg.Web.AuthMode)
	if v, ok := os.LookupEnv("OVERSEER_WEB_TRUSTED_LOCAL"); ok && v != "" {
		cfg.Web.TrustedLocal = v == "true" || v == "1"
	}
	if v, ok := os.LookupEnv("OVERSEER_WEB_COOKIE_INSECURE"); ok && v != "" {
		cfg.Web.CookieInsecure = v == "true" || v == "1"
	}
	setDur("OVERSEER_WEB_SESSION_TTL", &cfg.Web.SessionTTL)
	setDur("OVERSEER_WEB_SESSION_ROTATE", &cfg.Web.SessionRotate)
	setStr("OVERSEER_WEB_CSRF_KEY_ENV", &cfg.Web.CSRFKeyEnv)
	setDur("OVERSEER_WEB_FEED_INTERVAL", &cfg.Web.FeedInterval)
	setInt("OVERSEER_WEB_FEED_BUFFER", &cfg.Web.FeedBuffer)
	// A single compression provider may be declared through the environment.
	if name, ok := os.LookupEnv("OVERSEER_COMPRESSION_PROVIDER_NAME"); ok && name != "" {
		p := cfg.CompressionProviders[name]
		if p.Kind == "" {
			p.Kind = CompressionKindOpenAICompatible
		}
		setStr("OVERSEER_COMPRESSION_PROVIDER_KIND", &p.Kind)
		setStr("OVERSEER_COMPRESSION_PROVIDER_BASE_URL", &p.BaseURL)
		setStr("OVERSEER_COMPRESSION_PROVIDER_API_KEY_ENV", &p.APIKeyEnv)
		setStr("OVERSEER_COMPRESSION_PROVIDER_API_KEY_FILE", &p.APIKeyFile)
		setDur("OVERSEER_COMPRESSION_PROVIDER_TIMEOUT", &p.Timeout)
		setInt("OVERSEER_COMPRESSION_PROVIDER_MAX_OUTPUT_TOKENS", &p.MaxOutputTokens)
		if cfg.CompressionProviders == nil {
			cfg.CompressionProviders = map[string]CompressionProviderConfig{}
		}
		cfg.CompressionProviders[name] = p
	}
	// A single worker provider may be declared through the environment.
	if name, ok := os.LookupEnv("OVERSEER_WORKER_PROVIDER_NAME"); ok && name != "" {
		p := cfg.WorkerProviders[name]
		if p.Kind == "" {
			p.Kind = WorkerKindHermesGateway
		}
		setStr("OVERSEER_WORKER_PROVIDER_KIND", &p.Kind)
		setStr("OVERSEER_WORKER_PROVIDER_ENDPOINT", &p.Endpoint)
		setStr("OVERSEER_WORKER_PROVIDER_AUTH_SECRET_ENV", &p.AuthSecretEnv)
		setStr("OVERSEER_WORKER_PROVIDER_AUTH_SECRET_FILE", &p.AuthSecretFile)
		setDur("OVERSEER_WORKER_PROVIDER_CONNECT_TIMEOUT", &p.ConnectTimeout)
		setDur("OVERSEER_WORKER_PROVIDER_SESSION_TIMEOUT", &p.SessionTimeout)
		if v, ok := os.LookupEnv("OVERSEER_WORKER_PROVIDER_MAX_RESPONSE_BYTES"); ok && v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				p.MaxResponseBytes = n
			} else {
				errs = append(errs, fmt.Sprintf("config: OVERSEER_WORKER_PROVIDER_MAX_RESPONSE_BYTES: %q is not a valid integer", v))
			}
		}
		if v, ok := os.LookupEnv("OVERSEER_WORKER_PROVIDER_ALLOWED_ROOTS"); ok && v != "" {
			p.AllowedRoots = strings.Split(v, ":")
		}
		if cfg.WorkerProviders == nil {
			cfg.WorkerProviders = map[string]WorkerProviderConfig{}
		}
		cfg.WorkerProviders[name] = p
	}
	return errs
}

// rawDuration keeps an unparseable duration as a negative sentinel so
// Validate can report it as a configuration error instead of silently
// falling back.
func rawDuration(v string) time.Duration {
	d, err := time.ParseDuration(v)
	if err != nil {
		return -1
	}
	return d
}

// Validate checks the configuration before infrastructure initialization.
func (c Config) Validate() error {
	var errs []error
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("config: server.port %d out of range 1-65535", c.Server.Port))
	}
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		errs = append(errs, fmt.Errorf("config: database.port %d out of range 1-65535", c.Database.Port))
	}
	if !validLogLevels[c.Logging.Level] {
		errs = append(errs, fmt.Errorf("config: logging.level %q not supported (debug|info|warn|error)", c.Logging.Level))
	}
	if !validLogFormats[c.Logging.Format] {
		errs = append(errs, fmt.Errorf("config: logging.format %q not supported (json|text)", c.Logging.Format))
	}
	if c.Shutdown.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("config: shutdown.timeout %q invalid (must be a positive duration)", c.Shutdown.Timeout))
	}
	if c.Continuation.PollInterval < 0 || c.Continuation.Workers < 0 {
		errs = append(errs, fmt.Errorf("config: continuation.poll_interval and continuation.workers must not be negative"))
	}
	for _, name := range c.compressionProviderNames() {
		p := c.CompressionProviders[name]
		if p.Kind != CompressionKindOpenAICompatible {
			errs = append(errs, fmt.Errorf("config: compression_providers.%s.kind %q not supported (%s)", name, p.Kind, CompressionKindOpenAICompatible))
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("config: compression_providers.%s.base_url must be an http(s) URL", name))
		}
		if p.APIKeyEnv != "" && !envNameRE.MatchString(p.APIKeyEnv) {
			errs = append(errs, fmt.Errorf("config: compression_providers.%s.api_key_env must name an environment variable", name))
		}
		if p.Timeout < 0 {
			errs = append(errs, fmt.Errorf("config: compression_providers.%s.timeout invalid (must not be negative)", name))
		}
		if p.MaxOutputTokens < 0 {
			errs = append(errs, fmt.Errorf("config: compression_providers.%s.max_output_tokens must not be negative", name))
		}
	}
	errs = append(errs, c.MCP.validate()...)
	for _, name := range c.workerProviderNames() {
		errs = append(errs, c.WorkerProviders[name].validate(name)...)
	}
	if c.Web.AuthMode != "" && c.Web.AuthMode != "local" {
		errs = append(errs, fmt.Errorf("config: web.auth_mode %q not supported (local)", c.Web.AuthMode))
	}
	if c.Web.SessionTTL < 0 || c.Web.SessionRotate < 0 || c.Web.FeedInterval < 0 || c.Web.FeedBuffer < 0 ||
		c.Web.MaxBody < 0 || c.Web.MaxDocumentBody < 0 || c.Web.RatePerSecond < 0 || c.Web.RateBurst < 0 {
		errs = append(errs, fmt.Errorf("config: web limits and durations must not be negative"))
	}
	if c.Web.CSRFKeyEnv != "" && !envNameRE.MatchString(c.Web.CSRFKeyEnv) {
		errs = append(errs, fmt.Errorf("config: web.csrf_key_env must name an environment variable"))
	}
	errs = append(errs, c.validatePhase18()...)
	return errors.Join(errs...)
}

func (p WorkerProviderConfig) validate(name string) []error {
	var errs []error
	if p.Kind != WorkerKindHermesGateway {
		errs = append(errs, fmt.Errorf("config: worker_providers.%s.kind %q not supported (%s)", name, p.Kind, WorkerKindHermesGateway))
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("config: worker_providers.%s.endpoint must be an http(s) URL", name))
	}
	if p.TLS != nil && u.Scheme != "https" {
		errs = append(errs, fmt.Errorf("config: worker_providers.%s.tls requires an https endpoint", name))
	}
	if p.AuthSecretEnv != "" && !envNameRE.MatchString(p.AuthSecretEnv) {
		errs = append(errs, fmt.Errorf("config: worker_providers.%s.auth_secret_env must name an environment variable", name))
	}
	if p.ConnectTimeout < 0 || p.SessionTimeout < 0 || p.Reconnect.InitialDelay < 0 || p.Reconnect.MaxDelay < 0 {
		errs = append(errs, fmt.Errorf("config: worker_providers.%s timeouts must not be negative", name))
	}
	if p.MaxResponseBytes < 0 || p.Reconnect.MaxAttempts < 0 {
		errs = append(errs, fmt.Errorf("config: worker_providers.%s.max_response_bytes / reconnect.max_attempts must not be negative", name))
	}
	for _, r := range p.AllowedRoots {
		if !strings.HasPrefix(r, "/") {
			errs = append(errs, fmt.Errorf("config: worker_providers.%s.allowed_roots must be absolute paths", name))
		}
	}
	return errs
}

// workerProviderNames returns the worker provider names in sorted order.
func (c Config) workerProviderNames() []string {
	names := make([]string, 0, len(c.WorkerProviders))
	for n := range c.WorkerProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// compressionProviderNames returns the provider names in sorted order.
func (c Config) compressionProviderNames() []string {
	names := make([]string, 0, len(c.CompressionProviders))
	for n := range c.CompressionProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// String returns a human-readable rendering with secrets redacted. A
// compression provider shows only the NAME of its API-key variable, never a
// value.
func (c Config) String() string {
	s := fmt.Sprintf(
		"server=%s:%d database=%s:%d/%s user=%s password=%s ssl=%s logging=%s/%s shutdown=%s",
		c.Server.Host, c.Server.Port,
		c.Database.Host, c.Database.Port, c.Database.Name, c.Database.User,
		redact(c.Database.Password), c.Database.SSLMode,
		c.Logging.Level, c.Logging.Format, c.Shutdown.Timeout,
	)
	for _, name := range c.compressionProviderNames() {
		p := c.CompressionProviders[name]
		key := "none"
		if p.APIKeyEnv != "" {
			key = "env:" + p.APIKeyEnv
		}
		s += fmt.Sprintf(" compression_provider=%s(%s %s key=%s)", name, p.Kind, p.BaseURL, key)
	}
	for _, name := range c.workerProviderNames() {
		p := c.WorkerProviders[name]
		auth := "none"
		if p.AuthSecretEnv != "" {
			auth = "env:" + p.AuthSecretEnv
		}
		s += fmt.Sprintf(" worker_provider=%s(%s %s auth=%s)", name, p.Kind, p.Endpoint, auth)
	}
	return s
}

func redact(v string) string {
	if v == "" {
		return "(unset)"
	}
	return "REDACTED"
}

// Addr returns the server listen address.
func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// DSN builds the PostgreSQL connection string (includes the password;
// do not log it).
func (c Config) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		c.Database.User, urlEscape(c.Database.Password), c.Database.Host, c.Database.Port, c.Database.Name, c.Database.SSLMode)
}

func urlEscape(s string) string {
	r := strings.NewReplacer("%", "%25", "@", "%40", "/", "%2F", ":", "%3A", "?", "%3F", "#", "%23", "&", "%26", "+", "%2B", "=", "%3D", " ", "%20", "$", "%24", ",", "%2C")
	return r.Replace(s)
}
