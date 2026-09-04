package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/oidc"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc/oidctest"
)

func newClient(t *testing.T, p *oidctest.Provider, mutate func(*oidc.Config)) *oidc.Client {
	t.Helper()
	cfg := oidc.Config{
		Issuer:      p.Issuer(),
		ClientID:    p.ClientID,
		RedirectURL: "http://127.0.0.1:3001/api/v1/auth/oidc/callback",
		Scopes:      oidc.DefaultScopes(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return oidc.NewClient(cfg, p.Server.Client(), nil)
}

// exchange runs one full authorization-code exchange against the mock
// provider: mint verifier/nonce, hand the provider a code bound to them, then
// exchange it.
func exchange(t *testing.T, c *oidc.Client, p *oidctest.Provider, claims oidctest.Claims) (*oidc.IDTokenClaims, error) {
	t.Helper()
	verifier, err := oidc.NewCodeVerifier()
	if err != nil {
		t.Fatalf("code verifier: %v", err)
	}
	nonce, err := oidc.NewNonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	code := p.AuthorizationCodeFor(nonce, oidc.CodeChallengeS256(verifier), claims)
	return c.Exchange(context.Background(), code, verifier, nonce)
}

func TestDiscoveryReadsProviderMetadata(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)

	meta, err := c.Metadata(context.Background())
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if meta.Issuer != p.Issuer() {
		t.Errorf("issuer = %q, want %q", meta.Issuer, p.Issuer())
	}
	if meta.TokenEndpoint == "" || meta.JWKSURI == "" || meta.AuthorizationEndpoint == "" {
		t.Errorf("discovery document missing endpoints: %+v", meta)
	}
	if !meta.SupportsPKCES256() {
		t.Error("provider advertising S256 reported as not supporting PKCE")
	}
}

// A discovery document whose own `iss` differs from the configured issuer is
// the substituted-provider attack: every later `iss` check would compare
// against the attacker's value and pass.
func TestDiscoveryRejectsIssuerMismatch(t *testing.T) {
	p := oidctest.New(t)
	p.DiscoveryIssuerOverride = "https://attacker.example"
	c := newClient(t, p, nil)

	_, err := c.Metadata(context.Background())
	if !errors.Is(err, oidc.ErrIssuerMismatch) {
		t.Fatalf("err = %v, want ErrIssuerMismatch", err)
	}
}

func TestDiscoveryUnreachableProvider(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)
	p.Server.Close()

	_, err := c.Metadata(context.Background())
	if !errors.Is(err, oidc.ErrProviderUnreachable) {
		t.Fatalf("err = %v, want ErrProviderUnreachable", err)
	}
}

func TestAuthorizationURLCarriesStateNonceAndPKCE(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)

	verifier, _ := oidc.NewCodeVerifier()
	got, err := c.AuthorizationURL(context.Background(), "the-state", "the-nonce", oidc.CodeChallengeS256(verifier))
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}
	for _, want := range []string{
		"response_type=code",
		"state=the-state",
		"nonce=the-nonce",
		"code_challenge_method=S256",
		"code_challenge=" + oidc.CodeChallengeS256(verifier),
		"scope=openid+profile+email",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("authorization url %q missing %q", got, want)
		}
	}
	if strings.Contains(got, verifier) {
		t.Error("authorization url leaked the PKCE code verifier")
	}
}

func TestExchangeVerifiesAndReturnsClaims(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)

	claims, err := exchange(t, c, p, oidctest.Claims{
		Subject: "sub-123", Email: "Ada@Example.COM", EmailVerified: true, Name: "Ada Lovelace",
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if claims.Subject != "sub-123" {
		t.Errorf("sub = %q", claims.Subject)
	}
	if claims.Issuer != p.Issuer() {
		t.Errorf("iss = %q, want %q", claims.Issuer, p.Issuer())
	}
	if claims.Email != "ada@example.com" {
		t.Errorf("email = %q, want lowercased ada@example.com", claims.Email)
	}
	if !claims.EmailVerified {
		t.Error("email_verified lost")
	}
	if claims.DisplayName() != "Ada Lovelace" {
		t.Errorf("display name = %q", claims.DisplayName())
	}
	if claims.EmailDomain() != "example.com" {
		t.Errorf("email domain = %q", claims.EmailDomain())
	}
}

// PKCE is not decoration: a token endpoint that checks the verifier must
// reject an exchange that presents the wrong one.
func TestExchangeFailsOnWrongCodeVerifier(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)

	nonce, _ := oidc.NewNonce()
	good, _ := oidc.NewCodeVerifier()
	other, _ := oidc.NewCodeVerifier()
	code := p.AuthorizationCodeFor(nonce, oidc.CodeChallengeS256(good), oidctest.Claims{Subject: "s"})

	_, err := c.Exchange(context.Background(), code, other, nonce)
	if !errors.Is(err, oidc.ErrTokenExchange) {
		t.Fatalf("err = %v, want ErrTokenExchange", err)
	}
}

