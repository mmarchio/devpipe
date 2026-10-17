package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionCookieName is the browser session cookie (WBS 17.130).
const SessionCookieName = "overseer_session"

// SessionConfig tunes browser sessions.
type SessionConfig struct {
	// TTL is the absolute session lifetime (default 12 h).
	TTL time.Duration
	// Rotate is the id rotation interval (default 1 h); after it the next
	// request receives a new id and the old one stops working.
	Rotate time.Duration
	// Secure sets the cookie Secure attribute (default true; disable only
	// for plain-http local use).
	Secure bool
	// Now is the clock (tests).
	Now func() time.Time
}

// Session is one browser session. CSRFSecret is stable for the session's
// life, so id rotation does not invalidate tokens in open tabs.
type Session struct {
	ID         string
	CSRFSecret string
	Created    time.Time
	IDIssued   time.Time
	Expires    time.Time
}

// SessionManager is an in-memory browser session store. Sessions carry no
// workflow state: a restart only forces a new session (WBS 17.138).
type SessionManager struct {
	cfg SessionConfig
	mu  sync.Mutex
	m   map[string]*Session
}

// NewSessionManager returns a manager with defaults applied.
func NewSessionManager(cfg SessionConfig) *SessionManager {
	if cfg.TTL <= 0 {
		cfg.TTL = 12 * time.Hour
	}
	if cfg.Rotate <= 0 {
		cfg.Rotate = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SessionManager{cfg: cfg, m: map[string]*Session{}}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("session: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// lookup returns the live session for id (nil when absent / expired).
func (m *SessionManager) lookup(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.m[id]
	if s == nil {
		return nil
	}
	if !m.cfg.Now().Before(s.Expires) {
		delete(m.m, id)
		return nil
	}
	cp := *s
	return &cp
}

func (m *SessionManager) create() *Session {
	now := m.cfg.Now()
	s := &Session{ID: randomHex(32), CSRFSecret: randomHex(32), Created: now, IDIssued: now, Expires: now.Add(m.cfg.TTL)}
	m.mu.Lock()
	m.m[s.ID] = s
	m.mu.Unlock()
	cp := *s
	return &cp
}

// rotate replaces the session id (old id invalid immediately).
func (m *SessionManager) rotate(old *Session) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.m[old.ID]
	if cur == nil {
		return old
	}
	delete(m.m, old.ID)
	cur.ID = randomHex(32)
	cur.IDIssued = m.cfg.Now()
	m.m[cur.ID] = cur
	cp := *cur
	return &cp
}

// Cookie returns the Set-Cookie value for s.
func (m *SessionManager) Cookie(s *Session) *http.Cookie {
	maxAge := int(s.Expires.Sub(m.cfg.Now()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	return &http.Cookie{
		Name: SessionCookieName, Value: s.ID, Path: "/", MaxAge: maxAge,
		Expires: s.Expires.UTC(), HttpOnly: true, Secure: m.cfg.Secure, SameSite: http.SameSiteStrictMode,
	}
}

type sessionKey struct{}

// SessionFromContext returns the request's session (nil when none).
func SessionFromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(sessionKey{}).(*Session)
	return s
}

// issuesSession reports whether a request may receive a new session: page
// (non-API, non-static) GETs only.
func issuesSession(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	return !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/static/") && p != "/health" && p != "/ready"
}

// Middleware attaches the session (rotating its id when due) and issues a
// new one on page GETs without a valid session (WBS 17.130).
func (m *SessionManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s *Session
		if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
			s = m.lookup(c.Value)
		}
		switch {
		case s != nil && m.cfg.Now().Sub(s.IDIssued) >= m.cfg.Rotate:
			s = m.rotate(s)
			http.SetCookie(w, m.Cookie(s))
		case s == nil && issuesSession(r):
			s = m.create()
			http.SetCookie(w, m.Cookie(s))
		}
		if s != nil {
			r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, s))
		}
		next.ServeHTTP(w, r)
	})
}
