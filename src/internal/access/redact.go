package access

import (
	"slices"
	"strings"
	"sync"

	"overseer/internal/worker"
)

// Redacted is the replacement text for secret values.
const Redacted = "[REDACTED]"

// IsSecretKey reports whether a field name denotes a secret value
// (WBS 17.122). Names that reference a secret (…_env, …_ref) or describe
// its presence (…_configured) are not secrets; token counts
// (token_estimate, max_tokens) are not secrets.
func IsSecretKey(k string) bool {
	l := strings.ToLower(k)
	for _, suf := range []string{"_env", "_ref", "_configured", "_present"} {
		if strings.HasSuffix(l, suf) {
			return false
		}
	}
	for _, f := range []string{"password", "passwd", "secret", "api_key", "apikey", "authorization", "credential", "private_key"} {
		if strings.Contains(l, f) {
			return true
		}
	}
	return l == "token" || strings.HasSuffix(l, "_token") || l == "dsn"
}

// Redactor removes secrets from response / audit / event values
// (WBS 17.122, 17.57, 17.105): values under secret-like keys and every
// occurrence of a known secret value (configured provider credentials).
type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// NewRedactor returns a redactor for the known secret values (empty and
// very short values are ignored to avoid redacting ordinary text).
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// Add remembers more secret values (a provider configured at runtime).
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		if len(s) >= 6 && !slices.Contains(r.secrets, s) {
			r.secrets = append(r.secrets, s)
		}
	}
}

// String redacts known secret values in s (reuses worker.Redact).
func (r *Redactor) String(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	secrets := r.secrets
	r.mu.RUnlock()
	if len(secrets) == 0 {
		return s
	}
	return worker.Redact(s, secrets...)
}

// Value redacts a decoded JSON value recursively and returns a copy.
func (r *Redactor) Value(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if IsSecretKey(k) {
				if s, ok := val.(string); ok && s == "" {
					out[k] = ""
					continue
				}
				if _, ok := val.(bool); ok {
					out[k] = val
					continue
				}
				out[k] = Redacted
				continue
			}
			out[k] = r.Value(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.Value(e)
		}
		return out
	case string:
		return r.String(x)
	}
	return v
}