// An authorization code is single use. A replayed one must not produce a
// second identity.
func TestExchangeRejectsReplayedCode(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)

	nonce, _ := oidc.NewNonce()
	verifier, _ := oidc.NewCodeVerifier()
	code := p.AuthorizationCodeFor(nonce, oidc.CodeChallengeS256(verifier), oidctest.Claims{Subject: "s"})

	if _, err := c.Exchange(context.Background(), code, verifier, nonce); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	if _, err := c.Exchange(context.Background(), code, verifier, nonce); !errors.Is(err, oidc.ErrTokenExchange) {
		t.Fatalf("replayed exchange err = %v, want ErrTokenExchange", err)
	}
}

func TestVerifyRejections(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*oidctest.Provider)
		want    error
	}{
		{
			name:    "wrong issuer in the id token",
			arrange: func(p *oidctest.Provider) { p.IssuerOverride = "https://attacker.example" },
			want:    oidc.ErrIssuerMismatch,
		},
		{
			name:    "token issued for another client",
			arrange: func(p *oidctest.Provider) { p.AudienceOverride = []string{"some-other-client"} },
			want:    oidc.ErrAudienceMismatch,
		},
		{
			name: "multi-audience token without a matching azp",
			arrange: func(p *oidctest.Provider) {
				p.AudienceOverride = []string{p.ClientID, "another-rp"}
				p.AuthorizedPartyOverride = "another-rp"
			},
			want: oidc.ErrAudienceMismatch,
		},
		{
			name:    "already expired",
			arrange: func(p *oidctest.Provider) { p.ExpiryOffset = -30 * time.Minute },
			want:    oidc.ErrTokenExpired,
		},
		{
			name: "nonce does not match this login",
			arrange: func(p *oidctest.Provider) {
				other := "a-different-nonce"
				p.NonceOverride = &other
			},
			want: oidc.ErrNonceMismatch,
		},
		{
			name: "nonce omitted entirely",
			arrange: func(p *oidctest.Provider) {
				empty := ""
				p.NonceOverride = &empty
			},
			want: oidc.ErrNonceMismatch,
		},
		{
			name:    "signed with a key that is not published",
			arrange: func(p *oidctest.Provider) { p.SignWithForeignKey = true },
			want:    oidc.ErrInvalidToken,
		},
		{
			name:    "unsigned alg=none token",
			arrange: func(p *oidctest.Provider) { p.AlgNone = true },
			want:    oidc.ErrInvalidToken,
		},
		{
			name:    "token response carried no id_token",
			arrange: func(p *oidctest.Provider) { p.OmitIDToken = true },
			want:    oidc.ErrTokenExchange,
		},
		{
			name: "token endpoint declines",
			arrange: func(p *oidctest.Provider) {
				p.TokenEndpointStatus = http.StatusBadRequest
				p.TokenEndpointError = "invalid_grant"
			},
			want: oidc.ErrTokenExchange,
		},
		{
			name:    "id token stale enough to be a replay",
			arrange: func(p *oidctest.Provider) { p.IssuedAtOffset = -2 * time.Hour; p.ExpiryOffset = 8 * time.Hour },
			want:    oidc.ErrInvalidToken,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := oidctest.New(t)
			tc.arrange(p)
			c := newClient(t, p, nil)

			_, err := exchange(t, c, p, oidctest.Claims{Subject: "sub-1", Email: "a@example.com"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A multi-audience token IS acceptable when azp names this client — the spec
// permits it, and rejecting it outright would break real deployments.
func TestVerifyAcceptsMultiAudienceWithMatchingAzp(t *testing.T) {
	p := oidctest.New(t)
	p.AudienceOverride = []string{p.ClientID, "another-rp"}
	p.AuthorizedPartyOverride = p.ClientID
	c := newClient(t, p, nil)

	if _, err := exchange(t, c, p, oidctest.Claims{Subject: "s", Email: "a@example.com"}); err != nil {
		t.Fatalf("exchange: %v", err)
	}
}

// Several providers only release email/name from userinfo. A login must still
// carry them.
func TestExchangeFallsBackToUserInfoForDisplayClaims(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)

	claims, err := exchange(t, c, p, oidctest.Claims{
		Subject: "sub-9", Email: "grace@example.com", EmailVerified: true,
		Name: "Grace Hopper", EmailOnlyInUserInfo: true,
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if claims.Email != "grace@example.com" || !claims.EmailVerified {
		t.Errorf("userinfo email not merged: %+v", claims)
	}
	if claims.DisplayName() != "Grace Hopper" {
		t.Errorf("display name = %q", claims.DisplayName())
	}
}

func TestConfidentialClientAuthenticatesAtTokenEndpoint(t *testing.T) {
	p := oidctest.New(t)
	p.ClientSecret = "s3cret-value"
	c := newClient(t, p, func(cfg *oidc.Config) { cfg.ClientSecret = "s3cret-value" })

	if _, err := exchange(t, c, p, oidctest.Claims{Subject: "s", Email: "a@example.com"}); err != nil {
		t.Fatalf("exchange with client secret: %v", err)
	}

	wrong := newClient(t, p, func(cfg *oidc.Config) { cfg.ClientSecret = "not-the-secret" })
	if _, err := exchange(t, wrong, p, oidctest.Claims{Subject: "s2", Email: "b@example.com"}); !errors.Is(err, oidc.ErrTokenExchange) {
		t.Fatalf("err = %v, want ErrTokenExchange for a wrong client secret", err)
	}
}

func TestEndSessionURL(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)

	got, err := c.EndSessionURL(context.Background(), "http://127.0.0.1:3001/")
	if err != nil {
		t.Fatalf("end session url: %v", err)
	}
	if !strings.HasPrefix(got, p.Issuer()+"/logout") || !strings.Contains(got, "post_logout_redirect_uri=") {
		t.Errorf("end session url = %q", got)
	}

	// A provider that advertises none yields "", and callers must not claim a
	// provider logout happened.
	p2 := oidctest.New(t)
	p2.OmitEndSession = true
	c2 := newClient(t, p2, nil)
	got2, err := c2.EndSessionURL(context.Background(), "")
	if err != nil {
		t.Fatalf("end session url: %v", err)
	}
	if got2 != "" {
		t.Errorf("end session url = %q, want empty for a provider that advertises none", got2)
	}
}

func TestClaimConstraintMatchesStringAndArrayClaims(t *testing.T) {
	p := oidctest.New(t)
	c := newClient(t, p, nil)

	claims, err := exchange(t, c, p, oidctest.Claims{
		Subject: "s", Email: "a@example.com",
		Extra: map[string]any{"groups": []string{"eng", "ao-users"}, "hd": "example.com"},
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !claims.HasClaimValue("groups", "ao-users") {
		t.Error("array claim membership not detected")
	}
	if claims.HasClaimValue("groups", "admins") {
		t.Error("array claim reported a value it does not carry")
	}
	if !claims.HasClaimValue("hd", "example.com") {
		t.Error("string claim equality not detected")
	}
	if claims.HasClaimValue("missing", "x") {
		t.Error("absent claim reported as matching")
	}
}

func TestPKCEChallengeIsS256OfVerifier(t *testing.T) {
	v, err := oidc.NewCodeVerifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Errorf("verifier length %d outside RFC 7636's 43-128", len(v))
	}
	challenge := oidc.CodeChallengeS256(v)
	if challenge == v {
		t.Error("challenge equals verifier — S256 not applied")
	}
	// Deterministic: the token endpoint recomputes it from the verifier AO
	// sends, so a challenge that varied would fail every exchange.
	other, err := oidc.NewCodeVerifier()
	if err != nil {
		t.Fatalf("second verifier: %v", err)
	}
	if oidc.CodeChallengeS256(v) != challenge {
		t.Error("challenge is not deterministic for the same verifier")
	}
	if oidc.CodeChallengeS256(other) == challenge {
		t.Error("two different verifiers produced the same challenge")
	}
}
