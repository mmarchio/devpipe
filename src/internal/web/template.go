package web

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
)

// Renderer serves server-rendered HTML pages through a shared base layout.
type Renderer struct {
	pages map[string]*template.Template
}

// NewRenderer parses base.html + the page templates from dir. The base
// template defines the layout blocks; each page defines "title" and
// "content".
func NewRenderer(dir string) (*Renderer, error) {
	base, err := template.ParseFiles(dir + "/base.html")
	if err != nil {
		return nil, fmt.Errorf("template: parse base: %w", err)
	}
	r := &Renderer{pages: map[string]*template.Template{}}
	for _, name := range []string{"home.html", "error.html"} {
		tmpl, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("template: clone %s: %w", name, err)
		}
		tmpl, err = tmpl.ParseFiles(dir + "/" + name)
		if err != nil {
			return nil, fmt.Errorf("template: parse %s: %w", name, err)
		}
		r.pages[name[:len(name)-len(".html")]] = tmpl
	}
	return r, nil
}

// Render writes the named page through the base layout. Controlled errors:
// an unknown page or a template execution error returns a 500 JSON response
// via WriteError — it never panics (T1.10.4/T1.10.5). Output is buffered so
// the status code is correct even when execution fails.
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, page string, data any) error {
	tmpl, ok := r.pages[page]
	if !ok {
		WriteError(w, req, NewInternalError(fmt.Sprintf("template %q not found", page)))
		return NewInternalError(fmt.Sprintf("template %q not found", page))
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base.html", data); err != nil {
		WriteError(w, req, NewInternalError("template execution failed"))
		return NewInternalError("template execution failed")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
	return nil
}
