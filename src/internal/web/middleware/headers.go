package middleware

import "net/http"

// ContentSecurityPolicy is the console's CSP (WBS 17.115): same-origin
// scripts, styles and connections only; no inline script, plugins, framing
// or base-URI rewriting.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// SecurityHeaders sets the security headers on every response (HTML, JSON,
// SSE, static and errors) before the handler writes (WBS 17.115).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
