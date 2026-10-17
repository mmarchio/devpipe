// Package hermes is the Hermes Gateway worker adapter (Phase 12). Every
// Hermes-specific type lives here; the rest of the overseer speaks only
// the generic internal/worker contract.
package hermes

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"overseer/internal/worker"
)

// TLSConfig is the gateway transport security configuration (WBS 12.95).
type TLSConfig struct {
	CAFile     string `json:"ca_file,omitempty"`
	ServerName string `json:"server_name,omitempty"`
	MinVersion string `json:"min_version,omitempty"`
}

// HermesGatewayConfig is the provider-specific configuration (WBS 12.3).
// AuthSecretRef names the environment variable holding the gateway token;
// the value itself is never part of this struct's serialization.
type HermesGatewayConfig struct {
	Endpoint string

	AuthSecretRef *string

	ConnectTimeout time.Duration

	ReconnectPolicy worker.ReconnectPolicy

	TLS *TLSConfig

	// DialControl, when set, vets every resolved address before a
	// connection is made (endpoint policy, WBS 18.88). Not serialized.
	DialControl func(network, address string, c syscall.RawConn) error

	// MaxResponseBytes bounds a transport response (WBS 12.93),
	// independent of model context limits.
	MaxResponseBytes int64

	// AllowedRoots restricts working directories (WBS 12.25); empty = no
	// sandbox restriction.
	AllowedRoots []string
}

// Defaults.
const (
	DefaultConnectTimeout   = 5 * time.Second
	DefaultMaxResponseBytes = 4 << 20
)

// wireConfig is the JSON form carried in WorkerConfig.ProviderConfig.
type wireConfig struct {
	Endpoint         string     `json:"endpoint"`
	AuthSecretRef    string     `json:"auth_secret_ref,omitempty"`
	ConnectTimeout   string     `json:"connect_timeout,omitempty"`
	Reconnect        *wireRecon `json:"reconnect,omitempty"`
	TLS              *TLSConfig `json:"tls,omitempty"`
	MaxResponseBytes int64      `json:"max_response_bytes,omitempty"`
	AllowedRoots     []string   `json:"allowed_roots,omitempty"`
}

type wireRecon struct {
	MaxAttempts  int    `json:"max_attempts"`
	InitialDelay string `json:"initial_delay,omitempty"`
	MaxDelay     string `json:"max_delay,omitempty"`
}

// ParseConfig decodes and validates ProviderConfig.
func ParseConfig(raw json.RawMessage) (HermesGatewayConfig, error) {
	var w wireConfig
	if len(raw) == 0 {
		return HermesGatewayConfig{}, &worker.ConfigError{Problems: []string{"hermes gateway configuration missing"}}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return HermesGatewayConfig{}, &worker.ConfigError{Problems: []string{"hermes gateway configuration: " + err.Error()}}
	}
	var problems []string
	dur := func(name, v string, def time.Duration) time.Duration {
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			problems = append(problems, name+" is not a valid duration")
			return def
		}
		return d
	}
	c := HermesGatewayConfig{Endpoint: w.Endpoint, TLS: w.TLS, MaxResponseBytes: w.MaxResponseBytes, AllowedRoots: w.AllowedRoots,
		ConnectTimeout: dur("connect_timeout", w.ConnectTimeout, DefaultConnectTimeout), ReconnectPolicy: worker.DefaultReconnectPolicy}
	if w.AuthSecretRef != "" {
		ref := w.AuthSecretRef
		c.AuthSecretRef = &ref
	}
	if w.Reconnect != nil {
		c.ReconnectPolicy = worker.ReconnectPolicy{MaxAttempts: w.Reconnect.MaxAttempts,
			InitialDelay: dur("reconnect.initial_delay", w.Reconnect.InitialDelay, worker.DefaultReconnectPolicy.InitialDelay),
			MaxDelay:     dur("reconnect.max_delay", w.Reconnect.MaxDelay, worker.DefaultReconnectPolicy.MaxDelay)}
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if err := c.Validate(); err != nil {
		problems = append(problems, err.(*worker.ConfigError).Problems...)
	}
	if len(problems) > 0 {
		return HermesGatewayConfig{}, &worker.ConfigError{Problems: problems}
	}
	return c, nil
}

