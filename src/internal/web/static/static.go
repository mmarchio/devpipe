package static

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed css/*.css js/*.js images/*.svg
var files embed.FS

// Assets serves the embedded files under /static/ with content-hash
// cache busting: URL(name) returns "/static/<name>?v=<hash>" and responses
// carry a long-lived immutable Cache-Control when the hash matches.
type Assets struct {
	hash map[string]string
}

// New indexes the embedded files.
func New() *Assets {
	a := &Assets{hash: map[string]string{}}
	_ = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		a.hash[p] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	return a
}

// URL returns the cache-busted URL of an asset ("css/app.css").
func (a *Assets) URL(name string) string {
	if h, ok := a.hash[name]; ok {
		return "/static/" + name + "?v=" + h
	}
	return "/static/" + name
}

// Names lists the embedded assets.
func (a *Assets) Names() []string {
	out := make([]string, 0, len(a.hash))
	for n := range a.hash {
		out = append(out, n)
	}
	return out
}

// ServeHTTP serves one asset.
func (a *Assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/static/")), "/")
	h, ok := a.hash[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := files.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if r.URL.Query().Get("v") == h {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("ETag", `"`+h+`"`)
	if r.Header.Get("If-None-Match") == `"`+h+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}
