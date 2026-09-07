package httpd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc"
	"github.com/aoagents/agent-orchestrator/backend/internal/oidc/oidctest"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/authz"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/ssosvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// cli_oidc_auth_e2e_test.go — the CLI's identity on an SSO installation.
//
// P4-B shipped with a documented gap: under AO_AUTH_MODE=oidc a cookie-less
// loopback CLI request resolves no principal, so every permission-gated route
// answers 401 NOT_AUTHENTICATED and `ao send` / `ao workflow …` were unusable.
// The fix does NOT weaken that: it gives the CLI a way to BE somebody, through
// the loopback sign-in handoff the desktop supervisor already uses. These
// tests are the proof, made over real HTTP against a real router, a real
// database and a real (mock) identity provider:
//
//  1. a cookie-less request is still refused, with the stable code;
//  2. a CLI that completes the handoff holds an ordinary federated session;
//  3. that session is authorized exactly like a browser's — project grants and
//     row filtering are unchanged, so signing the CLI in grants no authority
//     the same person would not have in the app.

type cliOIDCWorld struct {
	t        *testing.T
	srv      *httptest.Server
	provider *oidctest.Provider
	client   *http.Client
	rbac     *rbac.Service
	owner    domain.User
}

// newCLIOIDCWorld wires an SSO-only installation (trusted-local off, exactly
// as config.loadOIDC derives it) holding two projects and a run in each.
func newCLIOIDCWorld(t *testing.T) *cliOIDCWorld {
	t.Helper()
	ctx := context.Background()
	p := oidctest.New(t)
	st := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })

	// A real installation already has an owner and owned projects before
	// anybody reaches for the CLI. Seeding one is what closes P4-B's
	// "before the first account exists, authorization is not yet meaningful"
	// latch, so the refusals below are the real ones and not that latch.
	owner, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "Ada", Email: "ada@example.com", Username: "ada",
		Password: "correct-horse-ada", Role: domain.UserRoleOwner,
	})
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	projects := map[domain.ProjectID]projectsvc.Project{}
	runs := map[string]domain.WorkflowRun{}
	for _, id := range []domain.ProjectID{"proj-cli", "proj-other"} {
		if err := st.UpsertProject(ctx, domain.ProjectRecord{
			ID: string(id), Path: "/tmp/" + string(id), DisplayName: string(id), RegisteredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed project %s: %v", id, err)
		}
		if _, err := st.SetProjectOwner(ctx, id, owner.ID); err != nil {
			t.Fatalf("set project owner: %v", err)
		}
		projects[id] = projectsvc.Project{ID: id, Name: string(id), Path: "/tmp/" + string(id)}
		runID := "run-" + string(id)
		run := domain.WorkflowRun{
			ID: runID, ProjectID: string(id), Objective: "work", State: domain.WorkflowRunNeedsAttention,
			PolicySnapshot: "{}", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if _, _, err := st.CreateWorkflowRun(ctx, run, nil); err != nil {
			t.Fatalf("seed run %s: %v", runID, err)
		}
		runs[runID] = run
	}

	oidcCfg := config.OIDCConfig{
		Enabled: true, LinkVerifiedEmail: true,
		Issuer: p.Issuer(), ClientID: p.ClientID,
		Scopes: oidc.DefaultScopes(), DisplayName: "Test IdP",
	}
	client := oidc.NewClient(oidc.Config{
		Issuer: oidcCfg.Issuer, ClientID: oidcCfg.ClientID, Scopes: oidcCfg.Scopes,
	}, p.Server.Client(), nil)
	sso := ssosvc.New(oidcCfg, client, st, ssoIssuer{authMgr}, nil)
	rbacSvc := rbac.New(st, nil, rbac.NoopAudit{}, nil)

	deps := APIDeps{
		Auth:              authMgr,
		SSO:               sso,
		Projects:          &fakeProjectManager{items: projects},
		Workflows:         &fakeWorkflowManager{runs: runs},
		ProjectOwnership:  st,
		WorkflowOwnership: st,
		SessionOwnership:  st,
		Authz:             authz.New(st),
		ProjectScope:      st,
		RBAC:              rbacSvc,
	}
	cfg := config.Config{TrustedLocalMode: false, AuthMode: domain.AuthModeOIDC, OIDC: oidcCfg}
	srv := httptest.NewServer(NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)

	return &cliOIDCWorld{
		t: t, srv: srv, provider: p, rbac: rbacSvc, owner: owner,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// signInAsCLI performs exactly what `ao auth login` performs: start a login
// with the loopback handoff kind and a locally generated secret, let a person
// sign in at the provider, then pick the session up over loopback. The CLI
// never sees the authorization code and the browser never sees the secret.
func (w *cliOIDCWorld) signInAsCLI(email, subject string) *http.Cookie {
	w.t.Helper()
	secret := strings.Repeat("c", 43)

	startResp, err := w.client.Post(w.srv.URL+"/api/v1/auth/oidc/start", "application/json",
		strings.NewReader(`{"clientKind":"desktop","handoffSecret":"`+secret+`"}`))
	if err != nil {
		w.t.Fatalf("start: %v", err)
	}
	var start struct {
		AuthorizationURL string `json:"authorizationUrl"`
		FlowID           string `json:"flowId"`
	}
	if err := json.NewDecoder(startResp.Body).Decode(&start); err != nil {
		w.t.Fatalf("decode start: %v", err)
	}
	_ = startResp.Body.Close()

	cb := w.provider.Authorize(w.t, start.AuthorizationURL, oidctest.Claims{
		Subject: subject, Email: email, EmailVerified: true, Name: email,
	})
	cbResp, err := w.client.Get(w.srv.URL + "/api/v1/auth/oidc/callback?" + cb.Encode())
	if err != nil {
		w.t.Fatalf("callback: %v", err)
	}
	_ = cbResp.Body.Close()

	claimResp, err := w.client.Post(w.srv.URL+"/api/v1/auth/oidc/claim", "application/json",
		strings.NewReader(`{"flowId":"`+start.FlowID+`","handoffSecret":"`+secret+`"}`))
	if err != nil {
		w.t.Fatalf("claim: %v", err)
	}
	body, _ := io.ReadAll(claimResp.Body)
	_ = claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"complete"`) {
		w.t.Fatalf("claim status=%d body=%s", claimResp.StatusCode, body)
	}
	for _, c := range claimResp.Cookies() {
		if c.Name == "ao_session" && c.Value != "" {
			// The credential the CLI stores is exactly this and nothing else.
			if strings.Contains(string(body), c.Value) {
				w.t.Error("the claim body carried the raw session token")
			}
			return c
		}
	}
	w.t.Fatal("claim issued no session cookie")
	return nil
}

func (w *cliOIDCWorld) get(path string, cookie *http.Cookie) (int, string) {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.srv.URL+path, nil)
	if err != nil {
		w.t.Fatalf("build request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		w.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestCLIWithoutCredentialsIsStillRefusedUnderOIDC pins the behavior the fix
// must NOT change: nothing about signing the CLI in makes a cookie-less
// request resolve an identity. The refusal keeps its stable code, which is
// what the CLI turns into "run `ao auth login`".
func TestCLIWithoutCredentialsIsStillRefusedUnderOIDC(t *testing.T) {
	w := newCLIOIDCWorld(t)
	for _, path := range []string{"/api/v1/auth/me", "/api/v1/workflows/run-proj-cli", "/api/v1/projects"} {
		status, body := w.get(path, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401 (%s)", path, status, body)
		}
		if !strings.Contains(body, "NOT_AUTHENTICATED") {
			t.Errorf("GET %s did not carry the stable code: %s", path, body)
		}
	}
}

// TestCLILoopbackSignInProducesAnOrdinaryAuthorizedIdentity is the fix: after
// the handoff the CLI holds a federated session, and every route treats it as
// the user it is.
func TestCLILoopbackSignInProducesAnOrdinaryAuthorizedIdentity(t *testing.T) {
	w := newCLIOIDCWorld(t)
	// The person signing the CLI in is the person who uses the app: the
	// federated login links to their existing account on a verified email
	// match, so the CLI ends up as them and not as a new principal.
	cookie := w.signInAsCLI(w.owner.Email, "sub-cli")

	status, body := w.get("/api/v1/auth/me", cookie)
	if status != http.StatusOK {
		t.Fatalf("me = %d (%s)", status, body)
	}
	for _, want := range []string{`"status":"authenticated"`, `"authMethod":"oidc"`, `"email":"` + w.owner.Email + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("me body missing %s: %s", want, body)
		}
	}
	// The route family `ao workflow recover status` and `ao workflow resume`
	// live in, reached with the identity the CLI now holds.
	if status, body := w.get("/api/v1/workflows/run-proj-cli", cookie); status != http.StatusOK {
		t.Fatalf("workflow read as the signed-in CLI = %d (%s)", status, body)
	}
}

// TestCLISignInGrantsNoAuthorityTheUserLacks is the containment test: the CLI
// identity is the person's identity, so project grants and row filtering
// decide exactly what it can reach. Signing the CLI in grants no authority the
// same person would not have in the app.
func TestCLISignInGrantsNoAuthorityTheUserLacks(t *testing.T) {
	w := newCLIOIDCWorld(t)
	ctx := context.Background()

	ownerCookie := w.signInAsCLI(w.owner.Email, "sub-owner")
	memberCookie := w.signInAsCLI("member@example.com", "sub-member")

	status, meBody := w.get("/api/v1/auth/me", ownerCookie)
	if status != http.StatusOK || !strings.Contains(meBody, `"role":"owner"`) {
		t.Fatalf("the linked federated login should be the owner: %d %s", status, meBody)
	}

	var member struct {
		User struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"user"`
	}
	_, memberBody := w.get("/api/v1/auth/me", memberCookie)
	if err := json.Unmarshal([]byte(memberBody), &member); err != nil {
		t.Fatalf("decode member me: %v", err)
	}
	if member.User.Role == string(domain.UserRoleOwner) {
		t.Fatalf("a second federated login must not become an owner: %s", memberBody)
	}

	// Before the grant, the member's CLI sees neither project's work.
	if status, body := w.get("/api/v1/workflows/run-proj-cli", memberCookie); status != http.StatusNotFound {
		t.Fatalf("ungranted run before the grant = %d, want 404 (%s)", status, body)
	}

	ownerActor := domain.Principal{User: w.owner, AuthMethod: domain.AuthMethodOIDC}
	if _, err := w.rbac.GrantProjectAccess(ctx, ownerActor, "proj-cli",
		domain.GrantSubjectUser, member.User.ID, domain.ProjectRoleMember); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if status, body := w.get("/api/v1/workflows/run-proj-cli", memberCookie); status != http.StatusOK {
		t.Fatalf("granted project's run = %d, want 200 (%s)", status, body)
	}
	// Not 403: a project this identity cannot read must not be shown to exist.
	if status, body := w.get("/api/v1/workflows/run-proj-other", memberCookie); status != http.StatusNotFound {
		t.Fatalf("ungranted project's run = %d, want 404 (%s)", status, body)
	}
	_, list := w.get("/api/v1/projects", memberCookie)
	if strings.Contains(list, "proj-other") {
		t.Fatalf("the project list leaked an inaccessible project to the CLI identity: %s", list)
	}
}
