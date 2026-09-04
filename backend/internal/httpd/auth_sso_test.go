package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc/oidctest"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/ssosvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// ssoIssuer is the daemon's federatedSessionIssuer adapter.
type ssoIssuer struct{ mgr authsvc.Manager }

func (a ssoIssuer) CreateFederatedUser(ctx context.Context, in ssosvc.FederatedUserInput) (domain.User, error) {
	return a.mgr.CreateFederatedUser(ctx, authsvc.FederatedUserInput{
		DisplayName: in.DisplayName, Email: in.Email, Username: in.Username, PreferOwner: in.PreferOwner,
	})
}

func (a ssoIssuer) CreateSessionAs(ctx context.Context, userID domain.UserID, method domain.AuthMethod, issuer, subject string) (string, domain.AuthSession, error) {
	return a.mgr.CreateSessionAs(ctx, userID, method, issuer, subject)
}

type ssoServer struct {
	srv      *httptest.Server
	provider *oidctest.Provider
	store    *store.Store
	auth     authsvc.Manager
	client   *http.Client
}

// newSSOServer wires a real router over a real store with SSO enabled,
// exactly as internal/daemon does.
func newSSOServer(t *testing.T) *ssoServer {
	t.Helper()
	p := oidctest.New(t)
	st := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })

	oidcCfg := config.OIDCConfig{
		Enabled:           true,
		LinkVerifiedEmail: true,
		Issuer:            p.Issuer(),
		ClientID:          p.ClientID,
		Scopes:            oidc.DefaultScopes(),
		DisplayName:       "Test IdP",
	}
	cfg := config.Config{
		TrustedLocalMode: false,
		AuthMode:         domain.AuthModeOIDC,
		OIDC:             oidcCfg,
	}
	client := oidc.NewClient(oidc.Config{
		Issuer: oidcCfg.Issuer, ClientID: oidcCfg.ClientID, Scopes: oidcCfg.Scopes,
	}, p.Server.Client(), nil)
	sso := ssosvc.New(oidcCfg, client, st, ssoIssuer{authMgr}, nil)

	router := NewRouterWithControl(cfg, discardLogger(), nil, APIDeps{Auth: authMgr, SSO: sso}, ControlDeps{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	jar := &http.Client{
		// The callback's 302 must be observable, not silently followed into a
		// 404 for a SPA route this test server does not serve.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &ssoServer{srv: srv, provider: p, store: st, auth: authMgr, client: jar}
}

func (s *ssoServer) startLogin(t *testing.T, body string) map[string]any {
	t.Helper()
	resp, err := s.client.Post(s.srv.URL+"/api/v1/auth/oidc/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start login status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	return out
}

func (s *ssoServer) callback(t *testing.T, q url.Values) *http.Response {
	t.Helper()
	resp, err := s.client.Get(s.srv.URL + "/api/v1/auth/oidc/callback?" + q.Encode())
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	return resp
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == "ao_session" {
			return c
		}
	}
	return nil
}

// TestSSOBrowserLoginEndToEnd is the realistic smoke test the checkpoint asks
// for, against a local mock provider: login, callback, session, authenticated
// API, logout, unauthenticated again.
func TestSSOBrowserLoginEndToEnd(t *testing.T) {
	s := newSSOServer(t)

	// --- unauthenticated to begin with ---
	resp, err := s.client.Get(s.srv.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me before login = %d, want 401", resp.StatusCode)
	}

	// --- the login screen learns SSO is on, and learns nothing else ---
	provResp, err := s.client.Get(s.srv.URL + "/api/v1/auth/providers")
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	provBody := readAll(t, provResp)
	if !strings.Contains(provBody, `"mode":"oidc"`) || !strings.Contains(provBody, `"displayName":"Test IdP"`) {
		t.Fatalf("providers body = %s", provBody)
	}
	for _, secret := range []string{s.provider.Issuer(), s.provider.ClientID, "clientSecret", "issuer"} {
		if strings.Contains(provBody, secret) {
			t.Errorf("providers response leaked backend configuration (%q): %s", secret, provBody)
		}
	}

	// --- start ---
	start := s.startLogin(t, `{"returnTo":"/board"}`)
	authURL, _ := start["authorizationUrl"].(string)
	if authURL == "" {
		t.Fatal("start returned no authorization url")
	}

	// --- the person signs in at the provider ---
	cb := s.provider.Authorize(t, authURL, oidctest.Claims{
		Subject: "sub-1", Email: "ada@example.com", EmailVerified: true, Name: "Ada Lovelace",
	})

	// --- callback: session cookie + bounded redirect ---
	cbResp := s.callback(t, cb)
	_ = cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", cbResp.StatusCode)
	}
	if got := cbResp.Header.Get("Location"); got != "/board" {
		t.Errorf("callback redirected to %q, want /board", got)
	}
	cookie := sessionCookie(cbResp)
	if cookie == nil {
		t.Fatal("callback set no session cookie")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax for a same-origin deployment", cookie.SameSite)
	}
	if cookie.Value == "" {
		t.Fatal("session cookie carried no value")
	}

	// --- authenticated API ---
	meBody := getWithCookie(t, s.client, s.srv.URL+"/api/v1/auth/me", cookie, http.StatusOK)
	for _, want := range []string{`"status":"authenticated"`, `"authMethod":"oidc"`, `"email":"ada@example.com"`, `"role":"owner"`} {
		if !strings.Contains(meBody, want) {
			t.Errorf("me body missing %s: %s", want, meBody)
		}
	}
	if !strings.Contains(meBody, `"issuer":"`+s.provider.Issuer()+`"`) {
		t.Errorf("me body did not name the issuer: %s", meBody)
	}

	// --- logout invalidates the AO session and OFFERS the provider's own ---
	logoutBody := postWithCookie(t, s.client, s.srv.URL+"/api/v1/auth/logout", cookie, http.StatusOK)
	if !strings.Contains(logoutBody, `"ok":true`) {
		t.Fatalf("logout body = %s", logoutBody)
	}
	var logout struct {
		OK                    bool   `json:"ok"`
		ProviderEndSessionURL string `json:"providerEndSessionUrl"`
	}
	if err := json.Unmarshal([]byte(logoutBody), &logout); err != nil {
		t.Fatalf("decode logout: %v", err)
	}
	if !strings.HasPrefix(logout.ProviderEndSessionURL, s.provider.Issuer()+"/logout") {
		t.Errorf("providerEndSessionUrl = %q", logout.ProviderEndSessionURL)
	}

	// --- unauthenticated again, with the same cookie ---
	getWithCookie(t, s.client, s.srv.URL+"/api/v1/auth/me", cookie, http.StatusUnauthorized)
}

// A provider that advertises no end_session_endpoint must not produce an offer
// AO cannot honor: logout says the AO session ended and nothing more.
func TestSSOLogoutDoesNotClaimProviderLogoutWhenUnsupported(t *testing.T) {
	s := newSSOServer(t)
	s.provider.OmitEndSession = true

	start := s.startLogin(t, `{}`)
	cb := s.provider.Authorize(t, start["authorizationUrl"].(string), oidctest.Claims{
		Subject: "sub-1", Email: "ada@example.com", EmailVerified: true,
	})
	cookie := sessionCookie(s.callback(t, cb))
	if cookie == nil {
		t.Fatal("no session cookie")
	}

	body := postWithCookie(t, s.client, s.srv.URL+"/api/v1/auth/logout", cookie, http.StatusOK)
	if strings.Contains(body, "providerEndSessionUrl") {
		t.Errorf("logout offered a provider logout the provider does not support: %s", body)
	}
}

// The post-login destination must never leave the app's own origin.
func TestSSOCallbackWillNotOpenRedirect(t *testing.T) {
	s := newSSOServer(t)

	start := s.startLogin(t, `{"returnTo":"https://evil.example/steal"}`)
	cb := s.provider.Authorize(t, start["authorizationUrl"].(string), oidctest.Claims{
		Subject: "sub-1", Email: "ada@example.com", EmailVerified: true,
	})
	resp := s.callback(t, cb)
	_ = resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("callback redirected to %q, want /", got)
	}
}

