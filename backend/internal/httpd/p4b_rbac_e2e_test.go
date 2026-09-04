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
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/authz"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// p4bWorld is the realistic two-project installation section 30 asks for: an
// owner, a member with access to ONE project, a read-only viewer, a team, and
// a run in each project. Every assertion below is made over real HTTP against
// a real router and a real database -- the only fakes are the project and
// workflow read models, which serve rows rather than deciding anything.
type p4bWorld struct {
	t      *testing.T
	store  *store.Store
	auth   authsvc.Manager
	rbac   *rbac.Service
	srv    *httptest.Server
	client *http.Client

	owner  domain.User
	member domain.User
	viewer domain.User
}

func newP4BWorld(t *testing.T) *p4bWorld {
	t.Helper()
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })

	mk := func(name string, role domain.UserRole) domain.User {
		u, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
			DisplayName: name, Email: name + "@example.test", Username: name,
			Password: "correct-horse-" + name, Role: role,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return u
	}
	owner := mk("owner", domain.UserRoleOwner)
	member := mk("member", domain.UserRoleMember)
	viewer := mk("viewer", domain.UserRoleViewer)

	projects := map[domain.ProjectID]projectsvc.Project{}
	runs := map[string]domain.WorkflowRun{}
	for _, id := range []domain.ProjectID{"proj-one", "proj-two"} {
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
			ID: runID, ProjectID: string(id), Objective: "work", State: domain.WorkflowRunPending,
			PolicySnapshot: "{}", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if _, _, err := st.CreateWorkflowRun(ctx, run, nil); err != nil {
			t.Fatalf("seed run %s: %v", runID, err)
		}
		runs[runID] = run
	}

	rbacSvc := rbac.New(st, nil, rbac.NoopAudit{}, nil)
	authzSvc := authz.New(st)

	deps := APIDeps{
		Auth:              authMgr,
		Projects:          &fakeProjectManager{items: projects},
		Workflows:         &fakeWorkflowManager{runs: runs},
		ProjectOwnership:  st,
		WorkflowOwnership: st,
		SessionOwnership:  st,
		Authz:             authzSvc,
		ProjectScope:      st,
		RBAC:              rbacSvc,
	}
	srv := httptest.NewServer(NewRouterWithControl(config.Config{TrustedLocalMode: false}, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)

	return &p4bWorld{
		t: t, store: st, auth: authMgr, rbac: rbacSvc, srv: srv, client: &http.Client{},
		owner: owner, member: member, viewer: viewer,
	}
}

func (w *p4bWorld) login(name string) *http.Cookie {
	w.t.Helper()
	_, cookie := loginOK(w.t, w.srv.URL, name+"@example.test", "correct-horse-"+name)
	return cookie
}

// do issues one request with the caller's session and returns the status and
// body. Every "direct API call" assertion in this file goes through it, which
// is the point: the frontend is not involved, so nothing here can pass because
// a button was hidden.
func (w *p4bWorld) do(method, path string, cookie *http.Cookie, body string) (int, string) {
	w.t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, reader)
	if err != nil {
		w.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		w.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(out)
}

func (w *p4bWorld) expect(method, path string, cookie *http.Cookie, body string, want int) string {
	w.t.Helper()
	got, out := w.do(method, path, cookie, body)
	if got != want {
		w.t.Fatalf("%s %s: got %d want %d (%s)", method, path, got, want, out)
	}
	return out
}

