package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseHTML = `<!doctype html>
<html>
<head><meta charset="utf-8"><title>{{block "title" .}}Overseer{{end}}</title>
<link rel="stylesheet" href="/static/styles.css"></head>
<body>
<nav>
<a href="/projects">Projects</a>
<a href="/executions">Executions</a>
<a href="/roles">Roles</a>
<a href="/settings">Settings</a>
</nav>
<main>{{block "content" .}}{{end}}</main>
</body>
</html>`

const homeHTML = `{{define "title"}}Home - Overseer{{end}}
{{define "content"}}<h1>Overseer</h1><p class="tagline">Development overseer.</p><span>{{.Subtitle}}</span>{{end}}`

const errorHTML = `{{define "title"}}Error - Overseer{{end}}
{{define "content"}}<h1>{{.Code}}</h1><p>{{.Message}}</p>{{end}}`

func writeTemplates(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"base.html":  baseHTML,
		"home.html":  homeHTML,
		"error.html": errorHTML,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// T1.10.1 — Base template renders.
func TestBaseTemplateRenders(t *testing.T) {
	rnd, err := NewRenderer(writeTemplates(t))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := rnd.Render(rec, httptest.NewRequest("GET", "/", nil), "home", map[string]any{}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<html>") || !strings.Contains(body, "</html>") {
		t.Errorf("not a valid HTML shell: %s", body)
	}
}

// T1.10.2 — Home page uses the base layout (nav + home content).
func TestHomeUsesBaseLayout(t *testing.T) {
	rnd, _ := NewRenderer(writeTemplates(t))
	rec := httptest.NewRecorder()
	_ = rnd.Render(rec, httptest.NewRequest("GET", "/", nil), "home", map[string]any{})
	body := rec.Body.String()
	for _, want := range []string{"Projects", "Executions", "Roles", "Settings", "Development overseer"} {
		if !strings.Contains(body, want) {
			t.Errorf("home page missing %q", want)
		}
	}
}

// T1.10.3 — Static assets (CSS) served.
func TestStaticAssetsServed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "styles.css"), []byte("body { color: black; }"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(RouterConfig{StaticDir: dir})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/static/styles.css", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "color: black") {
		t.Fatalf("css = %d body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Errorf("css content-type = %q", ct)
	}
}

// T1.10.4 — Missing template produces a controlled error, not a panic.
func TestMissingTemplateControlledError(t *testing.T) {
	rnd, _ := NewRenderer(writeTemplates(t))
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("Render panicked: %v", p)
			}
		}()
		_ = rnd.Render(rec, httptest.NewRequest("GET", "/", nil), "nope", map[string]any{})
	}()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing template status = %d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
}

// T1.10.5 — Template execution error is controlled; the server keeps serving.
func TestTemplateExecutionErrorNoPanic(t *testing.T) {
	rnd, _ := NewRenderer(writeTemplates(t))
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("Render panicked: %v", p)
			}
		}()
		// String where the template expects a map → execution error.
		_ = rnd.Render(rec, httptest.NewRequest("GET", "/", nil), "home", "not-a-map")
	}()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("execution error status = %d, want 500", rec.Code)
	}
	// Server keeps serving: a subsequent render succeeds.
	rec2 := httptest.NewRecorder()
	if err := rnd.Render(rec2, httptest.NewRequest("GET", "/", nil), "home", map[string]any{}); err != nil || rec2.Code != http.StatusOK {
		t.Fatalf("post-error render = %d, %v", rec2.Code, err)
	}
}

// Router integration: / renders home through the base layout when a renderer is set.
func TestRouterRendersHome(t *testing.T) {
	r := NewRouter(RouterConfig{})
	rnd, err := NewRenderer(writeTemplates(t))
	if err != nil {
		t.Fatal(err)
	}
	r.SetRenderer(rnd)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Projects") {
		t.Fatalf("GET / = %d, layout missing", rec.Code)
	}
}
