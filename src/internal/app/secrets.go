package app

import (
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"overseer/internal/config"
)

// secretSource resolves secret references (WBS 18.78): "file:/path" reads
// a file, re-read when its modification time or size changes (rotation
// without a restart); anything else is an environment variable name.
// Values are never logged; every value ever resolved is remembered for
// redaction.
type secretSource struct {
	mu     sync.Mutex
	files  map[string]fileSecret
	seen   map[string]bool
	getenv func(string) (string, bool)
}

type fileSecret struct {
	mod   time.Time
	size  int64
	value string
}

func newSecretSource() *secretSource {
	return &secretSource{files: map[string]fileSecret{}, seen: map[string]bool{}, getenv: os.LookupEnv}
}

// Resolve returns the current value of ref.
func (s *secretSource) Resolve(ref string) (string, bool) {
	if path, ok := strings.CutPrefix(ref, "file:"); ok {
		return s.file(path)
	}
	v, ok := s.getenv(ref)
	if ok && v != "" {
		s.remember(v)
	}
	return v, ok
}

func (s *secretSource) file(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.files[path]; ok {
			return c.value, c.value != "" // keep the last good value
		}
		return "", false
	}
	s.mu.Lock()
	c, ok := s.files[path]
	s.mu.Unlock()
	if ok && c.mod.Equal(st.ModTime()) && c.size == st.Size() {
		return c.value, c.value != ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c.value, c.value != ""
	}
	v := strings.TrimSpace(string(b))
	s.mu.Lock()
	s.files[path] = fileSecret{mod: st.ModTime(), size: st.Size(), value: v}
	if v != "" {
		s.seen[v] = true
	}
	s.mu.Unlock()
	return v, v != ""
}

func (s *secretSource) remember(v string) {
	s.mu.Lock()
	s.seen[v] = true
	s.mu.Unlock()
}

// Values returns every secret value resolved so far (redaction).
func (s *secretSource) Values() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.seen))
	for v := range s.seen {
		out = append(out, v)
	}
	return out
}

// workerSecretRef / compressionSecretRef render a provider's secret
// reference ("" = none).
func workerSecretRef(p config.WorkerProviderConfig) string {
	if p.AuthSecretFile != "" {
		return "file:" + p.AuthSecretFile
	}
	return p.AuthSecretEnv
}

func compressionSecretRef(p config.CompressionProviderConfig) string {
	if p.APIKeyFile != "" {
		return "file:" + p.APIKeyFile
	}
	return p.APIKeyEnv
}

// endpointDialControl enforces the provider endpoint policy on every
// resolved address at connect time (WBS 18.88), so a host name that
// resolves to a forbidden address (metadata, link-local, or private when
// disallowed) is refused even though the configured name looked fine.
func endpointDialControl(cfg config.Config) func(network, address string, c syscall.RawConn) error {
	pol := cfg.Security.ProviderEndpoints
	if !pol.Enabled && !cfg.IsProduction() {
		return nil
	}
	ipPolicy := pol
	ipPolicy.AllowedHosts = nil // names were checked at configuration time
	prod := cfg.IsProduction()
	return func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return nil
		}
		return config.CheckEndpointIP(ip, ipPolicy, prod)
	}
}