// A failed callback renders a terminal page carrying AO's own stable code, and
// never the provider's text, the state, or the code.
func TestSSOCallbackFailureRendersASafePage(t *testing.T) {
	s := newSSOServer(t)

	resp := s.callback(t, url.Values{"state": {"never-issued"}, "code": {"some-code"}})
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body, "SSO_INVALID_STATE") {
		t.Errorf("failure page did not carry the stable code: %s", body)
	}
	if strings.Contains(body, "some-code") || strings.Contains(body, "never-issued") {
		t.Errorf("failure page echoed callback parameters: %s", body)
	}
	if sessionCookie(resp) != nil {
		t.Error("a failed callback set a session cookie")
	}
}

func TestSSODesktopHandoffOverHTTP(t *testing.T) {
	s := newSSOServer(t)
	secret := strings.Repeat("k", 43)

	start := s.startLogin(t, `{"clientKind":"desktop","handoffSecret":"`+secret+`"}`)
	flowID, _ := start["flowId"].(string)
	if flowID == "" {
		t.Fatal("start returned no flow id")
	}

	// Pending before the person finishes at the provider.
	pending := postJSON(t, s.client, s.srv.URL+"/api/v1/auth/oidc/claim",
		`{"flowId":"`+flowID+`","handoffSecret":"`+secret+`"}`, http.StatusOK)
	if !strings.Contains(pending.body, `"status":"pending"`) {
		t.Fatalf("claim before completion = %s", pending.body)
	}

	cb := s.provider.Authorize(t, start["authorizationUrl"].(string), oidctest.Claims{
		Subject: "sub-desktop", Email: "ada@example.com", EmailVerified: true,
	})
	cbResp := s.callback(t, cb)
	cbBody := readAll(t, cbResp)
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("desktop callback status = %d", cbResp.StatusCode)
	}
	if sessionCookie(cbResp) != nil {
		t.Error("the desktop callback handed a session to the system browser")
	}
	if strings.Contains(cbBody, flowID) || strings.Contains(cbBody, secret) || strings.Contains(cbBody, cb.Get("code")) {
		t.Errorf("desktop callback page leaked flow material: %s", cbBody)
	}

	// The wrong secret claims nothing even with the right flow id.
	postJSON(t, s.client, s.srv.URL+"/api/v1/auth/oidc/claim",
		`{"flowId":"`+flowID+`","handoffSecret":"`+strings.Repeat("z", 43)+`"}`, http.StatusUnauthorized)

	claimed := postJSON(t, s.client, s.srv.URL+"/api/v1/auth/oidc/claim",
		`{"flowId":"`+flowID+`","handoffSecret":"`+secret+`"}`, http.StatusOK)
	if !strings.Contains(claimed.body, `"status":"complete"`) {
		t.Fatalf("claim body = %s", claimed.body)
	}
	// The session arrives as a cookie, never in the body.
	cookie := sessionCookie(claimed.resp)
	if cookie == nil || cookie.Value == "" {
		t.Fatal("claim set no session cookie")
	}
	if strings.Contains(claimed.body, cookie.Value) {
		t.Error("claim response body carried the raw session token")
	}
	getWithCookie(t, s.client, s.srv.URL+"/api/v1/auth/me", cookie, http.StatusOK)
}

