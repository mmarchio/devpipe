package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"overseer/internal/access"
)

func okHandler(calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.WriteHeader(http.StatusOK)
	})
}

type failAuth struct{}

func (failAuth) Authenticate(*http.Request) (access.Principal, error) {
	return access.Principal{}, errors.New("no")
}

type userAuth struct{}

func (userAuth) Authenticate(*http.Request) (access.Principal, error) {
	return access.Principal{ID: "u1", Name: "User", Type: access.PrincipalUser}, nil
}

// P17-04 (WBS 17.126) — the principal comes from the authenticator through
// the context; failure is 401 before the handler.
func TestP1704Authenticate(t *testing.T) {
	var calls int
	var seen access.Principal
	h := Authenticate(LocalAuthenticator{}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		seen = PrincipalFromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if calls != 1 || seen != access.LocalAdmin() {
		t.Fatalf("calls %d principal %+v", calls, seen)
	}
	rec := httptest.NewRecorder()
	Authenticate(failAuth{}, nil)(okHandler(&calls)).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 401 || calls != 1 {
		t.Fatalf("unauthenticated: %d calls %d", rec.Code, calls)
	}
}

// P17-05 (WBS 17.128 / 17.129) — authorization runs before the handler; an
// unauthorized mutation never reaches it.
func TestP1705Authorize(t *testing.T) {
	var calls int
	res := func(r *http.Request) access.Resource { return access.Resource{Kind: "project", ProjectID: "p"} }
	h := Authenticate(userAuth{}, nil)(Authorize(access.LocalPolicy{}, access.ActionLifecyclePause, res, nil, okHandler(&calls)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/projects/p/pause", nil))
	if rec.Code != 403 || calls != 0 {
		t.Fatalf("user mutation: %d calls %d", rec.Code, calls)
	}
	h = Authenticate(userAuth{}, nil)(Authorize(access.LocalPolicy{}, access.ActionRead, res, nil, okHandler(&calls)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/projects/p", nil))
	if rec.Code != 200 || calls != 1 {
		t.Fatalf("user read: %d", rec.Code)
	}
	// No principal at all → 401.
	rec = httptest.NewRecorder()
	Authorize(access.LocalPolicy{}, access.ActionRead, res, nil, okHandler(&calls)).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 401 {
		t.Fatalf("no principal: %d", rec.Code)
	}
}

// P17-07 (WBS 17.115) — security headers on every response, errors too.
func TestP1707SecurityHeaders(t *testing.T) {
	for _, h := range []http.Handler{
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<p>x</p>")) }),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }),
	} {
		rec := httptest.NewRecorder()
		SecurityHeaders(h).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "same-origin", "X-Frame-Options": "DENY"} {
			if got := rec.Header().Get(k); got != want {
				t.Fatalf("%s = %q", k, got)
			}
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, d := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
			if !strings.Contains(csp, d) {
				t.Fatalf("CSP missing %s: %s", d, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("CSP allows inline: %s", csp)
		}
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// P17-08 (WBS 17.130) — cookie attributes, rotation, expiry.
func TestP1708SessionCookie(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)}
	m := NewSessionManager(SessionConfig{TTL: 2 * time.Hour, Rotate: 30 * time.Minute, Secure: true, Now: c.now})
	var seen *Session
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = SessionFromContext(r.Context()) }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/projects", nil))
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || seen == nil {
		t.Fatalf("no session issued: %v", cookies)
	}
	ck := cookies[0]
	if ck.Name != SessionCookieName || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.MaxAge != 7200 || ck.Path != "/" || len(ck.Value) != 64 {
		t.Fatalf("cookie attributes: %+v", ck)
	}
	first := *seen

	// API / static requests never create sessions.
	for _, p := range []string{"/api/v1/projects", "/static/css/app.css"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s issued a session", p)
		}
	}

	// Within the rotation interval the same id is kept.
	req := httptest.NewRequest("GET", "/projects", nil)
	req.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen.ID != first.ID || len(rec.Result().Cookies()) != 0 {
		t.Fatal("session changed before rotation")
	}

	// Rotation: new id, old id invalid, CSRF secret kept.
	c.t = c.t.Add(31 * time.Minute)
	req = httptest.NewRequest("GET", "/projects", nil)
	req.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	rot := rec.Result().Cookies()
	if len(rot) != 1 || rot[0].Value == ck.Value || seen.CSRFSecret != first.CSRFSecret {
		t.Fatalf("rotation: %v", rot)
	}
	if m.lookup(ck.Value) != nil {
		t.Fatal("old id still valid after rotation")
	}

	// Expiry: the session disappears; an API request gets none.
	c.t = c.t.Add(3 * time.Hour)
	req = httptest.NewRequest("POST", "/api/v1/projects", nil)
	req.AddCookie(rot[0])
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != nil {
		t.Fatal("expired session accepted")
	}
}

// P17-09 (WBS 17.118) — CSRF tokens.
func TestP1709CSRF(t *testing.T) {
	m := NewSessionManager(SessionConfig{})
	csrf := NewCSRF([]byte("k"), nil)
	var calls int
	h := m.Middleware(csrf.Middleware(okHandler(&calls)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/projects", nil))
	ck := rec.Result().Cookies()[0]
	s := m.lookup(ck.Value)
	token := csrf.Token(s)

	other := m.create()
	post := func(body, ctype, hdr string, withCookie bool, origin string) int {
		req := httptest.NewRequest("POST", "/projects/x/pause", strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if hdr != "" {
			req.Header.Set(CSRFHeader, hdr)
		}
		if withCookie {
			req.AddCookie(ck)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	form := "application/x-www-form-urlencoded"
	cases := []struct {
		name string
		code int
		want int
	}{
		{"form without token", post("a=1", form, "", true, ""), 403},
		{"form with invalid token", post("csrf_token=nope", form, "", true, ""), 403},
		{"form with other session's token", post("csrf_token="+url.QueryEscape(csrf.Token(other)), form, "", true, ""), 403},
		{"form with token field", post("csrf_token="+token, form, "", true, ""), 200},
		{"json with header", post("{}", "application/json", token, true, ""), 200},
		{"json with cookie, no token", post("{}", "application/json", "", true, ""), 403},
		{"cookie-less form", post("a=1", form, "", false, ""), 403},
		{"cookie-less text/plain", post("a=1", "text/plain", "", false, ""), 403},
		{"cookie-less json api client", post("{}", "application/json", "", false, ""), 200},
		{"cookie-less body-less api client", post("", "", "", false, ""), 200},
		{"cookie-less json cross-site", post("{}", "application/json", "", false, "https://evil.example"), 403},
		{"token but cross-site origin", post("{}", "application/json", token, true, "https://evil.example"), 403},
		{"same origin header", post("{}", "application/json", token, true, "http://example.com"), 200},
	}
	for _, c := range cases {
		if c.code != c.want {
			t.Errorf("%s: %d want %d", c.name, c.code, c.want)
		}
	}
	// Safe methods never need tokens.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/projects", nil)
	req.AddCookie(ck)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("GET rejected")
	}
}

// P17-11 (WBS 17.120) — oversized bodies / queries rejected before the
// handler; normal bodies pass intact.
func TestP1711BodyLimit(t *testing.T) {
	var calls int
	var got string
	h := BodyLimit(LimitConfig{DefaultBody: 10, LargeBody: 20}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b := make([]byte, 100)
		n, _ := r.Body.Read(b)
		got = string(b[:n])
	}))
	do := func(path, body string, chunked bool) int {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		if chunked {
			req.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("/api/v1/projects", "0123456789", false); c != 200 || got != "0123456789" {
		t.Fatalf("at limit: %d %q", c, got)
	}
	if c := do("/api/v1/projects", "0123456789x", false); c != 413 {
		t.Fatalf("over limit: %d", c)
	}
	if c := do("/api/v1/projects", "0123456789x", true); c != 413 {
		t.Fatalf("chunked over limit: %d", c)
	}
	if c := do("/api/v1/projects/p/phases/f/wbs", strings.Repeat("a", 20), false); c != 200 {
		t.Fatalf("large route: %d", c)
	}
	if c := do("/api/v1/projects/p/phases/f/wbs", strings.Repeat("a", 21), false); c != 413 {
		t.Fatalf("large route over: %d", c)
	}
	before := calls
	if c := do("/api/v1/projects?name="+strings.Repeat("a", 300), "", false); c != 400 {
		t.Fatalf("long value: %d", c)
	}
	if c := do("/api/v1/projects?"+strings.Repeat("a=b&", 3000), "", false); c != 413 {
		t.Fatalf("long query: %d", c)
	}
	if calls != before {
		t.Fatal("rejected request reached handler")
	}
}

// P17-12 (WBS 17.154) — expensive routes limited per principal; others not.
func TestP1712RateLimit(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	l := NewRateLimiter(RateConfig{PerSecond: 1, Burst: 2, Now: c.now}, nil)
	var calls int
	h := Authenticate(LocalAuthenticator{}, nil)(l.Middleware(okHandler(&calls)))
	do := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", p, nil))
		return rec
	}
	p := "/api/v1/projects/x/prompts/preview"
	if do(p).Code != 200 || do(p).Code != 200 {
		t.Fatal("burst denied")
	}
	rec := do(p)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over limit: %d", rec.Code)
	}
	for i := 0; i < 20; i++ {
		if do("/api/v1/projects/x/pause").Code != 200 {
			t.Fatal("non-expensive route limited")
		}
	}
	c.t = c.t.Add(time.Second)
	if do(p).Code != 200 {
		t.Fatal("refill failed")
	}
}

// T18.82 — an expired session cannot continue mutations (distinct
// SESSION_EXPIRED rejection, handler never reached); the next page GET
// issues a fresh session that works.
func TestT1882SessionExpiration(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)}
	m := NewSessionManager(SessionConfig{TTL: time.Hour, Rotate: 2 * time.Hour, Now: c.now})
	var lastCode string
	csrf := NewCSRF([]byte("k"), func(w http.ResponseWriter, _ *http.Request, err error) {
		rej := err.(*Rejection)
		lastCode = rej.Code
		w.WriteHeader(rej.Status)
	})
	var calls int
	h := m.Middleware(csrf.Middleware(okHandler(&calls)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/projects", nil))
	ck := rec.Result().Cookies()[0]
	token := csrf.Token(m.lookup(ck.Value))
	post := func(cookie *http.Cookie, tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/projects/x/pause", strings.NewReader("csrf_token="+tok))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	base := calls
	if r := post(ck, token); r.Code != 200 || calls != base+1 {
		t.Fatalf("live session: %d", r.Code)
	}
	c.t = c.t.Add(61 * time.Minute)
	r := post(ck, token)
	if r.Code != http.StatusForbidden || lastCode != CodeSessionExpired || calls != base+1 {
		t.Fatalf("expired session mutated: %d %s", r.Code, r.Body.String())
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/projects", nil)
	req.AddCookie(ck)
	h.ServeHTTP(rec, req)
	fresh := rec.Result().Cookies()
	if len(fresh) != 1 || fresh[0].Value == ck.Value {
		t.Fatal("no fresh session issued")
	}
	if r := post(fresh[0], csrf.Token(m.lookup(fresh[0].Value))); r.Code != 200 || calls != base+3 {
		t.Fatalf("fresh session: %d", r.Code)
	}
}
