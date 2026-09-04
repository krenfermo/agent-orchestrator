// Package oidctest is a deterministic, in-process OpenID Provider for tests.
//
// It exists so the entire P4-A suite — protocol verification, service flow,
// and the HTTP surface end to end — runs with no network, no container, and
// no vendor account, while still exercising real RS256 signatures, a real
// JWKS, a real PKCE check and a real authorization-code exchange.
//
// Every deviation a test needs (a wrong issuer, a foreign audience, an
// expired or unsigned token, a mismatched nonce, a declined authorization) is
// a field on Provider rather than a bespoke fake, so a test states the ONE
// thing it is varying.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Claims are the identity claims the provider will assert for a code.
type Claims struct {
	Subject           string
	Email             string
	EmailVerified     bool
	Name              string
	PreferredUsername string
	// Extra carries any additional claim (groups, hd, …).
	Extra map[string]any
	// EmailOnlyInUserInfo puts email/name behind the userinfo endpoint
	// instead of in the ID token, which is how several real providers behave.
	EmailOnlyInUserInfo bool
}

// Provider is a running mock OpenID Provider.
type Provider struct {
	Server   *httptest.Server
	ClientID string
	// ClientSecret, when set, is required at the token endpoint.
	ClientSecret string

	// --- deviation knobs, all zero-valued for a conformant provider ---

	// IssuerOverride puts a different `iss` in the ID token than the one the
	// discovery document declares.
	IssuerOverride string
	// DiscoveryIssuerOverride makes the discovery document declare an issuer
	// other than this server's own URL.
	DiscoveryIssuerOverride string
	// AudienceOverride replaces the ID token's `aud`.
	AudienceOverride []string
	// AuthorizedPartyOverride sets `azp`.
	AuthorizedPartyOverride string
	// ExpiryOffset shifts the ID token's `exp` relative to now. Negative
	// values mint an already-expired token.
	ExpiryOffset time.Duration
	// IssuedAtOffset shifts `iat`.
	IssuedAtOffset time.Duration
	// NonceOverride replaces the nonce echoed into the ID token. A pointer so
	// a test can distinguish "leave it alone" from "omit it entirely".
	NonceOverride *string
	// SignWithForeignKey signs with a key that is not published in the JWKS.
	SignWithForeignKey bool
	// AlgNone emits an unsigned ("alg":"none") token.
	AlgNone bool
	// TokenEndpointStatus, when non-zero, makes the token endpoint answer
	// that status with an OAuth error body instead of tokens.
	TokenEndpointStatus int
	// TokenEndpointError is the OAuth error code returned with the above.
	TokenEndpointError string
	// OmitIDToken returns a token response with no id_token.
	OmitIDToken bool
	// OmitEndSession drops end_session_endpoint from discovery.
	OmitEndSession bool
	// RequirePKCE fails the exchange when the verifier does not match the
	// challenge. On by default (set via New); tests that want to prove PKCE
	// is actually sent flip it off to show the difference.
	RequirePKCE bool
	// Now is the provider's clock.
	Now func() time.Time

	mu       sync.Mutex
	signKey  *rsa.PrivateKey
	otherKey *rsa.PrivateKey
	codes    map[string]*pendingCode
}

type pendingCode struct {
	claims        Claims
	nonce         string
	codeChallenge string
	redirectURI   string
	used          bool
}

// New starts a mock provider. The caller closes it via t.Cleanup, which New
// registers.
func New(t *testing.T) *Provider {
	t.Helper()
	signKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate foreign key: %v", err)
	}
	p := &Provider{
		ClientID:    "ao-test-client",
		RequirePKCE: true,
		Now:         time.Now,
		signKey:     signKey,
		otherKey:    otherKey,
		codes:       map[string]*pendingCode{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.serveDiscovery)
	mux.HandleFunc("/jwks", p.serveJWKS)
	mux.HandleFunc("/token", p.serveToken)
	mux.HandleFunc("/userinfo", p.serveUserInfo)
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, _ *http.Request) {
		// Never actually visited by a test: the harness drives the redirect
		// itself via Authorize. Present so a misrouted request is obvious.
		http.Error(w, "the test harness drives authorization directly", http.StatusNotImplemented)
	})
	mux.HandleFunc("/logout", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Server.Close)
	return p
}

// Issuer is the provider's issuer identifier.
func (p *Provider) Issuer() string { return p.Server.URL }

func (p *Provider) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// Authorize simulates the person completing sign-in at the provider. It takes
// the authorization URL AO produced, validates the request parameters a real
// provider would, and returns the callback query string the browser would be
// redirected with.
func (p *Provider) Authorize(t *testing.T, authorizationURL string, claims Claims) url.Values {
	t.Helper()
	u, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("parse authorization url: %v", err)
	}
	q := u.Query()
	if got := q.Get("client_id"); got != p.ClientID {
		t.Fatalf("authorization request client_id = %q, want %q", got, p.ClientID)
	}
	if got := q.Get("response_type"); got != "code" {
		t.Fatalf("authorization request response_type = %q, want code", got)
	}
	state := q.Get("state")
	if state == "" {
		t.Fatal("authorization request carried no state")
	}
	nonce := q.Get("nonce")
	if nonce == "" {
		t.Fatal("authorization request carried no nonce")
	}
	code := fmt.Sprintf("code-%d-%s", p.now().UnixNano(), state[:8])

	p.mu.Lock()
	p.codes[code] = &pendingCode{
		claims:        claims,
		nonce:         nonce,
		codeChallenge: q.Get("code_challenge"),
		redirectURI:   q.Get("redirect_uri"),
	}
	p.mu.Unlock()

	return url.Values{"code": {code}, "state": {state}}
}

