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

type principalContextKey struct{}

// WithPrincipal returns a copy of ctx carrying the resolved principal.
//
// P4-A: the principal — not the bare user — is what the middleware attaches,
// so "who is this" and "how did they authenticate" travel together and are
// resolved exactly once, at the edge. WithUser is the pre-P4-A shorthand,
// preserved for the callers (and tests) that only ever had a user.
func WithPrincipal(ctx context.Context, p domain.Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// WithUser returns a copy of ctx carrying the resolved current user, with no
// recorded authentication method. Equivalent to WithPrincipal for a principal
// carrying only a user.
func WithUser(ctx context.Context, u domain.User) context.Context {
	return WithPrincipal(ctx, domain.Principal{User: u})
}

// PrincipalFromContext returns the request's resolved principal, if any.
func PrincipalFromContext(ctx context.Context) (domain.Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(domain.Principal)
	return p, ok
}

// FromContext returns the request's resolved current user, if any. ok is
// false when no session cookie resolved (multi-user mode with no/invalid
// cookie, and trusted-local mode did not synthesize one either).
func FromContext(ctx context.Context) (domain.User, bool) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return domain.User{}, false
	}
	return p.User, true
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

// Unauthorized is the canonical 401 for a request that resolved no identity.
// Callers that discover this outside Require/RequirePrincipal (a list handler
// that resolves a subject rather than a user, say) use this so every
// unauthenticated response carries the same code and message.
func Unauthorized() error {
	return apierr.Unauthorized("NOT_AUTHENTICATED", "authentication required")
}

// RequirePrincipal is Require's full-fidelity form: the resolved user plus how
// they authenticated. P4-B's authorization checks read this; P4-A only ever
// answers who authenticated, never what they may do.
func RequirePrincipal(r *http.Request) (domain.Principal, error) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		return domain.Principal{}, apierr.Unauthorized("NOT_AUTHENTICATED", "authentication required")
	}
	return p, nil
}

// Resolver is the subset of authsvc.Manager the identity middleware needs.
// Kept narrow so router.go doesn't have to import the service package
// directly.
//
// P4-A widened it from ResolveSession to ResolvePrincipal: the middleware now
// attaches the authentication method and (for a federated session) the
// issuer/subject alongside the user, so no layer above it re-derives any of
// that — the point of having one canonical principal at all.
type Resolver interface {
	ResolvePrincipal(ctx context.Context, rawToken string) (domain.Principal, error)
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
					if p, err := resolver.ResolvePrincipal(r.Context(), c.Value); err == nil {
						next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
						return
					}
				}
			}
			if trustedLocal && bootstrapAdmin != nil {
				if u, ok := bootstrapAdmin(r.Context()); ok {
					// Recorded as trusted_local, not as a login: no credential
					// was presented, and the audit trail says so.
					next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(),
						domain.Principal{User: u, AuthMethod: domain.AuthMethodTrustedLocal})))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
