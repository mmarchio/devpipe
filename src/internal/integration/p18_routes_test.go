package integration

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"overseer/internal/access"
)

// T18.80 / T18.81 / T18.225 — route inventory: every state-changing route
// carries a mutation (or explicit preview) action and is CSRF-protected —
// a browser session without its token, a cross-origin call and a form
// body without a token are refused before any handler runs.
func TestT1880RouteInventory(t *testing.T) {
	r := p17NewRig(t)
	routes := r.h.App.Router().Routes()
	param := regexp.MustCompile(`\{[a-zA-Z]+\}`)
	anon := &http.Client{}
	unsafe := 0
	for route, action := range routes {
		method, pattern, _ := strings.Cut(route, " ")
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			continue
		}
		unsafe++
		if !action.IsMutation() && action != access.ActionPreview {
			t.Errorf("%s: action %s is neither a mutation nor a preview", route, action)
		}
		path := param.ReplaceAllString(pattern, r.pid.String())
		// v1 authentication is "local" (every caller is the local
		// administrator, production requires the trusted-local
		// acknowledgement); cookie-less JSON API calls are programmatic
		// clients. They are still refused cross-origin, and a form body
		// without a token is refused (CSRF).
		for _, c := range []struct{ ctype, origin string }{{"application/json", "https://evil.example"}, {"application/x-www-form-urlencoded", ""}} {
			req, _ := http.NewRequest(method, r.h.BaseURL+path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", c.ctype)
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			resp, err := anon.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s cookie-less %s from %q: %d", route, c.ctype, c.origin, resp.StatusCode)
			}
		}
		// A session but no CSRF token.
		r.b.page("/")
		req, _ := http.NewRequest(method, r.h.BaseURL+path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := r.b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a CSRF token: %d", route, resp.StatusCode)
		}
	}
	if unsafe < 50 {
		t.Fatalf("only %d unsafe routes inventoried", unsafe)
	}
	t.Logf("%d unsafe routes checked", unsafe)
}
