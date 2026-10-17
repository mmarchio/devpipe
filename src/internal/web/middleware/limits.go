package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// LimitConfig bounds request sizes (WBS 17.120).
type LimitConfig struct {
	// DefaultBody is the body limit for ordinary routes (default 1 MiB).
	DefaultBody int64
	// LargeBody applies to WBS, state-file and prompt-template routes
	// (default 2 MiB).
	LargeBody int64
	// MaxQuery bounds the raw query string (default 8 KiB).
	MaxQuery int
	// MaxQueryValue bounds each query parameter value (default 256).
	MaxQueryValue int
}

func (c LimitConfig) withDefaults() LimitConfig {
	if c.DefaultBody <= 0 {
		c.DefaultBody = 1 << 20
	}
	if c.LargeBody <= 0 {
		c.LargeBody = 2 << 20
	}
	if c.MaxQuery <= 0 {
		c.MaxQuery = 8 << 10
	}
	if c.MaxQueryValue <= 0 {
		c.MaxQueryValue = 256
	}
	return c
}

// largeBodyRoute reports routes that carry document content.
func largeBodyRoute(path string) bool {
	return strings.Contains(path, "/wbs") || strings.Contains(path, "/state-files/") ||
		strings.Contains(path, "/prompt-templates") || strings.Contains(path, "/system-prompt") ||
		strings.Contains(path, "/instructions")
}

// BodyLimit rejects oversized requests before any handler decodes them:
// the body is read up to the route's limit (413 beyond it) and replaced by
// an in-memory copy; over-long query strings are 413 and over-long query
// values 400 (WBS 17.120).
func BodyLimit(cfg LimitConfig, ew ErrorWriter) func(http.Handler) http.Handler {
	cfg = cfg.withDefaults()
	ew = writerOr(ew)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.URL.RawQuery) > cfg.MaxQuery {
				ew(w, r, errTooLarge)
				return
			}
			for _, vs := range r.URL.Query() {
				for _, v := range vs {
					if len(v) > cfg.MaxQueryValue {
						ew(w, r, &Rejection{Status: http.StatusBadRequest, Code: CodeInvalidInput, Message: "query parameter too long"})
						return
					}
				}
			}
			if r.Body != nil && r.Body != http.NoBody {
				limit := cfg.DefaultBody
				if largeBodyRoute(r.URL.Path) {
					limit = cfg.LargeBody
				}
				if r.ContentLength > limit {
					ew(w, r, errTooLarge)
					return
				}
				buf, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
				_ = r.Body.Close()
				if err != nil {
					ew(w, r, &Rejection{Status: http.StatusBadRequest, Code: CodeInvalidInput, Message: "request body could not be read"})
					return
				}
				if int64(len(buf)) > limit {
					ew(w, r, errTooLarge)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(buf))
				r.ContentLength = int64(len(buf))
			}
			next.ServeHTTP(w, r)
		})
	}
}
