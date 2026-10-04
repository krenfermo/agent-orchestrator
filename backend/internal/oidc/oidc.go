// Package oidc implements the OpenID Connect pieces P4-A needs — provider
// discovery, JWKS-backed ID token verification, PKCE, and the Authorization
// Code token exchange — against the standards rather than against any one
// identity vendor. Nothing here knows what an AO user is: it turns a
// provider's answer into verified claims and stops. Mapping claims onto an
// AO account is service/ssosvc's job, and issuing an AO session is authsvc's.
//
// It is deliberately dependency-free (standard library only). The alternative
// was three new modules in go.mod for ~600 lines of well-specified protocol,
// on a branch running in parallel with other work that shares that file.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// discoveryPath is the well-known suffix appended to the issuer, per OpenID
// Connect Discovery 1.0 §4. The issuer may itself carry a path, so this is
// appended to it rather than replacing it.
const discoveryPath = "/.well-known/openid-configuration"

// metadataTTL and jwksTTL bound how long a discovery document / key set is
// reused. Short enough that a provider's key rotation is picked up without a
// daemon restart, long enough that a login is not two extra round trips.
const (
	metadataTTL = 10 * time.Minute
	jwksTTL     = 10 * time.Minute
)

// maxResponseBytes caps every provider response AO reads. A provider is a
// trusted-ish but remote party; it does not get to decide how much memory the
// daemon spends on a login.
const maxResponseBytes = 1 << 20

// defaultHTTPTimeout bounds each individual call to the provider.
const defaultHTTPTimeout = 15 * time.Second

