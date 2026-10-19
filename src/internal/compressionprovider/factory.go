package compressionprovider

import (
	"net"
	"net/http"
	"os"
	"sort"
	"syscall"
	"time"

	"overseer/internal/compression"
	"overseer/internal/config"
)

// Factory selects a compression provider by the project's configured name
// (WBS 9.40 T9.40.4). Credentials are resolved from the environment variable
// named in the app configuration at call time and are never persisted.
type Factory struct {
	providers map[string]config.CompressionProviderConfig
	// source, when set, supplies the current providers on every call
	// (providers edited at runtime); providers is then ignored.
	source func() map[string]config.CompressionProviderConfig
	getenv func(string) string
	client *http.Client
	// resolve resolves secret references ("file:/path" / env name) per
	// client build, so a rotated key applies to the next call (WBS 18.78).
	resolve func(ref string) (string, bool)
	// dialControl vets resolved addresses (endpoint policy, WBS 18.88).
	dialControl func(network, address string, c syscall.RawConn) error
}

// WithSource reads the providers from source on every call, so providers
// added or changed at runtime apply to the next client.
func (f *Factory) WithSource(source func() map[string]config.CompressionProviderConfig) *Factory {
	f.source = source
	return f
}

// current returns the providers in effect.
func (f *Factory) current() map[string]config.CompressionProviderConfig {
	if f.source != nil {
		return f.source()
	}
	return f.providers
}

// WithResolver installs the secret-reference resolver.
func (f *Factory) WithResolver(r func(ref string) (string, bool)) *Factory {
	f.resolve = r
	return f
}

// WithDialControl installs the endpoint-policy dial check.
func (f *Factory) WithDialControl(c func(network, address string, c syscall.RawConn) error) *Factory {
	f.dialControl = c
	return f
}

// NewFactory builds a factory over the configured providers.
func NewFactory(providers map[string]config.CompressionProviderConfig) *Factory {
	return &Factory{providers: providers, getenv: os.Getenv}
}

// WithEnv overrides the environment lookup (tests).
func (f *Factory) WithEnv(getenv func(string) string) *Factory {
	f.getenv = getenv
	return f
}

// WithHTTPClient overrides the HTTP client (tests).
func (f *Factory) WithHTTPClient(c *http.Client) *Factory {
	f.client = c
	return f
}

// Names lists the configured provider names (sorted).
func (f *Factory) Names() []string {
	providers := f.current()
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Client builds the adapter for a provider name; an unknown name or a
// required-but-missing credential is a configuration error.
func (f *Factory) Client(name string) (*OpenAICompatible, error) {
	p, ok := f.current()[name]
	if !ok || name == "" {
		return nil, compression.NewError(compression.ErrorConfiguration, "unknown compression provider "+quote(name))
	}
	key := ""
	switch {
	case p.APIKeyFile != "":
		v, ok := "", false
		if f.resolve != nil {
			v, ok = f.resolve("file:" + p.APIKeyFile)
		}
		if !ok || v == "" {
			return nil, compression.NewError(compression.ErrorConfiguration, "compression provider "+quote(name)+": key file "+p.APIKeyFile+" is not readable")
		}
		key = v
	case p.APIKeyEnv != "":
		if f.resolve != nil {
			key, _ = f.resolve(p.APIKeyEnv)
		} else {
			key = f.getenv(p.APIKeyEnv)
		}
		if key == "" {
			return nil, compression.NewError(compression.ErrorConfiguration, "compression provider "+quote(name)+": environment variable "+p.APIKeyEnv+" is not set")
		}
	}
	client := f.client
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = (&net.Dialer{Timeout: 30 * time.Second, Control: f.dialControl}).DialContext
		client = &http.Client{Timeout: p.Timeout, Transport: tr}
	}
	return New(Options{Name: name, BaseURL: p.BaseURL, APIKey: key, MaxOutputTokens: p.MaxOutputTokens, HTTPClient: client})
}

// Compressor returns the provider's compression.Compressor.
func (f *Factory) Compressor(name string) (compression.Compressor, error) {
	c, err := f.Client(name)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// SemanticValidator returns an LLM semantic validator on the provider.
func (f *Factory) SemanticValidator(name, model string) (compression.SemanticValidator, error) {
	c, err := f.Client(name)
	if err != nil {
		return nil, err
	}
	return &LLMSemanticValidator{Client: c, Model: model}, nil
}

func quote(s string) string { return "\"" + s + "\"" }
