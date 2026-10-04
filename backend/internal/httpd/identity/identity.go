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
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// SessionCookieName carries the application-identity session token. Distinct
// from the LAN bridge's preview-file auth cookie (httpd/auth.go's
// authCookieName) — the two never share a name or a code path.
const SessionCookieName = "ao_session"

// AgentTokenHeader carries the credential an AO-LAUNCHED AGENT presents (P4-I).
//
// A header, deliberately, and never a cookie: an agent is not a browser, and the
// two credential kinds must be impossible to confuse. A browser session
// presented here would not resolve, and an agent credential presented in the
// cookie would not resolve either -- each is only ever read by the resolver that
// knows what it is.
//
//nolint:gosec // G101: an HTTP header name, not a credential.
const AgentTokenHeader = "X-AO-Agent-Token"

// AgentResolver turns a raw agent credential into a principal carrying the
// launch's bounded authority. Nil disables agent identity entirely, which is
// what every pre-P4-I wiring (and every test that wires no identity at all)
// gets.
type AgentResolver interface {
	ResolveAgentPrincipal(ctx context.Context, rawToken string) (domain.Principal, error)
}

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
// P4-I: agents, when wired, are resolved BEFORE the cookie and from their own
// header. See the comments in the body for why a presented agent credential is
// never allowed to fall back onto a browser session.
//
// trustedLocal true (the default) makes a request with no session cookie
// resolve to whatever bootstrapAdmin returns (when it returns ok) rather
// than to "no user" — this is what keeps today's single-user desktop flow
// visibly unchanged: no login screen, every route behaves as it always has.
func Middleware(resolver Resolver, agents AgentResolver, trustedLocal bool, bootstrapAdmin func(ctx context.Context) (domain.User, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// P4-I: an agent credential is decided FIRST and decided ALONE.
			//
			// Presenting the header is a claim about what this request is, and
			// once made it is the only identity considered: a bad agent token
			// resolves to NO principal rather than falling through to the
			// cookie or to trusted-local synthesis. Falling through is the
			// dangerous direction -- it would let a stale agent token quietly
			// borrow whatever identity happened to be lying around, which is
			// the escalation this whole mechanism exists to avoid.
			agentClaimed := false
			if raw := strings.TrimSpace(r.Header.Get(AgentTokenHeader)); raw != "" {
				agentClaimed = true
				if agents != nil {
					if p, err := agents.ResolveAgentPrincipal(r.Context(), raw); err == nil && p.IsAgent() {
						next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
						return
					}
				}
			}
			if resolver != nil && !agentClaimed {
				if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
					if p, err := resolver.ResolvePrincipal(r.Context(), c.Value); err == nil {
						next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
						return
					}
				}
			}
			// A failed agent claim falls through to trusted-local synthesis but
			// NEVER to the cookie. On a desktop install the loopback listener is
			// itself the trust boundary, so an agent whose credential expired
			// behaves exactly as it did before this mechanism existed. On a
			// multi-user install trustedLocal is off by construction, so the
			// same request resolves no identity at all and is answered 401 --
			// which is the honest answer, and is recoverable by relaunching the
			// agent rather than by borrowing somebody's session.
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