// ProviderMetadata is the subset of the discovery document AO uses. Unknown
// fields are ignored, as the spec requires.
type ProviderMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	UserInfoEndpoint                  string   `json:"userinfo_endpoint"`
	JWKSURI                           string   `json:"jwks_uri"`
	EndSessionEndpoint                string   `json:"end_session_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	IDTokenSigningAlgValuesSupported  []string `json:"id_token_signing_alg_values_supported"`
}

// SupportsPKCES256 reports whether the provider advertises S256. A provider
// that advertises nothing is assumed to support it: PKCE is mandatory for
// public clients in OAuth 2.1 and sending it to a provider that ignores it is
// harmless, whereas omitting it where it is needed is not.
func (m ProviderMetadata) SupportsPKCES256() bool {
	if len(m.CodeChallengeMethodsSupported) == 0 {
		return true
	}
	for _, v := range m.CodeChallengeMethodsSupported {
		if v == "S256" {
			return true
		}
	}
	return false
}

func (m ProviderMetadata) supportsAuthMethod(name string) bool {
	for _, v := range m.TokenEndpointAuthMethodsSupported {
		if v == name {
			return true
		}
	}
	return false
}

// Config is the operator-supplied provider configuration. ClientSecret is
// carried here and nowhere else that faces the API: no response shape in
// httpd/controllers ever renders it.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
}

// Client talks to one configured provider. It is safe for concurrent use; the
// discovery and JWKS caches are guarded.
type Client struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	mu            sync.Mutex
	meta          *ProviderMetadata
	metaFetchedAt time.Time
	keys          *keySet
	keysFetchedAt time.Time
}

// NewClient builds a Client. httpClient and now are injection points for
// tests; both default when nil.
func NewClient(cfg Config, httpClient *http.Client, now func() time.Time) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if now == nil {
		now = time.Now
	}
	return &Client{cfg: cfg, http: httpClient, now: now}
}

// Config returns the client's configuration. The caller sees ClientSecret, so
// this stays internal to the backend; it exists for the service layer's
// end-session URL construction and audit metadata, not for any DTO.
func (c *Client) Config() Config { return c.cfg }

// Metadata returns the provider's discovery document, fetching it if the
// cached copy is missing or stale.
//
// The document's own `issuer` MUST equal the configured issuer (Discovery 1.0
// §4.3). Skipping that check is how a redirected or hijacked discovery URL
// silently substitutes a different identity provider: every downstream `iss`
// check would then compare against the attacker's issuer and pass.
func (c *Client) Metadata(ctx context.Context) (ProviderMetadata, error) {
	c.mu.Lock()
	if c.meta != nil && c.now().Sub(c.metaFetchedAt) < metadataTTL {
		m := *c.meta
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()

	endpoint := strings.TrimSuffix(c.cfg.Issuer, "/") + discoveryPath
	var meta ProviderMetadata
	if err := c.getJSON(ctx, endpoint, &meta); err != nil {
		return ProviderMetadata{}, fmt.Errorf("%w: %w", ErrProviderUnreachable, err)
	}
	if meta.Issuer != c.cfg.Issuer {
		return ProviderMetadata{}, fmt.Errorf("%w: discovery document declares issuer %q, configured issuer is %q",
			ErrIssuerMismatch, meta.Issuer, c.cfg.Issuer)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" || meta.JWKSURI == "" {
		return ProviderMetadata{}, fmt.Errorf("%w: discovery document is missing authorization_endpoint, token_endpoint or jwks_uri", ErrProviderUnreachable)
	}

	c.mu.Lock()
	c.meta = &meta
	c.metaFetchedAt = c.now()
	c.mu.Unlock()
	return meta, nil
}

// AuthorizationURL builds the authorization request. state, nonce and the
// PKCE challenge are supplied by the caller, which owns their storage and
// their single-use lifetime.
func (c *Client) AuthorizationURL(ctx context.Context, state, nonce, codeChallenge string) (string, error) {
	meta, err := c.Metadata(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("%w: unparseable authorization_endpoint: %w", ErrProviderUnreachable, err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("scope", strings.Join(c.scopes(), " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	if codeChallenge != "" && meta.SupportsPKCES256() {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// EndSessionURL returns the provider's RP-initiated logout URL, or "" when the
// provider advertises none. Callers must not describe a missing end-session
// endpoint as a completed global sign-out.
func (c *Client) EndSessionURL(ctx context.Context, postLogoutRedirect string) (string, error) {
	meta, err := c.Metadata(ctx)
	if err != nil {
		return "", err
	}
	if meta.EndSessionEndpoint == "" {
		return "", nil
	}
	u, err := url.Parse(meta.EndSessionEndpoint)
	if err != nil {
		return "", fmt.Errorf("%w: unparseable end_session_endpoint: %w", ErrProviderUnreachable, err)
	}
	q := u.Query()
	q.Set("client_id", c.cfg.ClientID)
	if postLogoutRedirect != "" {
		q.Set("post_logout_redirect_uri", postLogoutRedirect)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) scopes() []string {
	if len(c.cfg.Scopes) > 0 {
		return c.cfg.Scopes
	}
	return DefaultScopes()
}

// DefaultScopes is the scope set AO requests when the operator configures
// none: the mandatory `openid` plus the two claims sets AO actually renders.
func DefaultScopes() []string { return []string{"openid", "profile", "email"} }

// tokenResponse is the token endpoint's answer. AO reads only id_token: it
// never re-presents the provider's access token as an AO session, and it does
// not store the refresh token, because it has nothing to refresh (there is no
// long-lived provider API call AO makes on the user's behalf in P4-A).
type tokenResponse struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// Exchange trades an authorization code for the provider's tokens and returns
// the VERIFIED ID token claims. Every check the callback depends on happens
// here: signature against the provider's JWKS, issuer, audience, expiry, and
// the nonce bound to this specific login.
//
// The authorization code, the raw ID token and the access token are local
// variables only; none is returned, stored or logged.
func (c *Client) Exchange(ctx context.Context, code, codeVerifier, expectedNonce string) (*IDTokenClaims, error) {
	meta, err := c.Metadata(ctx)
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURL)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(""))
	if err != nil {
		return nil, fmt.Errorf("%w: build token request: %w", ErrProviderUnreachable, err)
	}
	// client_secret_basic is the OIDC default and the one every provider must
	// accept; client_secret_post is used only when the provider advertises it
	// and not basic. A client with no secret is a public client: PKCE alone
	// authenticates the exchange.
	switch {
	case c.cfg.ClientSecret == "":
		form.Set("client_id", c.cfg.ClientID)
	case meta.supportsAuthMethod("client_secret_post") && !meta.supportsAuthMethod("client_secret_basic"):
		form.Set("client_id", c.cfg.ClientID)
		form.Set("client_secret", c.cfg.ClientSecret)
	default:
		req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	}
	body := form.Encode()
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: token endpoint: %w", ErrProviderUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: read token response: %w", ErrProviderUnreachable, err)
	}

	var tok tokenResponse
	// A provider is free to answer an error with a non-JSON body; the decode
	// failure must not mask the status code, which is the real signal.
	_ = json.Unmarshal(raw, &tok)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// tok.Error is the provider's own OAuth error code ("invalid_grant",
		// …) — safe to surface, and the only useful thing an operator has.
		// The response body at large is not echoed: it can contain the code.
		return nil, fmt.Errorf("%w: token endpoint returned %d %s", ErrTokenExchange, resp.StatusCode, tok.Error)
	}
	if tok.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrTokenExchange, tok.Error)
	}
	if tok.IDToken == "" {
		return nil, fmt.Errorf("%w: token response carried no id_token", ErrTokenExchange)
	}

	claims, err := c.VerifyIDToken(ctx, tok.IDToken, expectedNonce)
	if err != nil {
		return nil, err
	}
	// The provider may put email/name behind userinfo rather than in the ID
	// token. Best-effort: a userinfo failure never fails a login whose ID
	// token already verified — sub is what identity is built on, and sub is
	// always in the ID token.
	if claims.Email == "" && meta.UserInfoEndpoint != "" && tok.AccessToken != "" {
		c.fillFromUserInfo(ctx, meta.UserInfoEndpoint, tok.AccessToken, claims)
	}
	return claims, nil
}

// fillFromUserInfo tops up display claims from the userinfo endpoint. It
// enforces the one hard rule of userinfo (Core 1.0 §5.3.2): a userinfo
// response whose `sub` differs from the ID token's is discarded outright.
func (c *Client) fillFromUserInfo(ctx context.Context, endpoint, accessToken string, claims *IDTokenClaims) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return
	}
	var info rawClaims
	if err := json.Unmarshal(raw, &info); err != nil {
		return
	}
	if info.Subject != claims.Subject {
		return
	}
	claims.mergeDisplayClaims(info)
}

func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s returned %d", endpoint, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// Sentinel errors. Callers map these onto user-facing messages; none of them
// carries a token, a code or a secret in its text.
var (
	// ErrProviderUnreachable is a transport/discovery failure — the provider
	// could not be reached or answered something unusable.
	ErrProviderUnreachable = errors.New("oidc: provider unreachable")
	// ErrIssuerMismatch is a discovery document or ID token whose issuer is
	// not the configured one.
	ErrIssuerMismatch = errors.New("oidc: issuer mismatch")
	// ErrTokenExchange is a rejected or malformed authorization-code exchange.
	ErrTokenExchange = errors.New("oidc: token exchange failed")
	// ErrInvalidToken is an ID token that failed structural, signature or
	// claim validation.
	ErrInvalidToken = errors.New("oidc: invalid id token")
	// ErrAudienceMismatch is an ID token not issued for this client.
	ErrAudienceMismatch = errors.New("oidc: audience mismatch")
	// ErrTokenExpired is an ID token past its exp.
	ErrTokenExpired = errors.New("oidc: id token expired")
	// ErrNonceMismatch is an ID token whose nonce does not bind it to this
	// login request.
	ErrNonceMismatch = errors.New("oidc: nonce mismatch")
)
