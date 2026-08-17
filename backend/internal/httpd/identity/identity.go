// Package identity carries Checkpoint 8P-A's resolved-current-user request
// context helpers. It is deliberately a leaf package (imports only domain
// and apierr) so both httpd (which wires the resolving middleware) and
// httpd/controllers (which reads the resolved user) can depend on it without
// an import cycle — controllers cannot import the parent httpd package,
// which is what forces this out of router.go itself.
package identity

import (
	"context"
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// SessionCookieName carries the application-identity session token. Distinct
// from the LAN bridge's preview-file auth cookie (httpd/auth.go's
// authCookieName) — the two never share a name or a code path.
const SessionCookieName = "ao_session"

type userContextKey struct{}

// WithUser returns a copy of ctx carrying the resolved current user.
func WithUser(ctx context.Context, u domain.User) context.Context {
	return context.WithValue(ctx, userContextKey{}, u)
}

// FromContext returns the request's resolved current user, if any. ok is
// false when no session cookie resolved (multi-user mode with no/invalid
// cookie, and trusted-local mode did not synthesize one either).
func FromContext(ctx context.Context) (domain.User, bool) {
	u, ok := ctx.Value(userContextKey{}).(domain.User)
	return u, ok
}

// Require returns the request's resolved current user, or a structured 401
// apierr.Error suitable for envelope.WriteError when none resolved.
// Controllers call this at the top of any owner-scoped handler; in
// trusted-local mode a user is always resolved (the bootstrap admin), so
// this only ever fails in multi-user mode with no/invalid session cookie.
func Require(r *http.Request) (domain.User, error) {
	u, ok := FromContext(r.Context())
	if !ok {
		return domain.User{}, apierr.Unauthorized("NOT_AUTHENTICATED", "authentication required")
	}
	return u, nil
}

// Resolver is the subset of authsvc.Manager the identity middleware needs.
// Kept narrow so router.go doesn't have to import the service package
// directly.
type Resolver interface {
	ResolveSession(ctx context.Context, rawToken string) (domain.User, error)
}

// Middleware resolves the request's application-identity user from the
// session cookie, if present, and attaches it to the request context.
// Modeled exactly on router.go's previewOriginMiddleware shape: it NEVER
// rejects the request. Authorization (401/404 on a missing/foreign identity)
// happens later, in service/controller code, via envelope.WriteError — same
// as any other business-logic error. This is a hard rule, not a style
// choice: the primary loopback listener stays unauthenticated at the
// network level (see AGENTS.md); this middleware only ever attaches
// identity, it never gates it.
//
// trustedLocal true (the default) makes a request with no session cookie
// resolve to whatever bootstrapAdmin returns (when it returns ok) rather
// than to "no user" — this is what keeps today's single-user desktop flow
// visibly unchanged: no login screen, every route behaves as it always has.
func Middleware(resolver Resolver, trustedLocal bool, bootstrapAdmin func(ctx context.Context) (domain.User, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if resolver != nil {
				if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
					if u, err := resolver.ResolveSession(r.Context(), c.Value); err == nil {
						next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
						return
					}
				}
			}
			if trustedLocal && bootstrapAdmin != nil {
				if u, ok := bootstrapAdmin(r.Context()); ok {
					next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
