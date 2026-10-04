package identity_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

type stubResolver struct {
	principal domain.Principal
	err       error
	calls     int
}

func (s *stubResolver) ResolvePrincipal(_ context.Context, raw string) (domain.Principal, error) {
	s.calls++
	if s.err != nil {
		return domain.Principal{}, s.err
	}
	return s.principal, nil
}

type stubAgents struct {
	byToken map[string]domain.Principal
	calls   int
}

func (s *stubAgents) ResolveAgentPrincipal(_ context.Context, raw string) (domain.Principal, error) {
	s.calls++
	p, ok := s.byToken[raw]
	if !ok {
		return domain.Principal{}, errors.New("agent credential not found, revoked, or expired")
	}
	return p, nil
}

func agentPrincipal() domain.Principal {
	authority := domain.AgentAuthority{
		Role:        domain.AgentRoleReviewer,
		ProjectID:   "proj-1",
		SessionID:   "agent-orchestrator-59",
		Permissions: domain.AgentRoleCeiling(domain.AgentRoleReviewer),
	}
	return domain.Principal{
		User:       domain.User{ID: "user-owner", Role: domain.UserRoleOwner, Status: domain.UserStatusActive},
		AuthMethod: domain.AuthMethodAgent,
		Agent:      &authority,
	}
}

func humanPrincipal() domain.Principal {
	return domain.Principal{
		User:       domain.User{ID: "user-person", Role: domain.UserRoleOwner, Status: domain.UserStatusActive},
		AuthMethod: domain.AuthMethodOIDC,
		SessionID:  "sess-1",
	}
}

// captured records what the middleware attached, so each case can assert on the
// principal rather than on a status code some later handler would produce.
func captured(t *testing.T, mw func(http.Handler) http.Handler, build func(*http.Request)) (domain.Principal, bool) {
	t.Helper()
	var got domain.Principal
	var ok bool
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = identity.PrincipalFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/agent-orchestrator-59/reviews/submit", nil)
	build(req)
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got, ok
}

// The whole point: a reviewer pane presenting its own credential arrives with an
// identity. Before this it arrived with none and was answered 401 by the one
// route it exists to call.
func TestAgentCredentialResolvesToAnAgentPrincipal(t *testing.T) {
	agents := &stubAgents{byToken: map[string]domain.Principal{"agent-token": agentPrincipal()}}
	cookies := &stubResolver{principal: humanPrincipal()}
	mw := identity.Middleware(cookies, agents, false, nil)

	got, ok := captured(t, mw, func(r *http.Request) {
		r.Header.Set(identity.AgentTokenHeader, "agent-token")
	})
	if !ok || !got.IsAgent() {
		t.Fatalf("agent credential did not resolve: %+v (ok=%v)", got, ok)
	}
	if cookies.calls != 0 {
		t.Fatalf("the browser-session resolver was consulted for an agent request")
	}
}

// An agent credential must never be readable as a browser session, and a browser
// session must never be readable as an agent credential. Each is only ever read
// by the resolver that knows what it is.
func TestAgentAndBrowserCredentialsAreNotInterchangeable(t *testing.T) {
	agents := &stubAgents{byToken: map[string]domain.Principal{"agent-token": agentPrincipal()}}
	cookies := &stubResolver{principal: humanPrincipal()}
	mw := identity.Middleware(cookies, agents, false, nil)

	// An agent token in the cookie is just an unknown cookie value.
	agentInCookie := &stubResolver{err: errors.New("session not found")}
	got, ok := captured(t, identity.Middleware(agentInCookie, agents, false, nil), func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "agent-token"})
	})
	if ok {
		t.Fatalf("an agent token presented as a cookie resolved to %+v", got)
	}
	if agents.calls != 0 {
		t.Fatalf("the agent resolver was consulted for a cookie")
	}

	// A browser session in the agent header is just an unknown agent token.
	got, ok = captured(t, mw, func(r *http.Request) {
		r.Header.Set(identity.AgentTokenHeader, "a-real-browser-session-token")
	})
	if ok {
		t.Fatalf("a browser session presented as an agent token resolved to %+v", got)
	}
}