// TestP4BProjectScopedAccessOverHTTP is the two-project smoke: the owner sees
// everything, the member sees exactly the project they were granted, and the
// project they were not granted is indistinguishable from one that does not
// exist.
func TestP4BProjectScopedAccessOverHTTP(t *testing.T) {
	w := newP4BWorld(t)
	ctx := context.Background()
	ownerActor := domain.Principal{User: w.owner, AuthMethod: domain.AuthMethodPassword}
	if _, err := w.rbac.GrantProjectAccess(ctx, ownerActor, "proj-one", domain.GrantSubjectUser, string(w.member.ID), domain.ProjectRoleMember); err != nil {
		t.Fatalf("grant member access: %v", err)
	}

	ownerCookie := w.login("owner")
	memberCookie := w.login("member")

	ownerList := w.expect(http.MethodGet, "/api/v1/projects", ownerCookie, "", http.StatusOK)
	if !strings.Contains(ownerList, "proj-one") || !strings.Contains(ownerList, "proj-two") {
		t.Fatalf("the owner must see both projects: %s", ownerList)
	}

	memberList := w.expect(http.MethodGet, "/api/v1/projects", memberCookie, "", http.StatusOK)
	if !strings.Contains(memberList, "proj-one") {
		t.Fatalf("the member is missing their granted project: %s", memberList)
	}
	if strings.Contains(memberList, "proj-two") {
		t.Fatalf("the project list leaked an inaccessible project: %s", memberList)
	}

	w.expect(http.MethodGet, "/api/v1/projects/proj-one", memberCookie, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/projects/proj-two", memberCookie, "", http.StatusNotFound)

	// Work inside the granted project is reachable; work inside the other is
	// not, and by id rather than through a list.
	w.expect(http.MethodGet, "/api/v1/workflows/run-proj-one", memberCookie, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/workflows/run-proj-two", memberCookie, "", http.StatusNotFound)

	runList := w.expect(http.MethodGet, "/api/v1/workflows", memberCookie, "", http.StatusOK)
	if strings.Contains(runList, "run-proj-two") {
		t.Fatalf("the run list leaked a run from an inaccessible project: %s", runList)
	}

	// Managing the project they can only work in is refused -- and refused as
	// 403, not 404: they can see it, so pretending it is missing would be a lie.
	w.expect(http.MethodDelete, "/api/v1/projects/proj-one", memberCookie, "", http.StatusForbidden)
	// Managing a project they cannot see stays a 404.
	w.expect(http.MethodDelete, "/api/v1/projects/proj-two", memberCookie, "", http.StatusNotFound)
}

// TestP4BTeamGrantsAndRevokesProjectAccess: access can arrive through a team
// and leaves the moment the membership does, with no session change and no
// cache to wait on.
func TestP4BTeamGrantsAndRevokesProjectAccess(t *testing.T) {
	w := newP4BWorld(t)
	ctx := context.Background()
	actor := domain.Principal{User: w.owner, AuthMethod: domain.AuthMethodPassword}

	team, err := w.rbac.CreateTeam(ctx, actor, "Platform", "")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := w.rbac.GrantProjectAccess(ctx, actor, "proj-two", domain.GrantSubjectTeam, string(team.ID), domain.ProjectRoleMember); err != nil {
		t.Fatalf("grant team access: %v", err)
	}

	memberCookie := w.login("member")
	w.expect(http.MethodGet, "/api/v1/projects/proj-two", memberCookie, "", http.StatusNotFound)

	if _, err := w.rbac.AddTeamMember(ctx, actor, team.ID, w.member.ID, domain.TeamRoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	w.expect(http.MethodGet, "/api/v1/projects/proj-two", memberCookie, "", http.StatusOK)

	if err := w.rbac.RemoveTeamMember(ctx, actor, team.ID, w.member.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	w.expect(http.MethodGet, "/api/v1/projects/proj-two", memberCookie, "", http.StatusNotFound)
}

// TestP4BAdministrationSurfacesAreEnforcedServerSide is the "hidden button"
// test: the member's UI would never render these controls, and calling the
// routes directly must still be refused.
func TestP4BAdministrationSurfacesAreEnforcedServerSide(t *testing.T) {
	w := newP4BWorld(t)
	memberCookie := w.login("member")
	ownerCookie := w.login("owner")

	denied := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/users", ""},
		{http.MethodPost, "/api/v1/users", `{"displayName":"x","email":"x@example.test","username":"x","password":"correct-horse-x","role":"member"}`},
		{http.MethodPatch, "/api/v1/users/" + string(w.viewer.ID) + "/role", `{"role":"admin"}`},
		{http.MethodPatch, "/api/v1/users/" + string(w.viewer.ID) + "/status", `{"status":"disabled"}`},
		{http.MethodGet, "/api/v1/teams", ""},
		{http.MethodPost, "/api/v1/teams", `{"name":"Sneaky","description":""}`},
		{http.MethodPatch, "/api/v1/settings/session-interface", `{"defaultSessionMode":"chat"}`},
		{http.MethodPost, "/api/v1/provider-profiles", `{}`},
	}
	for _, tc := range denied {
		got, body := w.do(tc.method, tc.path, memberCookie, tc.body)
		if got != http.StatusForbidden {
			t.Errorf("%s %s as a member: got %d want 403 (%s)", tc.method, tc.path, got, body)
		}
	}

	// The same routes answer the owner. A permission that denies everyone is
	// not a permission, it is an outage.
	w.expect(http.MethodGet, "/api/v1/users", ownerCookie, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/teams", ownerCookie, "", http.StatusOK)
}

// TestP4BViewerIsReadOnly: a viewer granted ADMIN on a project is still a
// viewer there. The global role caps the grant, so a per-project grant can
// never promote past the account's standing.
func TestP4BViewerIsReadOnly(t *testing.T) {
	w := newP4BWorld(t)
	ctx := context.Background()
	actor := domain.Principal{User: w.owner, AuthMethod: domain.AuthMethodPassword}
	if _, err := w.rbac.GrantProjectAccess(ctx, actor, "proj-one", domain.GrantSubjectUser, string(w.viewer.ID), domain.ProjectRoleAdmin); err != nil {
		t.Fatalf("grant viewer access: %v", err)
	}
	viewerCookie := w.login("viewer")

	w.expect(http.MethodGet, "/api/v1/projects/proj-one", viewerCookie, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/workflows/run-proj-one", viewerCookie, "", http.StatusOK)
	// Cancel, repair and continue are all refused, and refused as 403: the
	// viewer can see the run, so its existence is not the secret.
	for _, path := range []string{
		"/api/v1/workflows/run-proj-one/cancel",
		"/api/v1/workflows/run-proj-one/repair",
		"/api/v1/workflows/run-proj-one/continue",
	} {
		got, body := w.do(http.MethodPost, path, viewerCookie, `{}`)
		if got != http.StatusForbidden {
			t.Errorf("POST %s as a viewer: got %d want 403 (%s)", path, got, body)
		}
	}
	// And it cannot create a project, because creating one would make it that
	// project's administrator.
	got, body := w.do(http.MethodPost, "/api/v1/projects", viewerCookie, `{"path":"/tmp/new"}`)
	if got != http.StatusForbidden {
		t.Errorf("POST /api/v1/projects as a viewer: got %d want 403 (%s)", got, body)
	}
}

// TestP4BDisabledAccountLosesAccessImmediately: disabling an account stops it
// authenticating at all, so an already-issued cookie stops resolving.
func TestP4BDisabledAccountLosesAccessImmediately(t *testing.T) {
	w := newP4BWorld(t)
	ctx := context.Background()
	actor := domain.Principal{User: w.owner, AuthMethod: domain.AuthMethodPassword}
	if _, err := w.rbac.GrantProjectAccess(ctx, actor, "proj-one", domain.GrantSubjectUser, string(w.member.ID), domain.ProjectRoleMember); err != nil {
		t.Fatalf("grant: %v", err)
	}
	memberCookie := w.login("member")
	w.expect(http.MethodGet, "/api/v1/projects/proj-one", memberCookie, "", http.StatusOK)

	if _, err := w.rbac.SetUserStatus(ctx, actor, w.member.ID, domain.UserStatusDisabled); err != nil {
		t.Fatalf("disable member: %v", err)
	}
	w.expect(http.MethodGet, "/api/v1/projects/proj-one", memberCookie, "", http.StatusUnauthorized)
}

// TestP4BOwnerLockoutIsImpossibleOverHTTP exercises the invariant through the
// API rather than the service, because the API is what an operator actually
// reaches.
func TestP4BOwnerLockoutIsImpossibleOverHTTP(t *testing.T) {
	w := newP4BWorld(t)
	ownerCookie := w.login("owner")

	body := w.expect(http.MethodPatch, "/api/v1/users/"+string(w.owner.ID)+"/role", ownerCookie, `{"role":"admin"}`, http.StatusConflict)
	if !strings.Contains(body, rbac.CodeLastOwner) {
		t.Fatalf("demoting the owner should report %s: %s", rbac.CodeLastOwner, body)
	}
	body = w.expect(http.MethodPatch, "/api/v1/users/"+string(w.owner.ID)+"/status", ownerCookie, `{"status":"disabled"}`, http.StatusConflict)
	if !strings.Contains(body, rbac.CodeLastOwner) {
		t.Fatalf("disabling the owner should report %s: %s", rbac.CodeLastOwner, body)
	}

	owners, err := w.store.CountOwners(context.Background())
	if err != nil || owners != 1 {
		t.Fatalf("owners = %d (err %v), want 1", owners, err)
	}
}

// TestP4BMeReportsCapabilities is section 19: the frontend receives the
// backend's own answer about what it may do, rather than mapping a role name
// to buttons in React.
func TestP4BMeReportsCapabilities(t *testing.T) {
	w := newP4BWorld(t)

	read := func(cookie *http.Cookie) map[string]bool {
		body := w.expect(http.MethodGet, "/api/v1/auth/me", cookie, "", http.StatusOK)
		var out struct {
			Permissions []string `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode /me: %v (%s)", err, body)
		}
		set := map[string]bool{}
		for _, p := range out.Permissions {
			set[p] = true
		}
		return set
	}

	ownerPerms := read(w.login("owner"))
	if !ownerPerms[string(domain.PermUsersManage)] || !ownerPerms[string(domain.PermTeamsManage)] {
		t.Fatalf("the owner must report the administration capabilities: %v", ownerPerms)
	}
	memberPerms := read(w.login("member"))
	if memberPerms[string(domain.PermUsersManage)] || memberPerms[string(domain.PermSettingsManage)] {
		t.Fatalf("a member must not report administration capabilities: %v", memberPerms)
	}
	if !memberPerms[string(domain.PermSettingsRead)] {
		t.Fatalf("a member should still report the read capabilities the app renders from: %v", memberPerms)
	}
}

// TestP4BAuthorizationSurvivesADaemonRestart: authority lives in the database,
// not in the process. A new router over the same store answers identically for
// the same session cookie.
func TestP4BAuthorizationSurvivesADaemonRestart(t *testing.T) {
	w := newP4BWorld(t)
	ctx := context.Background()
	actor := domain.Principal{User: w.owner, AuthMethod: domain.AuthMethodPassword}
	if _, err := w.rbac.GrantProjectAccess(ctx, actor, "proj-one", domain.GrantSubjectUser, string(w.member.ID), domain.ProjectRoleMember); err != nil {
		t.Fatalf("grant: %v", err)
	}
	memberCookie := w.login("member")
	w.expect(http.MethodGet, "/api/v1/projects/proj-one", memberCookie, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/projects/proj-two", memberCookie, "", http.StatusNotFound)

	// "Restart": a second router, a second evaluator, the same database.
	restarted := httptest.NewServer(NewRouterWithControl(
		config.Config{TrustedLocalMode: false}, discardLogger(), nil, APIDeps{
			Auth:             w.auth,
			Projects:         &fakeProjectManager{items: map[domain.ProjectID]projectsvc.Project{"proj-one": {ID: "proj-one"}, "proj-two": {ID: "proj-two"}}},
			Workflows:        &fakeWorkflowManager{runs: map[string]domain.WorkflowRun{}},
			ProjectOwnership: w.store,
			Authz:            authz.New(w.store),
			ProjectScope:     w.store,
			RBAC:             w.rbac,
		}, ControlDeps{}))
	defer restarted.Close()

	assertStatus(t, w.client, restarted.URL+"/api/v1/projects/proj-one", memberCookie, http.StatusOK)
	assertStatus(t, w.client, restarted.URL+"/api/v1/projects/proj-two", memberCookie, http.StatusNotFound)
}

// TestP4BTrustedLocalOwnerIsUnaffected is section 6: the single-user desktop
// keeps working exactly as before. No cookie, no login screen, full authority.
func TestP4BTrustedLocalOwnerIsUnaffected(t *testing.T) {
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })
	owner, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "desktop", Email: "desktop@example.test", Username: "desktop",
		Password: "correct-horse-desktop", Role: domain.UserRoleOwner,
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-one", Path: "/tmp/one", RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	srv := httptest.NewServer(NewRouterWithControl(
		config.Config{TrustedLocalMode: true}, discardLogger(), nil, APIDeps{
			Auth:             authMgr,
			Projects:         &fakeProjectManager{items: map[domain.ProjectID]projectsvc.Project{"proj-one": {ID: "proj-one"}}},
			Workflows:        &fakeWorkflowManager{runs: map[string]domain.WorkflowRun{}},
			ProjectOwnership: st,
			Authz:            authz.New(st),
			ProjectScope:     st,
			RBAC:             rbac.New(st, nil, rbac.NoopAudit{}, nil),
		}, ControlDeps{}))
	defer srv.Close()

	client := &http.Client{}
	// No cookie at all -- the desktop's whole point.
	for _, path := range []string{
		"/api/v1/projects",
		"/api/v1/projects/proj-one",
		"/api/v1/users",
		"/api/v1/teams",
		"/api/v1/settings",
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Errorf("the trusted-local owner was refused %s with %d", path, resp.StatusCode)
		}
	}
	_ = owner
}

// TestP4BAuthorityIsIdenticalAcrossLoginMethods is section 27 over HTTP: the
// same account, reached through a password session and through the
// trusted-local synthesis, gets the same answers.
func TestP4BAuthorityIsIdenticalAcrossLoginMethods(t *testing.T) {
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })
	owner, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
		DisplayName: "solo", Email: "solo@example.test", Username: "solo",
		Password: "correct-horse-solo", Role: domain.UserRoleOwner,
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-one", Path: "/tmp/one", RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := st.SetProjectOwner(ctx, "proj-one", owner.ID); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	deps := APIDeps{
		Auth:             authMgr,
		Projects:         &fakeProjectManager{items: map[domain.ProjectID]projectsvc.Project{"proj-one": {ID: "proj-one"}}},
		Workflows:        &fakeWorkflowManager{runs: map[string]domain.WorkflowRun{}},
		ProjectOwnership: st,
		Authz:            authz.New(st),
		ProjectScope:     st,
		RBAC:             rbac.New(st, nil, rbac.NoopAudit{}, nil),
	}
	trusted := httptest.NewServer(NewRouterWithControl(config.Config{TrustedLocalMode: true}, discardLogger(), nil, deps, ControlDeps{}))
	defer trusted.Close()
	multi := httptest.NewServer(NewRouterWithControl(config.Config{TrustedLocalMode: false}, discardLogger(), nil, deps, ControlDeps{}))
	defer multi.Close()

	_, cookie := loginOK(t, multi.URL, "solo@example.test", "correct-horse-solo")
	client := &http.Client{}
	for _, path := range []string{"/api/v1/users", "/api/v1/teams", "/api/v1/projects/proj-one"} {
		// trusted-local, no cookie
		req, _ := http.NewRequest(http.MethodGet, trusted.URL+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("trusted GET %s: %v", path, err)
		}
		trustedStatus := resp.StatusCode
		_ = resp.Body.Close()

		// password session
		req2, _ := http.NewRequest(http.MethodGet, multi.URL+path, nil)
		req2.AddCookie(cookie)
		resp2, err := client.Do(req2)
		if err != nil {
			t.Fatalf("password GET %s: %v", path, err)
		}
		passwordStatus := resp2.StatusCode
		_ = resp2.Body.Close()

		if trustedStatus != passwordStatus {
			t.Errorf("%s: trusted-local answered %d but the same account's password session answered %d",
				path, trustedStatus, passwordStatus)
		}
	}
}

// TestP4BSmokeOnAnIsolatedDataDirSurvivesRestart is section 30's realistic
// smoke, and it is deliberately harder than the in-process restart above: the
// store is CLOSED and reopened from the same on-disk data directory between the
// two halves, so what is being checked is that authority lives in the database
// AO ships, not in a handle two routers happened to share.
func TestP4BSmokeOnAnIsolatedDataDirSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	type ids struct {
		owner  domain.UserID
		member domain.UserID
		team   domain.TeamID
	}
	var seeded ids
	var memberCookie *http.Cookie
	client := &http.Client{}

	projects := map[domain.ProjectID]projectsvc.Project{
		"proj-one": {ID: "proj-one", Name: "one", Path: "/tmp/one"},
		"proj-two": {ID: "proj-two", Name: "two", Path: "/tmp/two"},
	}

	// --- first boot: create the world -------------------------------------
	func() {
		st, err := sqlitetest.Open(dataDir)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer func() { _ = st.Close() }()

		authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })
		rbacSvc := rbac.New(st, nil, rbac.NoopAudit{}, nil)

		mk := func(name string, role domain.UserRole) domain.User {
			u, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
				DisplayName: name, Email: name + "@example.test", Username: name,
				Password: "correct-horse-" + name, Role: role,
			})
			if err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
			return u
		}
		owner := mk("owner", domain.UserRoleOwner)
		member := mk("member", domain.UserRoleMember)
		seeded.owner, seeded.member = owner.ID, member.ID

		for id := range projects {
			if err := st.UpsertProject(ctx, domain.ProjectRecord{
				ID: string(id), Path: "/tmp/" + string(id), RegisteredAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("seed project: %v", err)
			}
			if _, err := st.SetProjectOwner(ctx, id, owner.ID); err != nil {
				t.Fatalf("set project owner: %v", err)
			}
		}

		actor := domain.Principal{User: owner, AuthMethod: domain.AuthMethodPassword}
		team, err := rbacSvc.CreateTeam(ctx, actor, "Platform", "")
		if err != nil {
			t.Fatalf("create team: %v", err)
		}
		seeded.team = team.ID
		if _, err := rbacSvc.AddTeamMember(ctx, actor, team.ID, member.ID, domain.TeamRoleMember); err != nil {
			t.Fatalf("add member: %v", err)
		}
		if _, err := rbacSvc.GrantProjectAccess(ctx, actor, "proj-one", domain.GrantSubjectTeam, string(team.ID), domain.ProjectRoleMember); err != nil {
			t.Fatalf("grant: %v", err)
		}

		srv := httptest.NewServer(NewRouterWithControl(
			config.Config{TrustedLocalMode: false}, discardLogger(), nil, APIDeps{
				Auth:             authMgr,
				Projects:         &fakeProjectManager{items: projects},
				Workflows:        &fakeWorkflowManager{runs: map[string]domain.WorkflowRun{}},
				ProjectOwnership: st,
				Authz:            authz.New(st),
				ProjectScope:     st,
				RBAC:             rbacSvc,
			}, ControlDeps{}))
		defer srv.Close()

		_, memberCookie = loginOK(t, srv.URL, "member@example.test", "correct-horse-member")

		assertStatus(t, client, srv.URL+"/api/v1/projects/proj-one", memberCookie, http.StatusOK)
		assertStatus(t, client, srv.URL+"/api/v1/projects/proj-two", memberCookie, http.StatusNotFound)

		ownerList := getBody(t, client, srv.URL+"/api/v1/projects", memberCookie)
		if strings.Contains(ownerList, "proj-two") {
			t.Fatalf("the member's list leaked the project they cannot reach: %s", ownerList)
		}
	}()

	// --- second boot: same directory, new everything ----------------------
	st, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st.Close() }()

	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })
	rbacSvc := rbac.New(st, nil, rbac.NoopAudit{}, nil)
	srv := httptest.NewServer(NewRouterWithControl(
		config.Config{TrustedLocalMode: false}, discardLogger(), nil, APIDeps{
			Auth:             authMgr,
			Projects:         &fakeProjectManager{items: projects},
			Workflows:        &fakeWorkflowManager{runs: map[string]domain.WorkflowRun{}},
			ProjectOwnership: st,
			Authz:            authz.New(st),
			ProjectScope:     st,
			RBAC:             rbacSvc,
		}, ControlDeps{}))
	defer srv.Close()

	// The session issued before the "restart" still resolves, and resolves to
	// the same authority.
	assertStatus(t, client, srv.URL+"/api/v1/projects/proj-one", memberCookie, http.StatusOK)
	assertStatus(t, client, srv.URL+"/api/v1/projects/proj-two", memberCookie, http.StatusNotFound)

	// Removing the membership removes the access it was carrying, on the very
	// next request and with no session change.
	owner, ok, err := st.GetUserByID(ctx, seeded.owner)
	if err != nil || !ok {
		t.Fatalf("reload owner: %v", err)
	}
	actor := domain.Principal{User: owner, AuthMethod: domain.AuthMethodPassword}
	if err := rbacSvc.RemoveTeamMember(ctx, actor, seeded.team, seeded.member); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	assertStatus(t, client, srv.URL+"/api/v1/projects/proj-one", memberCookie, http.StatusNotFound)
}