// An expired session is rejected, and the caller is told to sign in again
// rather than being silently handed an identity.
func TestSSOSessionExpiryIsEnforced(t *testing.T) {
	s := newSSOServer(t)
	start := s.startLogin(t, `{}`)
	cb := s.provider.Authorize(t, start["authorizationUrl"].(string), oidctest.Claims{
		Subject: "sub-1", Email: "ada@example.com", EmailVerified: true,
	})
	cookie := sessionCookie(s.callback(t, cb))
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	getWithCookie(t, s.client, s.srv.URL+"/api/v1/auth/me", cookie, http.StatusOK)

	// Age the session past its TTL by resolving through a manager whose clock
	// has moved on — the same store, a later "now".
	future := authsvc.New(s.store, func() time.Time { return time.Now().UTC().Add(authsvc.SessionTTL + time.Hour) })
	if _, err := future.ResolvePrincipal(context.Background(), cookie.Value); err == nil {
		t.Fatal("an expired session still resolved")
	}
}

// Trusted-local compatibility: an installation that configures no provider
// behaves exactly as it did before P4-A.
func TestTrustedLocalUnaffectedWithoutAnyOIDCConfiguration(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })
	if _, err := authMgr.Bootstrap(context.Background(), "owner@example.com", "supersecret1"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cfg := config.Config{TrustedLocalMode: true, AuthMode: domain.AuthModeTrustedLocal}
	router := NewRouterWithControl(cfg, discardLogger(), nil, APIDeps{Auth: authMgr}, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// No cookie, no login screen: the bootstrap admin is resolved as before,
	// and the method is recorded honestly as trusted_local.
	resp, err := http.Get(srv.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"status":"trusted-local"`) {
		t.Errorf("me body = %s", body)
	}
	if !strings.Contains(body, `"authMethod":"trusted_local"`) {
		t.Errorf("me body did not record the trusted-local method: %s", body)
	}

	// The sign-in surface offers passwords only.
	provResp, err := http.Get(srv.URL + "/api/v1/auth/providers")
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	provBody := readAll(t, provResp)
	if !strings.Contains(provBody, `"mode":"trusted_local"`) || !strings.Contains(provBody, `"passwordEnabled":true`) {
		t.Errorf("providers body = %s", provBody)
	}
	if strings.Contains(provBody, `"oidc"`) {
		t.Errorf("providers advertised SSO on an installation with no provider: %s", provBody)
	}

	// The OIDC routes exist and answer 501 rather than 404 — the same
	// optional-surface convention every other unwired service follows.
	startResp, err := http.Post(srv.URL+"/api/v1/auth/oidc/start", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = startResp.Body.Close()
	if startResp.StatusCode != http.StatusNotImplemented {
		t.Errorf("oidc start on an unconfigured install = %d, want 501", startResp.StatusCode)
	}

	// Password login still works untouched.
	loginResp, err := http.Post(srv.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"usernameOrEmail":"owner@example.com","password":"supersecret1"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	loginBody := readAll(t, loginResp)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("password login = %d: %s", loginResp.StatusCode, loginBody)
	}
	cookie := sessionCookie(loginResp)
	if cookie == nil {
		t.Fatal("password login set no session cookie")
	}
	meBody := getWithCookie(t, &http.Client{}, srv.URL+"/api/v1/auth/me", cookie, http.StatusOK)
	if !strings.Contains(meBody, `"authMethod":"password"`) {
		t.Errorf("password session did not record its method: %s", meBody)
	}
}

// A federated account has no local credential, so no password can ever
// authenticate it — including the empty one.
func TestFederatedAccountCannotBePasswordAuthenticated(t *testing.T) {
	s := newSSOServer(t)
	start := s.startLogin(t, `{}`)
	cb := s.provider.Authorize(t, start["authorizationUrl"].(string), oidctest.Claims{
		Subject: "sub-1", Email: "ada@example.com", EmailVerified: true,
	})
	if sessionCookie(s.callback(t, cb)) == nil {
		t.Fatal("sign-in failed")
	}
	for _, password := range []string{"", " ", "supersecret1", "$2a$10$"} {
		if _, err := s.auth.Authenticate(context.Background(), "ada@example.com", password, "test"); err == nil {
			t.Fatalf("password %q authenticated a federated account", password)
		}
	}
}

// The desktop cookie policy is the other half of the deployment story: a
// renderer on a different origin needs SameSite=None; Secure or the cookie is
// never sent at all.
func TestDesktopCookiePolicyIsCrossSiteCapable(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })
	if _, err := authMgr.Bootstrap(context.Background(), "owner@example.com", "supersecret1"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cfg := config.Config{TrustedLocalMode: false, SessionCookieCrossSite: true}
	router := NewRouterWithControl(cfg, discardLogger(), nil, APIDeps{Auth: authMgr}, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"usernameOrEmail":"owner@example.com","password":"supersecret1"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = resp.Body.Close()
	cookie := sessionCookie(resp)
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	if cookie.SameSite != http.SameSiteNoneMode {
		t.Errorf("SameSite = %v, want None under the desktop policy", cookie.SameSite)
	}
	if !cookie.Secure {
		t.Error("SameSite=None was set without Secure, which browsers reject")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
}

// --- small shared helpers -------------------------------------------------

func getWithCookie(t *testing.T, client *http.Client, target string, cookie *http.Cookie, wantStatus int) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s = %d, want %d: %s", target, resp.StatusCode, wantStatus, body)
	}
	return body
}

func postWithCookie(t *testing.T, client *http.Client, target string, cookie *http.Cookie, wantStatus int) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s = %d, want %d: %s", target, resp.StatusCode, wantStatus, body)
	}
	return body
}

type jsonPost struct {
	resp *http.Response
	body string
}

func postJSON(t *testing.T, client *http.Client, target, body string, wantStatus int) jsonPost {
	t.Helper()
	resp, err := client.Post(target, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	got := readAll(t, resp)
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s = %d, want %d: %s", target, resp.StatusCode, wantStatus, got)
	}
	return jsonPost{resp: resp, body: got}
}
