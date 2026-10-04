package ssosvc_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc/oidctest"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/ssosvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// issuerAdapter is the daemon's federatedSessionIssuer, restated here so the
// service test exercises the same translation production uses.
type issuerAdapter struct{ mgr authsvc.Manager }

func (a issuerAdapter) CreateFederatedUser(ctx context.Context, in ssosvc.FederatedUserInput) (domain.User, error) {
	return a.mgr.CreateFederatedUser(ctx, authsvc.FederatedUserInput{
		DisplayName: in.DisplayName, Email: in.Email, Username: in.Username, PreferOwner: in.PreferOwner,
	})
}

func (a issuerAdapter) CreateSessionAs(ctx context.Context, userID domain.UserID, method domain.AuthMethod, issuer, subject string) (string, domain.AuthSession, error) {
	return a.mgr.CreateSessionAs(ctx, userID, method, issuer, subject)
}

type harness struct {
	provider *oidctest.Provider
	store    *store.Store
	auth     authsvc.Manager
	svc      *ssosvc.Service
	clock    *clock
	cfg      config.OIDCConfig
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newHarness(t *testing.T, mutate func(*config.OIDCConfig)) *harness {
	t.Helper()
	p := oidctest.New(t)
	st := sqlitetest.MustOpen(t)
	clk := &clock{t: time.Now().UTC()}
	authMgr := authsvc.New(st, clk.now)
	cfg := config.OIDCConfig{
		Enabled:           true,
		LinkVerifiedEmail: true,
		Issuer:            p.Issuer(),
		ClientID:          p.ClientID,
		RedirectURL:       "http://127.0.0.1:3001/api/v1/auth/oidc/callback",
		Scopes:            oidc.DefaultScopes(),
		DisplayName:       "Test IdP",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client := oidc.NewClient(oidc.Config{
		Issuer:       cfg.Issuer,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Scopes:       cfg.Scopes,
	}, p.Server.Client(), clk.now)
	return &harness{
		provider: p,
		store:    st,
		auth:     authMgr,
		svc:      ssosvc.New(cfg, client, st, issuerAdapter{authMgr}, clk.now),
		clock:    clk,
		cfg:      cfg,
	}
}

// signIn drives a whole browser login: start, satisfy the provider, complete.
func (h *harness) signIn(t *testing.T, claims oidctest.Claims) (ssosvc.CompleteResult, error) {
	t.Helper()
	start, err := h.svc.Start(context.Background(), ssosvc.StartInput{ClientKind: domain.OIDCClientBrowser})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	cb := h.provider.Authorize(t, start.AuthorizationURL, claims)
	return h.svc.Complete(context.Background(), ssosvc.CallbackInput{
		State: cb.Get("state"), Code: cb.Get("code"),
	})
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *apierr.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a structured apierr.Error", err)
	}
	return e.Code
}

func TestStartRecordsAFlowAndBuildsAnAuthorizationRequest(t *testing.T) {
	h := newHarness(t, nil)

	got, err := h.svc.Start(context.Background(), ssosvc.StartInput{ReturnTo: "/board"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	u, err := url.Parse(got.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse authorization url: %v", err)
	}
	if u.Query().Get("state") != got.FlowID {
		t.Errorf("state %q is not the flow id %q", u.Query().Get("state"), got.FlowID)
	}
	if u.Query().Get("code_challenge_method") != "S256" {
		t.Error("authorization request did not request PKCE S256")
	}

	flow, ok, err := h.store.GetOIDCLoginFlow(context.Background(), got.FlowID)
	if err != nil || !ok {
		t.Fatalf("flow not persisted: ok=%v err=%v", ok, err)
	}
	if flow.Nonce == "" || flow.CodeVerifier == "" {
		t.Error("flow did not persist the nonce/verifier the callback must check against")
	}
	if flow.ReturnTo != "/board" {
		t.Errorf("returnTo = %q, want /board", flow.ReturnTo)
	}
	// The verifier must never appear in what goes to the provider.
	if strings.Contains(got.AuthorizationURL, flow.CodeVerifier) {
		t.Error("authorization url leaked the PKCE verifier")
	}
}

func TestFirstFederatedLoginProvisionsTheInstallationOwner(t *testing.T) {
	h := newHarness(t, nil)

	res, err := h.signIn(t, oidctest.Claims{
		Subject: "sub-1", Email: "ada@example.com", EmailVerified: true, Name: "Ada Lovelace",
	})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if !res.Provisioned {
		t.Error("first login did not report provisioning an account")
	}
	if res.Principal.User.Role != domain.UserRoleOwner {
		t.Errorf("role = %q, want owner for the first identity on an empty installation", res.Principal.User.Role)
	}
	if res.Principal.AuthMethod != domain.AuthMethodOIDC {
		t.Errorf("authMethod = %q", res.Principal.AuthMethod)
	}
	if res.Principal.Issuer != h.provider.Issuer() || res.Principal.Subject != "sub-1" {
		t.Errorf("principal identity = (%q,%q)", res.Principal.Issuer, res.Principal.Subject)
	}
	if res.SessionToken == "" || res.SessionExpiresAt.IsZero() {
		t.Error("browser flow issued no session")
	}

	// The session resolves, and it records how it was authenticated.
	p, err := h.auth.ResolvePrincipal(context.Background(), res.SessionToken)
	if err != nil {
		t.Fatalf("resolve issued session: %v", err)
	}
	if p.AuthMethod != domain.AuthMethodOIDC || p.Issuer != h.provider.Issuer() || p.Subject != "sub-1" {
		t.Errorf("resolved principal = %+v", p)
	}
}

// The canonical key is (issuer, sub). A second login for the same subject must
// reuse the account, and a CHANGED EMAIL must not create a second one.
func TestSecondLoginReusesTheAccountEvenWhenEmailChanged(t *testing.T) {
	h := newHarness(t, nil)

	first, err := h.signIn(t, oidctest.Claims{Subject: "sub-1", Email: "ada@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("first sign in: %v", err)
	}
	second, err := h.signIn(t, oidctest.Claims{Subject: "sub-1", Email: "ada.lovelace@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("second sign in: %v", err)
	}
	if second.Provisioned {
		t.Error("second login for a known (issuer,sub) provisioned another account")
	}
	if second.Principal.User.ID != first.Principal.User.ID {
		t.Errorf("user changed across logins: %q then %q", first.Principal.User.ID, second.Principal.User.ID)
	}
	if second.SessionToken == first.SessionToken {
		t.Error("second login reused the first session token")
	}
}

// A DIFFERENT subject at the same issuer is a different person, even when the
// email claim matches an account — unless the provider verified it.
func TestLinkingRules(t *testing.T) {
	t.Run("verified email links to the existing local account", func(t *testing.T) {
		h := newHarness(t, nil)
		local, err := h.auth.RegisterFirstUser(context.Background(), authsvc.CreateUserInput{
			DisplayName: "Ada", Email: "ada@example.com", Username: "ada@example.com", Password: "supersecret1",
		})
		if err != nil {
			t.Fatalf("seed local account: %v", err)
		}
		res, err := h.signIn(t, oidctest.Claims{Subject: "sub-1", Email: "ada@example.com", EmailVerified: true})
		if err != nil {
			t.Fatalf("sign in: %v", err)
		}
		if res.Principal.User.ID != local.ID {
			t.Errorf("verified email did not link to the existing account")
		}
		if res.Provisioned {
			t.Error("linking reported as provisioning")
		}
	})

	t.Run("linking can be switched off entirely", func(t *testing.T) {
		h := newHarness(t, func(c *config.OIDCConfig) { c.LinkVerifiedEmail = false })
		if _, err := h.auth.RegisterFirstUser(context.Background(), authsvc.CreateUserInput{
			DisplayName: "Ada", Email: "ada@example.com", Username: "ada@example.com", Password: "supersecret1",
		}); err != nil {
			t.Fatalf("seed local account: %v", err)
		}
		_, err := h.signIn(t, oidctest.Claims{Subject: "sub-1", Email: "ada@example.com", EmailVerified: true})
		if got := codeOf(t, err); got != "SSO_LINKING_DISABLED" {
			t.Errorf("code = %q, want SSO_LINKING_DISABLED", got)
		}
	})

	t.Run("unverified email refuses to take over an existing account", func(t *testing.T) {
		h := newHarness(t, nil)
		if _, err := h.auth.RegisterFirstUser(context.Background(), authsvc.CreateUserInput{
			DisplayName: "Ada", Email: "ada@example.com", Username: "ada@example.com", Password: "supersecret1",
		}); err != nil {
			t.Fatalf("seed local account: %v", err)
		}
		_, err := h.signIn(t, oidctest.Claims{Subject: "attacker", Email: "ada@example.com", EmailVerified: false})
		if err == nil {
			t.Fatal("an unverified email claim took over an existing account")
		}
		if got := codeOf(t, err); got != "SSO_EMAIL_NOT_VERIFIED" {
			t.Errorf("code = %q, want SSO_EMAIL_NOT_VERIFIED", got)
		}
	})

	t.Run("a disabled account cannot sign in", func(t *testing.T) {
		h := newHarness(t, nil)
		res, err := h.signIn(t, oidctest.Claims{Subject: "sub-1", Email: "ada@example.com", EmailVerified: true})
		if err != nil {
			t.Fatalf("sign in: %v", err)
		}
		if _, err := h.store.UpdateUserStatus(context.Background(), res.Principal.User.ID, domain.UserStatusDisabled, time.Now().UTC()); err != nil {
			t.Fatalf("disable user: %v", err)
		}
		_, err = h.signIn(t, oidctest.Claims{Subject: "sub-1", Email: "ada@example.com", EmailVerified: true})
		if got := codeOf(t, err); got != "SSO_ACCOUNT_DISABLED" {
			t.Errorf("code = %q, want SSO_ACCOUNT_DISABLED", got)
		}
	})
}

func TestCallbackRejections(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*oidctest.Provider)
		want    string
	}{
		{"wrong issuer", func(p *oidctest.Provider) { p.IssuerOverride = "https://attacker.example" }, "SSO_ISSUER_MISMATCH"},
		{"wrong audience", func(p *oidctest.Provider) { p.AudienceOverride = []string{"other-rp"} }, "SSO_AUDIENCE_MISMATCH"},
		{"expired token", func(p *oidctest.Provider) { p.ExpiryOffset = -time.Hour }, "SSO_TOKEN_EXPIRED"},
		{"bad nonce", func(p *oidctest.Provider) { n := "wrong"; p.NonceOverride = &n }, "SSO_NONCE_MISMATCH"},
		{"unsigned token", func(p *oidctest.Provider) { p.AlgNone = true }, "SSO_INVALID_TOKEN"},
		{"token exchange declined", func(p *oidctest.Provider) { p.TokenEndpointStatus = 400 }, "SSO_TOKEN_EXCHANGE_FAILED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			tc.arrange(h.provider)
			_, err := h.signIn(t, oidctest.Claims{Subject: "s", Email: "a@example.com", EmailVerified: true})
			if err == nil {
				t.Fatal("expected the callback to be rejected")
			}
			if got := codeOf(t, err); got != tc.want {
				t.Errorf("code = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStateHandling(t *testing.T) {
	t.Run("unknown state", func(t *testing.T) {
		h := newHarness(t, nil)
		_, err := h.svc.Complete(context.Background(), ssosvc.CallbackInput{State: "never-issued", Code: "x"})
		if got := codeOf(t, err); got != "SSO_INVALID_STATE" {
			t.Errorf("code = %q, want SSO_INVALID_STATE", got)
		}
	})

	t.Run("missing state", func(t *testing.T) {
		h := newHarness(t, nil)
		_, err := h.svc.Complete(context.Background(), ssosvc.CallbackInput{Code: "x"})
		if got := codeOf(t, err); got != "SSO_MISSING_STATE" {
			t.Errorf("code = %q, want SSO_MISSING_STATE", got)
		}
	})

	t.Run("expired state", func(t *testing.T) {
		h := newHarness(t, nil)
		start, err := h.svc.Start(context.Background(), ssosvc.StartInput{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		cb := h.provider.Authorize(t, start.AuthorizationURL, oidctest.Claims{Subject: "s", Email: "a@example.com"})
		h.clock.t = h.clock.t.Add(ssosvc.FlowTTL + time.Minute)

		_, err = h.svc.Complete(context.Background(), ssosvc.CallbackInput{State: cb.Get("state"), Code: cb.Get("code")})
		if got := codeOf(t, err); got != "SSO_STATE_EXPIRED" {
			t.Errorf("code = %q, want SSO_STATE_EXPIRED", got)
		}
	})

	t.Run("replayed state is refused after a successful login", func(t *testing.T) {
		h := newHarness(t, nil)
		start, err := h.svc.Start(context.Background(), ssosvc.StartInput{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		cb := h.provider.Authorize(t, start.AuthorizationURL, oidctest.Claims{
			Subject: "s", Email: "a@example.com", EmailVerified: true,
		})
		in := ssosvc.CallbackInput{State: cb.Get("state"), Code: cb.Get("code")}
		if _, err := h.svc.Complete(context.Background(), in); err != nil {
			t.Fatalf("first callback: %v", err)
		}
		if _, err := h.svc.Complete(context.Background(), in); codeOf(t, err) != "SSO_INVALID_STATE" {
			t.Errorf("replayed callback err = %v, want SSO_INVALID_STATE", err)
		}
	})

	t.Run("provider declined", func(t *testing.T) {
		h := newHarness(t, nil)
		start, err := h.svc.Start(context.Background(), ssosvc.StartInput{})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		cb := h.provider.Decline(t, start.AuthorizationURL, "access_denied")
		_, err = h.svc.Complete(context.Background(), ssosvc.CallbackInput{
			State:                    cb.Get("state"),
			ProviderError:            cb.Get("error"),
			ProviderErrorDescription: cb.Get("error_description"),
		})
		if got := codeOf(t, err); got != "SSO_PROVIDER_DECLINED" {
			t.Errorf("code = %q, want SSO_PROVIDER_DECLINED", got)
		}
		if !strings.Contains(err.Error(), "access_denied") {
			t.Errorf("message %q does not name the provider's own error code", err.Error())
		}
	})
}

func TestOperatorConstraints(t *testing.T) {
	t.Run("allowed domain admits a matching identity", func(t *testing.T) {
		h := newHarness(t, func(c *config.OIDCConfig) { c.AllowedEmailDomains = []string{"example.com"} })
		if _, err := h.signIn(t, oidctest.Claims{Subject: "s", Email: "ada@Example.com", EmailVerified: true}); err != nil {
			t.Fatalf("sign in: %v", err)
		}
	})

	t.Run("allowed domain refuses another domain", func(t *testing.T) {
		h := newHarness(t, func(c *config.OIDCConfig) { c.AllowedEmailDomains = []string{"example.com"} })
		_, err := h.signIn(t, oidctest.Claims{Subject: "s", Email: "ada@other.example", EmailVerified: true})
		if got := codeOf(t, err); got != "SSO_DOMAIN_NOT_ALLOWED" {
			t.Errorf("code = %q, want SSO_DOMAIN_NOT_ALLOWED", got)
		}
	})

	t.Run("a domain restriction is not satisfied by an unverified address", func(t *testing.T) {
		h := newHarness(t, func(c *config.OIDCConfig) { c.AllowedEmailDomains = []string{"example.com"} })
		_, err := h.signIn(t, oidctest.Claims{Subject: "s", Email: "ada@example.com", EmailVerified: false})
		if got := codeOf(t, err); got != "SSO_EMAIL_NOT_VERIFIED" {
			t.Errorf("code = %q, want SSO_EMAIL_NOT_VERIFIED", got)
		}
	})

	t.Run("required claim gates the login", func(t *testing.T) {
		h := newHarness(t, func(c *config.OIDCConfig) {
			c.RequiredClaimName, c.RequiredClaimValue = "groups", "ao-users"
		})
		if _, err := h.signIn(t, oidctest.Claims{
			Subject: "s1", Email: "in@example.com", EmailVerified: true,
			Extra: map[string]any{"groups": []string{"ao-users"}},
		}); err != nil {
			t.Fatalf("permitted identity refused: %v", err)
		}
		_, err := h.signIn(t, oidctest.Claims{
			Subject: "s2", Email: "out@example.com", EmailVerified: true,
			Extra: map[string]any{"groups": []string{"other"}},
		})
		if got := codeOf(t, err); got != "SSO_CLAIM_NOT_SATISFIED" {
			t.Errorf("code = %q, want SSO_CLAIM_NOT_SATISFIED", got)
		}
	})

	t.Run("a refused identity leaves no account behind", func(t *testing.T) {
		h := newHarness(t, func(c *config.OIDCConfig) { c.AllowedEmailDomains = []string{"example.com"} })
		if _, err := h.signIn(t, oidctest.Claims{Subject: "s", Email: "ada@other.example", EmailVerified: true}); err == nil {
			t.Fatal("expected refusal")
		}
		n, err := h.store.CountUsers(context.Background())
		if err != nil {
			t.Fatalf("count users: %v", err)
		}
		if n != 0 {
			t.Errorf("a refused sign-in provisioned %d user(s)", n)
		}
	})
}

func TestDesktopHandoff(t *testing.T) {
	secret := strings.Repeat("d", 43)

	t.Run("callback mints no session; the supervisor claims it", func(t *testing.T) {
		h := newHarness(t, nil)
		start, err := h.svc.Start(context.Background(), ssosvc.StartInput{
			ClientKind: domain.OIDCClientDesktop, HandoffSecret: secret,
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}

		// Before the person finishes at the provider, a claim is "pending".
		if _, err := h.svc.Claim(context.Background(), start.FlowID, secret); !errors.Is(err, ssosvc.ErrHandoffPending) {
			t.Fatalf("early claim err = %v, want ErrHandoffPending", err)
		}

		cb := h.provider.Authorize(t, start.AuthorizationURL, oidctest.Claims{
			Subject: "sub-desktop", Email: "ada@example.com", EmailVerified: true,
		})
		res, err := h.svc.Complete(context.Background(), ssosvc.CallbackInput{State: cb.Get("state"), Code: cb.Get("code")})
		if err != nil {
			t.Fatalf("callback: %v", err)
		}
		if res.SessionToken != "" {
			t.Error("a desktop callback issued a session to the system browser")
		}
		if res.ClientKind != domain.OIDCClientDesktop {
			t.Errorf("client kind = %q", res.ClientKind)
		}

		// A wrong handoff secret claims nothing, even with the right state.
		if _, err := h.svc.Claim(context.Background(), start.FlowID, strings.Repeat("x", 43)); codeOf(t, err) != "SSO_INVALID_HANDOFF" {
			t.Errorf("wrong-secret claim err = %v, want SSO_INVALID_HANDOFF", err)
		}

		claimed, err := h.svc.Claim(context.Background(), start.FlowID, secret)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed.SessionToken == "" {
			t.Fatal("claim issued no session")
		}
		p, err := h.auth.ResolvePrincipal(context.Background(), claimed.SessionToken)
		if err != nil {
			t.Fatalf("resolve claimed session: %v", err)
		}
		if p.AuthMethod != domain.AuthMethodOIDC || p.Subject != "sub-desktop" {
			t.Errorf("claimed principal = %+v", p)
		}

		// Single use: a second claim gets nothing.
		if _, err := h.svc.Claim(context.Background(), start.FlowID, secret); codeOf(t, err) != "SSO_INVALID_HANDOFF" {
			t.Errorf("second claim err = %v, want SSO_INVALID_HANDOFF", err)
		}
	})

	t.Run("a desktop start requires a real handoff secret", func(t *testing.T) {
		h := newHarness(t, nil)
		_, err := h.svc.Start(context.Background(), ssosvc.StartInput{ClientKind: domain.OIDCClientDesktop, HandoffSecret: "short"})
		if got := codeOf(t, err); got != "SSO_INVALID_START" {
			t.Errorf("code = %q, want SSO_INVALID_START", got)
		}
	})
}

// A login started before a daemon restart must still complete afterwards: the
// flow is a durable row, not an in-memory map.
func TestFlowSurvivesAServiceRestart(t *testing.T) {
	h := newHarness(t, nil)
	start, err := h.svc.Start(context.Background(), ssosvc.StartInput{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	cb := h.provider.Authorize(t, start.AuthorizationURL, oidctest.Claims{
		Subject: "sub-1", Email: "ada@example.com", EmailVerified: true,
	})

	// A brand-new Service over the same store is what a restarted daemon has.
	client := oidc.NewClient(oidc.Config{
		Issuer: h.cfg.Issuer, ClientID: h.cfg.ClientID, RedirectURL: h.cfg.RedirectURL, Scopes: h.cfg.Scopes,
	}, h.provider.Server.Client(), h.clock.now)
	restarted := ssosvc.New(h.cfg, client, h.store, issuerAdapter{h.auth}, h.clock.now)

	res, err := restarted.Complete(context.Background(), ssosvc.CallbackInput{State: cb.Get("state"), Code: cb.Get("code")})
	if err != nil {
		t.Fatalf("callback after restart: %v", err)
	}
	if res.SessionToken == "" {
		t.Error("restarted service issued no session")
	}
}

func TestPurgeExpiredFlows(t *testing.T) {
	h := newHarness(t, nil)
	start, err := h.svc.Start(context.Background(), ssosvc.StartInput{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	h.clock.t = h.clock.t.Add(ssosvc.FlowTTL + time.Hour)
	n, err := h.svc.PurgeExpiredFlows(context.Background())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d flows, want 1", n)
	}
	if _, ok, _ := h.store.GetOIDCLoginFlow(context.Background(), start.FlowID); ok {
		t.Error("expired flow survived the purge")
	}
}

func TestDisabledServiceRefusesEveryFlow(t *testing.T) {
	svc := ssosvc.New(config.OIDCConfig{}, nil, nil, nil, nil)
	if svc.Enabled() {
		t.Fatal("a service with no provider reported itself enabled")
	}
	if _, err := svc.Start(context.Background(), ssosvc.StartInput{}); codeOf(t, err) != "SSO_NOT_CONFIGURED" {
		t.Errorf("start err = %v, want SSO_NOT_CONFIGURED", err)
	}
	if _, err := svc.Complete(context.Background(), ssosvc.CallbackInput{State: "x"}); codeOf(t, err) != "SSO_NOT_CONFIGURED" {
		t.Errorf("complete err = %v, want SSO_NOT_CONFIGURED", err)
	}
	if got := svc.DisplayName(); got != config.DefaultOIDCDisplayName {
		t.Errorf("display name = %q", got)
	}
}

// The post-login destination is the open-redirect boundary. Anything that is
// not a same-origin absolute path must collapse to the default.
func TestSafeReturnTo(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"/", ""},
		{"/board", "/board"},
		{"/projects/1?tab=runs", "/projects/1?tab=runs"},
		{"/board#top", "/board#top"},
		{"https://evil.example/steal", ""},
		{"//evil.example/steal", ""},
		{"http://127.0.0.1:3001/board", ""},
		{"\\\\evil.example", ""},
		{"/\\evil.example", ""},
		{"javascript:alert(1)", ""},
		{"board", ""},
		{"/board\nSet-Cookie: x=y", ""},
	}
	for _, tc := range tests {
		if got := ssosvc.SafeReturnTo(tc.in); got != tc.want {
			t.Errorf("SafeReturnTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := ssosvc.ResolveReturnTo("", "/"); got != "/" {
		t.Errorf("ResolveReturnTo fallback = %q", got)
	}
	if got := ssosvc.ResolveReturnTo("/board", "/"); got != "/board" {
		t.Errorf("ResolveReturnTo = %q", got)
	}
}

// An open-redirect attempt supplied at Start must already be neutralized by
// the time the callback reads it back.
func TestStartStoresOnlyAValidatedReturnTo(t *testing.T) {
	h := newHarness(t, nil)
	start, err := h.svc.Start(context.Background(), ssosvc.StartInput{ReturnTo: "https://evil.example/steal"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	flow, ok, err := h.store.GetOIDCLoginFlow(context.Background(), start.FlowID)
	if err != nil || !ok {
		t.Fatalf("flow: ok=%v err=%v", ok, err)
	}
	if flow.ReturnTo != "" {
		t.Errorf("stored returnTo = %q, want it neutralized", flow.ReturnTo)
	}
}
