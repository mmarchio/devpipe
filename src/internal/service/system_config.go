package service

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"overseer/internal/config"
)

// SystemConfigService edits the server configuration file from the
// console: the database connection (applied at the next restart) and the
// worker / compression providers (applied immediately). Secrets are never
// written: providers reference an environment variable or a file, and the
// database password stays in OVERSEER_DATABASE_PASSWORD.
type SystemConfigService struct {
	d  SystemConfigDeps
	mu sync.Mutex
}

// SystemConfigDeps wires the service.
type SystemConfigDeps struct {
	// Path is the configuration file (OVERSEER_CONFIG_PATH).
	Path string
	// Running is the database configuration the process started with.
	Running config.DatabaseConfig
	// Providers is the live provider configuration.
	Providers *config.ProviderStore
	// Apply installs a changed provider configuration in the running
	// process (store, cached workers, secret redaction).
	Apply func(cfg config.Config)
	// InUse lists the projects whose settings name a provider.
	InUse func(ctx context.Context, kind, name string) ([]string, error)
	// SecretSet reports whether a secret reference ("ENV_NAME" or
	// "file:/path") resolves to a value.
	SecretSet func(ref string) bool
}

// NewSystemConfigService builds the service.
func NewSystemConfigService(d SystemConfigDeps) *SystemConfigService {
	if d.SecretSet == nil {
		d.SecretSet = func(ref string) bool {
			if p, ok := strings.CutPrefix(ref, "file:"); ok {
				b, err := os.ReadFile(p)
				return err == nil && strings.TrimSpace(string(b)) != ""
			}
			return ref != "" && os.Getenv(ref) != ""
		}
	}
	return &SystemConfigService{d: d}
}

// DatabaseSettings are the editable database connection settings.
type DatabaseSettings struct {
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Name            string `json:"name"`
	User            string `json:"user"`
	SSLMode         string `json:"ssl_mode"`
	MaxConns        int    `json:"max_conns"`
	MinConns        int    `json:"min_conns"`
	ApplicationName string `json:"application_name"`
}

// DatabaseView is the database section of the System category.
type DatabaseView struct {
	// File is what the configuration file says (defaults filled in).
	File DatabaseSettings `json:"file"`
	// Effective is what applies after the OVERSEER_* overrides.
	Effective DatabaseSettings `json:"effective"`
	// Overrides maps a setting to the environment variable overriding it.
	Overrides map[string]string `json:"overrides"`
	// PasswordConfigured / PasswordSource ("environment", "file", "").
	PasswordConfigured bool   `json:"password_configured"`
	PasswordSource     string `json:"password_source"`
	// RestartRequired: the effective settings differ from the ones the
	// running process connected with.
	RestartRequired bool   `json:"restart_required"`
	ConfigPath      string `json:"config_path"`
}

// DatabaseTestResult is the outcome of a connection test.
type DatabaseTestResult struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	ServerVersion string `json:"server_version,omitempty"`
	// SchemaVersion is the overseer schema version found (0 = none).
	SchemaVersion int   `json:"schema_version"`
	DurationMS    int64 `json:"duration_ms"`
}

func dbSettings(c config.DatabaseConfig) DatabaseSettings {
	return DatabaseSettings{Host: c.Host, Port: c.Port, Name: c.Name, User: c.User, SSLMode: c.SSLMode, MaxConns: c.MaxConns,
		MinConns: c.MinConns, ApplicationName: c.ApplicationName}
}

// Database returns the database section.
func (s *SystemConfigService) Database(context.Context) (*DatabaseView, error) {
	file, err := config.LoadFileOnly(s.d.Path)
	if err != nil {
		return nil, err
	}
	eff, err := config.Load(s.d.Path)
	if err != nil {
		return nil, err
	}
	v := &DatabaseView{File: dbSettings(file.Database), Effective: dbSettings(eff.Database), Overrides: config.DatabaseEnvOverrides(),
		PasswordConfigured: eff.Database.Password != "", ConfigPath: s.d.Path}
	switch {
	case v.Overrides["password"] != "":
		v.PasswordSource = "environment"
	case file.Database.Password != "":
		v.PasswordSource = "file"
	}
	run := s.d.Running
	v.RestartRequired = dbSettings(run) != v.Effective || run.Password != eff.Database.Password
	return v, nil
}

var sslModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

func validateDatabase(in DatabaseSettings) map[string]string {
	f := map[string]string{}
	if strings.TrimSpace(in.Host) == "" {
		f["host"] = "required"
	}
	if in.Port < 1 || in.Port > 65535 {
		f["port"] = "must be between 1 and 65535"
	}
	if strings.TrimSpace(in.Name) == "" {
		f["name"] = "required"
	}
	if strings.TrimSpace(in.User) == "" {
		f["user"] = "required"
	}
	if in.SSLMode != "" && !slices.Contains(sslModes, in.SSLMode) {
		f["ssl_mode"] = "must be one of " + strings.Join(sslModes, ", ")
	}
	if in.MaxConns < 0 || in.MinConns < 0 || (in.MaxConns > 0 && in.MinConns > in.MaxConns) {
		f["max_conns"] = "connection counts must be positive, minimum at most maximum"
	}
	return f
}

// SaveDatabase writes the database settings to the configuration file.
// They apply at the next restart.
func (s *SystemConfigService) SaveDatabase(ctx context.Context, in DatabaseSettings) (*DatabaseView, error) {
	if f := validateDatabase(in); len(f) > 0 {
		return nil, &FieldErrors{Fields: f}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.edit(func(root *yaml.Node) error {
		db := config.MappingAt(root, "database")
		config.SetString(db, "host", in.Host)
		config.SetInt(db, "port", int64(in.Port))
		config.SetString(db, "name", in.Name)
		config.SetString(db, "user", in.User)
		config.SetString(db, "ssl_mode", in.SSLMode)
		config.SetInt(db, "max_conns", int64(in.MaxConns))
		config.SetInt(db, "min_conns", int64(in.MinConns))
		config.SetString(db, "application_name", in.ApplicationName)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Database(ctx)
}

// TestDatabase connects with the given settings and reports the server
// and overseer schema versions. password, when non-empty, is used for
// this test only (never stored); otherwise the configured password is.
func (s *SystemConfigService) TestDatabase(ctx context.Context, in DatabaseSettings, password string) DatabaseTestResult {
	if f := validateDatabase(in); len(f) > 0 {
		keys := make([]string, 0, len(f))
		for k, v := range f {
			keys = append(keys, k+": "+v)
		}
		sort.Strings(keys)
		return DatabaseTestResult{Error: strings.Join(keys, "; ")}
	}
	if password == "" {
		if eff, err := config.Load(s.d.Path); err == nil {
			password = eff.Database.Password
		}
	}
	c := config.Config{Database: config.DatabaseConfig{Host: in.Host, Port: in.Port, Name: in.Name, User: in.User, Password: password,
		SSLMode: in.SSLMode}}
	if c.Database.SSLMode == "" {
		c.Database.SSLMode = "disable"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	res := DatabaseTestResult{}
	conn, err := pgx.Connect(ctx, c.DSN())
	if err != nil {
		res.Error, res.DurationMS = scrub(err.Error(), password), time.Since(start).Milliseconds()
		return res
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&res.ServerVersion); err != nil {
		res.Error, res.DurationMS = scrub(err.Error(), password), time.Since(start).Milliseconds()
		return res
	}
	var v *int
	if err := conn.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&v); err == nil && v != nil {
		res.SchemaVersion = *v
	}
	res.OK, res.DurationMS = true, time.Since(start).Milliseconds()
	return res
}

func scrub(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}

// Provider kinds.
const (
	ProviderKindWorker      = "worker"
	ProviderKindCompression = "compression"
)

// ProviderSettings are the editable settings of one provider. The secret
// is a reference: an environment variable name or a file path.
type ProviderSettings struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Endpoint is the worker gateway URL or the compressor base URL.
	Endpoint   string   `json:"endpoint"`
	SecretEnv  string   `json:"secret_env"`
	SecretFile string   `json:"secret_file"`
	Models     []string `json:"models"`
	// Worker only.
	AllowedRoots   []string `json:"allowed_roots"`
	ConnectTimeout string   `json:"connect_timeout"`
	SessionTimeout string   `json:"session_timeout"`
	MaxConcurrent  int      `json:"max_concurrent"`
	// Compressor only.
	Timeout         string `json:"timeout"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

// ProviderView is a provider with its status.
type ProviderView struct {
	ProviderSettings
	ProviderType     string   `json:"provider_type"`
	SecretConfigured bool     `json:"secret_configured"`
	UsedBy           []string `json:"used_by"`
}

// ProvidersView lists the providers of both kinds.
type ProvidersView struct {
	Workers     []ProviderView `json:"workers"`
	Compressors []ProviderView `json:"compressors"`
	ConfigPath  string         `json:"config_path"`
}

func durStr(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

// Providers lists the live providers.
func (s *SystemConfigService) Providers(ctx context.Context) (*ProvidersView, error) {
	v := &ProvidersView{Workers: []ProviderView{}, Compressors: []ProviderView{}, ConfigPath: s.d.Path}
	for name, p := range s.d.Providers.Workers() {
		pv := ProviderView{ProviderSettings: ProviderSettings{Name: name, Kind: ProviderKindWorker, Endpoint: p.Endpoint, SecretEnv: p.AuthSecretEnv,
			SecretFile: p.AuthSecretFile, Models: p.Models, AllowedRoots: p.AllowedRoots, ConnectTimeout: durStr(p.ConnectTimeout),
			SessionTimeout: durStr(p.SessionTimeout), MaxConcurrent: p.MaxConcurrent}, ProviderType: p.Kind}
		s.status(ctx, &pv)
		v.Workers = append(v.Workers, pv)
	}
	for name, p := range s.d.Providers.Compressors() {
		pv := ProviderView{ProviderSettings: ProviderSettings{Name: name, Kind: ProviderKindCompression, Endpoint: p.BaseURL, SecretEnv: p.APIKeyEnv,
			SecretFile: p.APIKeyFile, Timeout: durStr(p.Timeout), MaxOutputTokens: p.MaxOutputTokens}, ProviderType: p.Kind}
		s.status(ctx, &pv)
		v.Compressors = append(v.Compressors, pv)
	}
	sort.Slice(v.Workers, func(i, j int) bool { return v.Workers[i].Name < v.Workers[j].Name })
	sort.Slice(v.Compressors, func(i, j int) bool { return v.Compressors[i].Name < v.Compressors[j].Name })
	return v, nil
}

func (s *SystemConfigService) status(ctx context.Context, pv *ProviderView) {
	switch {
	case pv.SecretFile != "":
		pv.SecretConfigured = s.d.SecretSet("file:" + pv.SecretFile)
	case pv.SecretEnv != "":
		pv.SecretConfigured = s.d.SecretSet(pv.SecretEnv)
	}
	pv.UsedBy = []string{}
	if s.d.InUse != nil {
		if used, err := s.d.InUse(ctx, pv.Kind, pv.Name); err == nil {
			pv.UsedBy = used
		}
	}
}

var providerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateProvider(in ProviderSettings) map[string]string {
	f := map[string]string{}
	if !providerNameRE.MatchString(in.Name) {
		f["name"] = "lowercase letters, digits, '-' and '_' (start with a letter or digit)"
	}
	if in.Kind != ProviderKindWorker && in.Kind != ProviderKindCompression {
		f["kind"] = "must be worker or compression"
	}
	if u, err := url.Parse(in.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		f["endpoint"] = "must be an http(s) URL"
	}
	if in.SecretEnv != "" && in.SecretFile != "" {
		f["secret_env"] = "give an environment variable or a secret file, not both"
	}
	if in.SecretEnv != "" && !envNameRE.MatchString(in.SecretEnv) {
		f["secret_env"] = "must be an environment variable name"
	}
	if in.SecretFile != "" && !strings.HasPrefix(in.SecretFile, "/") {
		f["secret_file"] = "must be an absolute path"
	}
	for k, d := range map[string]string{"connect_timeout": in.ConnectTimeout, "session_timeout": in.SessionTimeout, "timeout": in.Timeout} {
		if d == "" {
			continue
		}
		if v, err := time.ParseDuration(d); err != nil || v < 0 {
			f[k] = "must be a duration such as 30s or 5m"
		}
	}
	if in.MaxConcurrent < 0 {
		f["max_concurrent"] = "must not be negative"
	}
	if in.MaxOutputTokens < 0 {
		f["max_output_tokens"] = "must not be negative"
	}
	for _, r := range in.AllowedRoots {
		if !strings.HasPrefix(r, "/") {
			f["allowed_roots"] = "every root must be an absolute path"
		}
	}
	return f
}

// SaveProvider creates or updates a provider in the configuration file
// and applies it immediately. Keys the console does not edit (TLS,
// circuit breaker, reconnect) are kept as they are in the file.
func (s *SystemConfigService) SaveProvider(ctx context.Context, in ProviderSettings) (*ProvidersView, error) {
	in.Models, in.AllowedRoots = cleanList(in.Models), cleanList(in.AllowedRoots)
	if f := validateProvider(in); len(f) > 0 {
		return nil, &FieldErrors{Fields: f}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.edit(func(root *yaml.Node) error {
		switch in.Kind {
		case ProviderKindWorker:
			m := config.MappingAt(root, "worker_providers", in.Name)
			config.SetString(m, "kind", config.WorkerKindHermesGateway)
			config.SetString(m, "endpoint", in.Endpoint)
			config.SetString(m, "auth_secret_env", in.SecretEnv)
			config.SetString(m, "auth_secret_file", in.SecretFile)
			config.SetStrings(m, "models", in.Models)
			config.SetStrings(m, "allowed_roots", in.AllowedRoots)
			config.SetString(m, "connect_timeout", in.ConnectTimeout)
			config.SetString(m, "session_timeout", in.SessionTimeout)
			config.SetInt(m, "max_concurrent", int64(in.MaxConcurrent))
		case ProviderKindCompression:
			m := config.MappingAt(root, "compression_providers", in.Name)
			config.SetString(m, "kind", config.CompressionKindOpenAICompatible)
			config.SetString(m, "base_url", in.Endpoint)
			config.SetString(m, "api_key_env", in.SecretEnv)
			config.SetString(m, "api_key_file", in.SecretFile)
			config.SetString(m, "timeout", in.Timeout)
			config.SetInt(m, "max_output_tokens", int64(in.MaxOutputTokens))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.applyProviders(); err != nil {
		return nil, err
	}
	return s.Providers(ctx)
}

// DeleteProvider removes a provider no project uses and applies the
// change immediately.
func (s *SystemConfigService) DeleteProvider(ctx context.Context, kind, name string) (*ProvidersView, error) {
	section := map[string]string{ProviderKindWorker: "worker_providers", ProviderKindCompression: "compression_providers"}[kind]
	if section == "" {
		return nil, ErrNotFound
	}
	exists := false
	if kind == ProviderKindWorker {
		_, exists = s.d.Providers.Worker(name)
	} else {
		_, exists = s.d.Providers.Compressor(name)
	}
	if !exists {
		return nil, ErrNotFound
	}
	if s.d.InUse != nil {
		used, err := s.d.InUse(ctx, kind, name)
		if err != nil {
			return nil, err
		}
		if len(used) > 0 {
			return nil, fmt.Errorf("%w: provider %s is used by %s", ErrConflict, name, strings.Join(used, ", "))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.edit(func(root *yaml.Node) error {
		config.DeleteKey(config.MappingAt(root, section), name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.applyProviders(); err != nil {
		return nil, err
	}
	return s.Providers(ctx)
}

// edit applies a file edit and requires the result to load (with the
// environment overrides and validation); otherwise the previous file is
// restored and the validation error returned as a field error.
func (s *SystemConfigService) edit(fn func(root *yaml.Node) error) error {
	before, readErr := os.ReadFile(s.d.Path)
	if err := config.EditFile(s.d.Path, fn); err != nil {
		return err
	}
	if _, err := config.Load(s.d.Path); err != nil {
		if readErr == nil {
			_ = os.WriteFile(s.d.Path, before, 0o600)
		} else {
			_ = os.Remove(s.d.Path)
		}
		return &FieldErrors{Fields: map[string]string{"configuration": err.Error()}}
	}
	return nil
}

func (s *SystemConfigService) applyProviders() error {
	cfg, err := config.Load(s.d.Path)
	if err != nil {
		return err
	}
	if s.d.Apply != nil {
		s.d.Apply(cfg)
	} else {
		s.d.Providers.Replace(cfg)
	}
	return nil
}

func cleanList(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
