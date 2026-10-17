package templates

import (
	"bytes"
	"embed"
	"fmt"
	"github.com/google/uuid"
	"html/template"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

//go:embed layout.html partials/*.html pages/*.html
var files embed.FS

// Renderer renders the console pages and fragments (WBS 17.2, 17.177):
// html/template escapes every untrusted value by context; output is
// buffered so a failing template yields a clean error instead of a
// half-written page.
type Renderer struct {
	pages map[string]*template.Template
	base  *template.Template
}

// New parses the layout, partials and every page.
func New(extra template.FuncMap) (*Renderer, error) {
	funcs := Funcs()
	for k, v := range extra {
		funcs[k] = v
	}
	base, err := template.New("layout.html").Funcs(funcs).ParseFS(files, "layout.html", "partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("templates: parse layout: %w", err)
	}
	r := &Renderer{pages: map[string]*template.Template{}, base: base}
	names, _ := fs.Glob(files, "pages/*.html")
	sort.Strings(names)
	for _, n := range names {
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := t.ParseFS(files, n); err != nil {
			return nil, fmt.Errorf("templates: parse %s: %w", n, err)
		}
		r.pages[strings.TrimSuffix(path.Base(n), ".html")] = t
	}
	return r, nil
}

// Pages lists the parsed page names.
func (r *Renderer) Pages() []string {
	out := make([]string, 0, len(r.pages))
	for n := range r.pages {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Page renders a full page through the layout.
func (r *Renderer) Page(w io.Writer, name string, data any) error {
	t, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("templates: unknown page %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// Fragment renders one named partial (live-update regions).
func (r *Renderer) Fragment(w io.Writer, name string, data any) error {
	var buf bytes.Buffer
	if err := r.base.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// statusKinds maps statuses to a visual class; the text itself is always
// rendered (WBS 17.147: never colour alone).
var statusKinds = map[string]string{
	"DONE": "ok", "PASS": "ok", "COMPLETED": "ok", "CREATED": "ok", "READY": "ok", "HEALTHY": "ok", "SUCCEEDED": "ok",
	"SYNCHRONIZED": "ok", "ACTIVE": "ok", "CURRENT": "ok", "CONFIRMED": "ok", "RESPONSE RECEIVED": "ok", "LIVE": "ok",
	"RUNNING": "info", "EXECUTING": "info", "VALIDATING": "info", "PLANNING": "info", "WAITING": "info", "SENDING": "info",
	"INITIALIZING": "info", "CONTINUING": "info", "CHECKPOINT": "info", "COMPLETE": "info", "PENDING": "muted",
	"PAUSED": "warn", "INCOMPLETE": "warn", "STALE": "warn", "BLOCKED": "warn", "SKIPPED": "muted", "REVIEW": "warn",
	"RECOVERY_REQUIRED": "bad", "FAILED": "bad", "FAIL": "bad", "INFRA_ERROR": "bad", "UNKNOWN": "bad", "DELIVERY_UNKNOWN": "bad",
	"CANCELLED": "muted", "ABANDONED": "muted", "UNHEALTHY": "bad", "REJECTED": "warn", "DRIFTED": "warn", "MISSING": "warn",
	"NOT_CONFIGURED": "warn", "UNPLANNED": "muted", "DRAFT": "muted",
}

var statusGlyphs = map[string]string{"ok": "✓", "info": "●", "warn": "!", "bad": "✗", "muted": "○"}

// StatusKind returns the class of a status ("muted" when unknown).
func StatusKind(s string) string {
	if k, ok := statusKinds[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return k
	}
	if strings.HasPrefix(strings.ToUpper(s), "DELIVERY UNKNOWN") {
		return "bad"
	}
	return "muted"
}

// Funcs is the template function map.
func Funcs() template.FuncMap {
	return template.FuncMap{
		"asset":       func(name string) string { return "/static/" + name },
		"statusKind":  StatusKind,
		"statusGlyph": func(s string) string { return statusGlyphs[StatusKind(s)] },
		// newKey is a fresh idempotency key per rendered form (WBS 18.11):
		// a resubmitted form repeats the key, so the action runs once.
		"newKey":   func() string { return uuid.NewString() },
		"fmtTime":  fmtTime,
		"ago":      ago,
		"pct":      func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
		"truncate": truncate,
		"bytes":    humanBytes,
		"join":     func(xs []string, sep string) string { return strings.Join(xs, sep) },
		"add":      func(a, b int) int { return a + b },
		"sub":      func(a, b int) int { return a - b },
		"contains": func(xs []string, s string) bool {
			for _, x := range xs {
				if x == s {
					return true
				}
			}
			return false
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					m[k] = kv[i+1]
				}
			}
			return m
		},
		"lower": strings.ToLower,
		"upper": strings.ToUpper,
		"title": func(s string) string { return strings.ReplaceAll(s, "_", " ") },
		"short": func(s string) string {
			if len(s) > 8 {
				return s[:8]
			}
			return s
		},
		"int64":   func(i int) int64 { return int64(i) },
		"float":   func(i int) float64 { return float64(i) },
		"seconds": func(ms int64) string { return fmt.Sprintf("%.1fs", float64(ms)/1000) },
		"deref": func(p any) any {
			switch v := p.(type) {
			case *int:
				if v == nil {
					return nil
				}
				return *v
			case *string:
				if v == nil {
					return nil
				}
				return *v
			case *float64:
				if v == nil {
					return nil
				}
				return *v
			}
			return p
		},
	}
}

func fmtTime(v any) string {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	case *time.Time:
		if t == nil || t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	case string:
		if pt, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return pt.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		if t == "" {
			return "—"
		}
		return t
	}
	return "—"
}

func ago(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "—"
		}
		t = *x
	default:
		return "—"
	}
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t).Truncate(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String() + " ago"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