var (
	envRefRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	fileRefRE = regexp.MustCompile(`^file:/.+`)
)

// Validate checks the configuration without contacting the gateway.
func (c HermesGatewayConfig) Validate() error {
	var p []string
	u, err := url.Parse(c.Endpoint)
	switch {
	case c.Endpoint == "":
		p = append(p, "endpoint not configured")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		p = append(p, "endpoint must be an http(s) URL")
	case c.TLS != nil && u.Scheme != "https":
		p = append(p, "tls configured but endpoint is not https")
	}
	if c.AuthSecretRef != nil && !envRefRE.MatchString(*c.AuthSecretRef) && !fileRefRE.MatchString(*c.AuthSecretRef) {
		p = append(p, "auth_secret_ref must name an environment variable or file:/absolute/path")
	}
	if c.ConnectTimeout <= 0 {
		p = append(p, "connect timeout must be positive")
	}
	if c.ReconnectPolicy.Validate() != nil {
		p = append(p, "invalid reconnect policy")
	}
	if c.MaxResponseBytes < 0 {
		p = append(p, "max_response_bytes must not be negative")
	}
	if c.TLS != nil {
		if _, err := c.TLS.build(); err != nil {
			p = append(p, err.Error())
		}
	}
	for _, r := range c.AllowedRoots {
		if !strings.HasPrefix(r, "/") {
			p = append(p, "allowed_roots must be absolute paths")
			break
		}
	}
	if len(p) > 0 {
		return &worker.ConfigError{Problems: p}
	}
	return nil
}

// ClientTLS returns the gateway's client TLS configuration (nil when none
// is configured), for auxiliary gateway requests made outside the worker.
func (c HermesGatewayConfig) ClientTLS() (*tls.Config, error) {
	if c.TLS == nil {
		return nil, nil
	}
	return c.TLS.build()
}

func (t *TLSConfig) build() (*tls.Config, error) {
	cfg := &tls.Config{ServerName: t.ServerName, MinVersion: tls.VersionTLS12}
	switch t.MinVersion {
	case "", "1.2":
	case "1.3":
		cfg.MinVersion = tls.VersionTLS13
	default:
		return nil, fmt.Errorf("tls.min_version %q not supported (1.2|1.3)", t.MinVersion)
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tls.ca_file not readable")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.ca_file contains no PEM certificate")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// String renders the configuration without any secret value (T12.3.4).
func (c HermesGatewayConfig) String() string {
	ref := "none"
	if c.AuthSecretRef != nil {
		ref = *c.AuthSecretRef
		if !strings.HasPrefix(ref, "file:") {
			ref = "env:" + ref
		}
	}
	return fmt.Sprintf("hermes_gateway(%s auth=%s connect_timeout=%s tls=%v)", c.Endpoint, ref, c.ConnectTimeout, c.TLS != nil)
}

// MarshalJSON emits the secret-free JSON form.
func (c HermesGatewayConfig) MarshalJSON() ([]byte, error) {
	w := wireConfig{Endpoint: c.Endpoint, ConnectTimeout: c.ConnectTimeout.String(), TLS: c.TLS, MaxResponseBytes: c.MaxResponseBytes, AllowedRoots: c.AllowedRoots,
		Reconnect: &wireRecon{MaxAttempts: c.ReconnectPolicy.MaxAttempts, InitialDelay: c.ReconnectPolicy.InitialDelay.String(), MaxDelay: c.ReconnectPolicy.MaxDelay.String()}}
	if c.AuthSecretRef != nil {
		w.AuthSecretRef = *c.AuthSecretRef
	}
	return json.Marshal(w)
}