// THE ESCALATION THIS CLOSES. A request that claims to be an agent and fails to
// prove it must not quietly fall back onto whatever browser session happens to
// be attached -- that would let a stale or revoked agent token act with a
// person's authority over every project in the installation.
func TestAFailedAgentClaimNeverFallsBackToABrowserSession(t *testing.T) {
	agents := &stubAgents{byToken: map[string]domain.Principal{}}
	cookies := &stubResolver{principal: humanPrincipal()}
	mw := identity.Middleware(cookies, agents, false, nil)

	got, ok := captured(t, mw, func(r *http.Request) {
		r.Header.Set(identity.AgentTokenHeader, "revoked-token")
		r.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "a-valid-human-session"})
	})
	if ok {
		t.Fatalf("a failed agent claim borrowed an identity: %+v", got)
	}
	if cookies.calls != 0 {
		t.Fatalf("the cookie was read after an agent claim was made")
	}
}

// On a desktop install the loopback listener is itself the trust boundary, so an
// agent whose credential expired behaves exactly as it did before this mechanism
// existed. Regressing this would break every trusted-local reviewer.
func TestAFailedAgentClaimStillReachesTrustedLocalSynthesis(t *testing.T) {
	agents := &stubAgents{byToken: map[string]domain.Principal{}}
	admin := domain.User{ID: "user-admin", Role: domain.UserRoleOwner, Status: domain.UserStatusActive}
	mw := identity.Middleware(nil, agents, true, func(context.Context) (domain.User, bool) { return admin, true })

	got, ok := captured(t, mw, func(r *http.Request) {
		r.Header.Set(identity.AgentTokenHeader, "expired-token")
	})
	if !ok || got.User.ID != admin.ID {
		t.Fatalf("trusted-local synthesis did not apply after a failed agent claim: %+v (ok=%v)", got, ok)
	}
	if got.AuthMethod != domain.AuthMethodTrustedLocal {
		t.Fatalf("auth method = %q, want trusted_local", got.AuthMethod)
	}
}

// With no agent resolver wired -- every pre-P4-I configuration -- the header is
// inert, and it still does not open the cookie path to whoever sent it.
func TestNoAgentResolverLeavesTheHeaderInert(t *testing.T) {
	cookies := &stubResolver{principal: humanPrincipal()}
	mw := identity.Middleware(cookies, nil, false, nil)

	if _, ok := captured(t, mw, func(r *http.Request) {
		r.Header.Set(identity.AgentTokenHeader, "anything")
		r.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "a-valid-human-session"})
	}); ok {
		t.Fatalf("an agent header resolved with no agent resolver wired")
	}
	// And an ordinary request is completely unaffected.
	got, ok := captured(t, mw, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "a-valid-human-session"})
	})
	if !ok || got.User.ID != "user-person" {
		t.Fatalf("an ordinary browser request stopped resolving: %+v (ok=%v)", got, ok)
	}
}

// An empty header is not a claim. Middleware boxes sometimes add empty headers;
// treating one as "I am an agent" would lock out ordinary requests.
func TestAnEmptyAgentHeaderIsNotAClaim(t *testing.T) {
	agents := &stubAgents{byToken: map[string]domain.Principal{}}
	cookies := &stubResolver{principal: humanPrincipal()}
	mw := identity.Middleware(cookies, agents, false, nil)

	got, ok := captured(t, mw, func(r *http.Request) {
		r.Header.Set(identity.AgentTokenHeader, "   ")
		r.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: "a-valid-human-session"})
	})
	if !ok || got.User.ID != "user-person" {
		t.Fatalf("an empty agent header blocked an ordinary request: %+v (ok=%v)", got, ok)
	}
}
