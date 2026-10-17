package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"overseer/internal/access"
)

type userAuthn struct{}

func (userAuthn) Authenticate(*http.Request) (access.Principal, error) {
	return access.Principal{ID: "u", Name: "U", Type: access.PrincipalUser}, nil
}

type deniedAuthn struct{}

func (deniedAuthn) Authenticate(*http.Request) (access.Principal, error) {
	return access.Principal{}, errors.New("no credentials")
}

// P17-05 (WBS 17.128) — unsafe routes need an action; unknown ones fail
// registration.
func TestP1705UnsafeRouteNeedsAction(t *testing.T) {
	r := NewRouter(RouterConfig{})
	noop := func(w http.ResponseWriter, _ *http.Request) {}
	if err := r.RegisterAPI(http.MethodPost, "/api/v1/projects/{id}/explode", noop); err == nil {
		t.Fatal("unlisted unsafe route registered")
	}
	if err := r.RegisterAPI(http.MethodPost, "/api/v1/projects/{id}/pause", noop); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAPIAction(http.MethodGet, "/api/v1/x", access.ActionLifecyclePause, noop); err == nil {
		t.Fatal("mutation action on GET accepted")
	}
	if err := r.RegisterAPIAction(http.MethodPost, "/api/v1/y", access.ActionRead, noop); err == nil {
		t.Fatal("READ on POST accepted")
	}
	if got := r.Routes()["POST /api/v1/projects/{id}/pause"]; got != access.ActionLifecyclePause {
		t.Fatalf("route action %q", got)
	}
}

// P17-05 (T17.129) — an unauthorized mutation is rejected before the
// handler (service call count 0); reads stay allowed.
func TestP1705UnauthorizedMutationRejectedBeforeService(t *testing.T) {
	r := NewRouter(RouterConfig{Authenticator: userAuthn{}})
	calls := 0
	h := func(w http.ResponseWriter, _ *http.Request) { calls++ }
	_ = r.RegisterAPI(http.MethodPost, "/api/v1/projects/{id}/pause", h)
	_ = r.RegisterAPI(http.MethodGet, "/api/v1/projects/{id}", h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/pause", nil))
	if rec.Code != http.StatusForbidden || calls != 0 || !strings.Contains(rec.Body.String(), "FORBIDDEN") {
		t.Fatalf("mutation: %d calls %d %s", rec.Code, calls, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1", nil))
	if rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("read: %d", rec.Code)
	}
	// Unauthenticated → 401 for every route.
	r2 := NewRouter(RouterConfig{Authenticator: deniedAuthn{}})
	_ = r2.RegisterAPI(http.MethodGet, "/api/v1/projects/{id}", h)
	rec = httptest.NewRecorder()
	r2.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1", nil))
	if rec.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
}

// P17-05 — the authorization resource carries the project scope.
func TestP1705ResourceFromPath(t *testing.T) {
	vals := map[string]string{"project": "p1", "execution": "e1", "id": "p2", "phaseID": "f1"}
	get := func(k string) string { return vals[k] }
	res := resourceFor("/api/v1/projects/{project}/executions/{execution}/pause", get)
	if res.ProjectID != "p1" || res.ID != "e1" || res.Kind != "pause" {
		t.Fatalf("%+v", res)
	}
	res = resourceFor("/api/v1/projects/{id}/phases/{phaseID}/wbs", get)
	if res.ProjectID != "p2" || res.ID != "f1" || res.Kind != "wbs" {
		t.Fatalf("%+v", res)
	}
	if res := resourceFor("/api/v1/projects", get); res.ProjectID != "" || res.Kind != "projects" {
		t.Fatalf("%+v", res)
	}
}

// P17-10 (WBS 17.119 / 17.73) — the action table never assigns a READ
// action to an unsafe method and every table entry is a mutation or a
// preview.
func TestP1710ActionTableMethodSafety(t *testing.T) {
	for route, a := range RouteActions() {
		method := strings.SplitN(route, " ", 2)[0]
		if method == http.MethodGet || method == http.MethodHead {
			t.Fatalf("safe method in unsafe table: %s", route)
		}
		if !a.Valid() || a == access.ActionRead {
			t.Fatalf("%s has action %q", route, a)
		}
	}
}

// P17-05 — every console API route registers (each unsafe one has an
// action in the table).
func TestP1705ConsoleRoutesRegister(t *testing.T) {
	r := NewRouter(RouterConfig{})
	if err := RegisterConsoleRoutes(r, &ConsoleAPI{}); err != nil {
		t.Fatal(err)
	}
}
