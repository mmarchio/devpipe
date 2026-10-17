package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// CSRFHeader / CSRFField carry the token on state-changing requests.
const (
	CSRFHeader = "X-CSRF-Token"
	CSRFField  = "csrf_token"
)

// CSRF protects state-changing browser requests (WBS 17.118). Tokens are
// HMAC-SHA256(key, session CSRF secret).
type CSRF struct {
	key []byte
	ew  ErrorWriter
}

// NewCSRF returns the protector; key must be secret (random per process
// unless configured).
func NewCSRF(key []byte, ew ErrorWriter) *CSRF {
	if len(key) == 0 {
		key = []byte(randomHex(32))
	}
	return &CSRF{key: key, ew: writerOr(ew)}
}

// Token returns the CSRF token for a session ("" without one).
func (c *CSRF) Token(s *Session) string {
	if s == nil {
		return ""
	}
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(s.CSRFSecret))
	return hex.EncodeToString(mac.Sum(nil))
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// isJSON reports an application/json request body.
func isJSON(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// sameOrigin reports whether the request's Origin / Sec-Fetch-Site headers
// (when present) name this server.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

// Middleware enforces the token: unsafe requests carrying a session, or
// with a non-JSON body type (forms, text/plain, multipart), must present a
// valid token; cookie-less JSON / body-less requests (programmatic API
// clients) must not come from another origin. Rejected requests never
// reach the handler.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		s := SessionFromContext(r.Context())
		_, cookieErr := r.Cookie(SessionCookieName)
		bodyTyped := r.Header.Get("Content-Type") != ""
		if s == nil && cookieErr == nil {
			// A session cookie the server no longer knows (expired,
			// rotated away, or from before a restart) cannot mutate
			// (WBS 18.82); the next page GET issues a fresh session.
			c.ew(w, r, errSessionExpired)
			return
		}
		if s != nil || cookieErr == nil || (bodyTyped && !isJSON(r)) {
			if !c.valid(r, s) {
				c.ew(w, r, errCSRF)
				return
			}
		}
		if !sameOrigin(r) {
			c.ew(w, r, errCSRF)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (c *CSRF) valid(r *http.Request, s *Session) bool {
	want := c.Token(s)
	if want == "" {
		return false
	}
	got := r.Header.Get(CSRFHeader)
	if got == "" && !isJSON(r) {
		// Form field (ParseForm reads the bounded body; handlers re-read
		// r.PostForm).
		if err := r.ParseMultipartForm(1 << 20); err != nil && err != http.ErrNotMultipart {
			return false
		}
		got = r.PostFormValue(CSRFField)
	}
	return hmac.Equal([]byte(got), []byte(want))
}