// Decline simulates the provider refusing the sign-in.
func (p *Provider) Decline(t *testing.T, authorizationURL, oauthError string) url.Values {
	t.Helper()
	u, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("parse authorization url: %v", err)
	}
	return url.Values{
		"error":             {oauthError},
		"error_description": {"the resource owner denied the request"},
		"state":             {u.Query().Get("state")},
	}
}

// AuthorizationCodeFor mints a code bound to a nonce and PKCE challenge
// directly, for the protocol-level tests that never build an authorization
// URL.
func (p *Provider) AuthorizationCodeFor(nonce, codeChallenge string, claims Claims) string {
	code := fmt.Sprintf("direct-%d", p.now().UnixNano())
	p.mu.Lock()
	p.codes[code] = &pendingCode{claims: claims, nonce: nonce, codeChallenge: codeChallenge}
	p.mu.Unlock()
	return code
}

func (p *Provider) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	issuer := p.Server.URL
	if p.DiscoveryIssuerOverride != "" {
		issuer = p.DiscoveryIssuerOverride
	}
	doc := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                p.Server.URL + "/authorize",
		"token_endpoint":                        p.Server.URL + "/token",
		"userinfo_endpoint":                     p.Server.URL + "/userinfo",
		"jwks_uri":                              p.Server.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"code_challenge_methods_supported":      []string{"S256"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
	}
	if !p.OmitEndSession {
		doc["end_session_endpoint"] = p.Server.URL + "/logout"
	}
	writeJSON(w, doc)
}

func (p *Provider) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := p.signKey.PublicKey
	writeJSON(w, map[string]any{"keys": []any{map[string]any{
		"kty": "RSA",
		"kid": "test-key-1",
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (p *Provider) serveToken(w http.ResponseWriter, r *http.Request) {
	if p.TokenEndpointStatus != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(p.TokenEndpointStatus)
		errCode := p.TokenEndpointError
		if errCode == "" {
			errCode = "invalid_grant"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": errCode})
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if p.ClientSecret != "" && !p.clientAuthenticated(r) {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	code := r.PostFormValue("code")

	p.mu.Lock()
	pending, ok := p.codes[code]
	if ok && pending.used {
		ok = false
	}
	if ok {
		pending.used = true
	}
	p.mu.Unlock()
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if p.RequirePKCE && pending.codeChallenge != "" {
		verifier := r.PostFormValue("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != pending.codeChallenge {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
	}

	body := map[string]any{
		"access_token": "access-" + code,
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if !p.OmitIDToken {
		body["id_token"] = p.mintIDToken(pending)
	}
	writeJSON(w, body)
}

func (p *Provider) clientAuthenticated(r *http.Request) bool {
	if id, secret, ok := r.BasicAuth(); ok {
		gotID, _ := url.QueryUnescape(id)
		gotSecret, _ := url.QueryUnescape(secret)
		return gotID == p.ClientID && gotSecret == p.ClientSecret
	}
	return r.PostFormValue("client_id") == p.ClientID && r.PostFormValue("client_secret") == p.ClientSecret
}

func (p *Provider) serveUserInfo(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	code := strings.TrimPrefix(token, "access-")
	p.mu.Lock()
	pending, ok := p.codes[code]
	p.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	out := map[string]any{"sub": pending.claims.Subject}
	if pending.claims.Email != "" {
		out["email"] = pending.claims.Email
		out["email_verified"] = pending.claims.EmailVerified
	}
	if pending.claims.Name != "" {
		out["name"] = pending.claims.Name
	}
	writeJSON(w, out)
}

// mintIDToken builds and signs the ID token, applying every deviation knob.
func (p *Provider) mintIDToken(pending *pendingCode) string {
	now := p.now()
	issuer := p.Server.URL
	if p.IssuerOverride != "" {
		issuer = p.IssuerOverride
	}
	aud := any(p.ClientID)
	if len(p.AudienceOverride) > 0 {
		aud = p.AudienceOverride
	}
	nonce := pending.nonce
	if p.NonceOverride != nil {
		nonce = *p.NonceOverride
	}
	exp := now.Add(5 * time.Minute)
	if p.ExpiryOffset != 0 {
		exp = now.Add(p.ExpiryOffset)
	}
	claims := map[string]any{
		"iss":   issuer,
		"sub":   pending.claims.Subject,
		"aud":   aud,
		"exp":   exp.Unix(),
		"iat":   now.Add(p.IssuedAtOffset).Unix(),
		"nonce": nonce,
	}
	if p.AuthorizedPartyOverride != "" {
		claims["azp"] = p.AuthorizedPartyOverride
	}
	if !pending.claims.EmailOnlyInUserInfo {
		if pending.claims.Email != "" {
			claims["email"] = pending.claims.Email
			claims["email_verified"] = pending.claims.EmailVerified
		}
		if pending.claims.Name != "" {
			claims["name"] = pending.claims.Name
		}
		if pending.claims.PreferredUsername != "" {
			claims["preferred_username"] = pending.claims.PreferredUsername
		}
	}
	for k, v := range pending.claims.Extra {
		claims[k] = v
	}

	if p.AlgNone {
		header := b64JSON(map[string]any{"alg": "none", "typ": "JWT"})
		return header + "." + b64JSON(claims) + "."
	}
	header := b64JSON(map[string]any{"alg": "RS256", "kid": "test-key-1", "typ": "JWT"})
	signingInput := header + "." + b64JSON(claims)
	key := p.signKey
	if p.SignWithForeignKey {
		key = p.otherKey
	}
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func b64JSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
}
