package prompt

import (
	"strings"
)

// sensitiveMarkers are the field-name markers of WBS 8.38's default-deny
// classes: database passwords, API keys, provider credentials, license
// secrets, and session tokens. A field name that carries any marker is
// classified sensitive (P8-37 policy, stated in sensitive_test.go).
var sensitiveMarkers = []string{
	"password",
	"passwd",
	"secret",
	"api_key",
	"apikey",
	"token",
	"credential",
	"dsn",
	"connection_string",
}

// IsSecretField classifies a variable field name against the WBS 8.38
// default-deny set. The comparison is case-insensitive over the whole
// field and the marker positions in the name: "database_password",
// "API_KEY", "sessionToken" (via token), "provider_credential",
// "license_secret" all classify sensitive.
func IsSecretField(field string) bool {
	f := strings.ToLower(field)
	for _, m := range sensitiveMarkers {
		if strings.Contains(f, m) {
			return true
		}
	}
	return false
}

// PromptSafe reports whether a variable path is explicitly registered as
// prompt-safe: the only way a setting value may enter a template (WBS
// 8.38, T8.38.3). Absence of registration is unavailability — there is no
// lookup fallback (P8-37 NOTES).
func (r *Registry) PromptSafe(path string) bool {
	_, ok := r.promptSafeVars[path]
	return ok
}

// ContainsSensitive reports whether content carries a value registered as
// sensitive (tracked for the provenance surface filter, T8.38.5): any
// tracked value occurring as a substring of content marks it as exposing
// a secret.
func (r *Registry) ContainsSensitive(content string) bool {
	for v := range r.sensitiveValues {
		if strings.Contains(content, v) {
			return true
		}
	}
	return false
}

// RegisterPromptSafe marks a variable path as prompt-safe with its value:
// the explicit registration that lets a setting enter templates (WBS
// 8.38, T8.38.3). A path whose field classifies sensitive is rejected —
// classification is the first default-deny layer (T8.38.1/T8.38.2).
// Re-registering overwrites the previous safe value.
func (r *Registry) RegisterPromptSafe(path, value string) error {
	_, field, err := ParsePath(path)
	if err != nil {
		return err
	}
	if IsSecretField(field) {
		return ErrSensitiveVariable{Path: path}
	}
	r.promptSafeVars[path] = value
	return nil
}

// Register (the unsafe path) additionally tracks a sensitive value for
// surface filtering when its field classifies sensitive (T8.38.5).
func (r *Registry) registerUnsafe(path, value string) {
	if _, field, err := ParsePath(path); err == nil && IsSecretField(field) {
		r.sensitiveValues[value] = true
	}
	r.vars[path] = value
}

// ProvenanceContentEntry is a WBS 8.31 provenance entry extended with the
// component's content so the WBS 8.38 provenance surface filter (T8.38.5)
// can drop entries whose content carries a sensitive value.
type ProvenanceContentEntry struct {
	ProvenanceEntry
	Content string
}

// ProvenanceMapWithContent derives provenance entries INCLUDING each
// component's content (every non-empty-content component, same major-
// component rule as ProvenanceMap), for the CleanProvenance surface.
func ProvenanceMapWithContent(components Components) []ProvenanceContentEntry {
	out := make([]ProvenanceContentEntry, 0, len(components))
	for _, c := range components {
		if c.Content == "" {
			continue
		}
		out = append(out, ProvenanceContentEntry{
			ProvenanceEntry: ProvenanceEntry{
				ComponentType: c.Type,
				SourceID:      c.SourceID,
				SourceVersion: c.SourceVersion,
				SourceHash:    c.SourceHash,
			},
			Content: c.Content,
		})
	}
	return out
}

// CleanProvenance drops every provenance entry whose content carries a
// value the registry tracks as sensitive (WBS 8.38, T8.38.5): a secret
// that reached a component by any path is stripped from the provenance
// surface. Entries are otherwise returned verbatim, in order.
func CleanProvenance(reg *Registry, entries []ProvenanceContentEntry) []ProvenanceContentEntry {
	out := make([]ProvenanceContentEntry, 0, len(entries))
	for _, e := range entries {
		if reg.ContainsSensitive(e.Content) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// PreviewText renders a parsed template for the preview surface (WBS 8.38,
// T8.38.4): a variable whose path is prompt-safe registered is inlined
// with its registered value; every other variable (unregistered, unsafe,
// sensitive) stays as its "{{path}}" placeholder — the preview never
// exposes a secret even when the registry holds one through the unsafe
// path. Text nodes pass through verbatim.
func PreviewText(ir *TemplateIR, reg *Registry) string {
	var b strings.Builder
	for _, n := range ir.Nodes {
		if n.IsText() {
			b.WriteString(n.Text)
			continue
		}
		if v, ok := reg.promptSafeVars[n.Path]; ok {
			b.WriteString(v)
		} else {
			b.WriteString("{{" + n.Path + "}}")
		}
	}
	return b.String()
}
