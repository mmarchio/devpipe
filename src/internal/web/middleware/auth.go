package middleware

import (
	"context"
	"net/http"

	"overseer/internal/access"
)

// Authenticator resolves the principal of a request (WBS 17.126). Errors
// mean the request is unauthenticated.
type Authenticator interface {
	Authenticate(*http.Request) (access.Principal, error)
}

// LocalAuthenticator is the v1 authenticator for a local / internal
// deployment (config web.auth_mode: local): every request acts as the local
// administrator. Handlers still read the principal from the context and
// pass it through the Authorizer, so a real authenticator replaces this
// without handler changes.
type LocalAuthenticator struct{}

// Authenticate implements Authenticator.
func (LocalAuthenticator) Authenticate(*http.Request) (access.Principal, error) {
	return access.LocalAdmin(), nil
}

type principalKey struct{}

// WithPrincipal stores the authenticated principal in ctx.
func WithPrincipal(ctx context.Context, p access.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the authenticated principal (zero when the
// request was not authenticated).
func PrincipalFromContext(ctx context.Context) access.Principal {
	p, _ := ctx.Value(principalKey{}).(access.Principal)
	return p
}

// Authenticate runs the authenticator for every request; failure (or an
// invalid principal) is a 401 before any handler (WBS 17.126).
func Authenticate(a Authenticator, ew ErrorWriter) func(http.Handler) http.Handler {
	ew = writerOr(ew)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.Authenticate(r)
			if err != nil || p.Validate() != nil {
				ew(w, r, errUnauthenticated)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// Authorize wraps one route: the principal in context must be allowed the
// action on the resource resolved from the request, otherwise 403 before
// the handler runs (WBS 17.128 / 17.129).
func Authorize(z access.Authorizer, action access.Action, resource func(*http.Request) access.Resource, ew ErrorWriter, next http.Handler) http.Handler {
	ew = writerOr(ew)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFromContext(r.Context())
		if p.IsZero() {
			ew(w, r, errUnauthenticated)
			return
		}
		if !z.Can(p, action, resource(r)) {
			ew(w, r, errForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
