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
	notificationsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/notification"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// p4c_tenant_e2e_test.go -- Step 15, over real HTTP against a real router, a
// real authorization service and a real SQLite database. The only fakes are
// the project and workflow read models, which serve rows and decide nothing.
//
// The world: two organizations, one account and one project in each, plus the
// installation owner who is cross-tenant by design. Every assertion is a
// direct API call, so nothing here can pass because a button was hidden.

// p4cProjectManager is the read-model fake plus the one write the tenancy
// tests need: registering a project. It records nothing about authorization --
// the controller decides the organization before this is ever called.
type p4cProjectManager struct {
	fakeProjectManager
	store *store.Store
}

func (f *p4cProjectManager) Add(ctx context.Context, in projectsvc.AddInput) (projectsvc.Project, error) {
	id := domain.ProjectID(*in.ProjectID)
	// Written to the real store, without a tenant: the whole point of the
	// registration tests is that the CONTROLLER decides the organization, so
	// the row must arrive here exactly as the real service would leave it --
	// on the column default, waiting to be placed.
	if err := f.store.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(id), Path: in.Path, DisplayName: *in.Name, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		return projectsvc.Project{}, err
	}
	p := projectsvc.Project{ID: id, Name: *in.Name, Path: in.Path}
	f.items[id] = p
	return p, nil
}

type p4cWorld struct {
	t      *testing.T
	store  *store.Store
	rbac   *rbac.Service
	srv    *httptest.Server
	client *http.Client

	installOwner domain.User
	userA        domain.User
	userB        domain.User
	tenantA      domain.TenantID
	tenantB      domain.TenantID
}

func newP4CWorld(t *testing.T) *p4cWorld {
	t.Helper()
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })
	rbacSvc := rbac.New(st, nil, rbac.NoopAudit{}, nil)
	authzSvc := authz.New(st)

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
	installOwner := mk("owner", domain.UserRoleOwner)
	userA := mk("usera", domain.UserRoleMember)
	userB := mk("userb", domain.UserRoleMember)

	ownerActor := domain.Principal{User: installOwner, AuthMethod: domain.AuthMethodPassword}
	mkTenant := func(name string) domain.TenantID {
		tn, err := rbacSvc.CreateTenant(ctx, ownerActor, name, "")
		if err != nil {
			t.Fatalf("create tenant %s: %v", name, err)
		}
		return tn.ID
	}
	tenantA := mkTenant("Acme")
	tenantB := mkTenant("Umbrella")

	// Each account belongs to exactly one organization, and to no other. The
	// default organization membership every new account gets is removed here,
	// because "belongs to one organization" is the world this file describes.
	for _, m := range []struct {
		user   domain.User
		tenant domain.TenantID
	}{{userA, tenantA}, {userB, tenantB}} {
		if _, err := rbacSvc.AddTenantMember(ctx, ownerActor, m.tenant, m.user.ID, domain.TenantRoleMember); err != nil {
			t.Fatalf("add %s to its organization: %v", m.user.Username, err)
		}
		if err := rbacSvc.RemoveTenantMember(ctx, ownerActor, domain.DefaultTenantID, m.user.ID); err != nil {
			t.Fatalf("remove %s from the default organization: %v", m.user.Username, err)
		}
	}

	projects := map[domain.ProjectID]projectsvc.Project{}
	runs := map[string]domain.WorkflowRun{}
	seed := func(id domain.ProjectID, tenant domain.TenantID, owner domain.User) {
		if err := st.UpsertProject(ctx, domain.ProjectRecord{
			ID: string(id), Path: "/tmp/" + string(id), DisplayName: string(id),
			RegisteredAt: time.Now().UTC(), TenantID: tenant,
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

		if _, _, err := st.CreateNotification(ctx, domain.NotificationRecord{
			ID: "notif-" + string(id), ProjectID: id, WorkflowRunID: runID,
			// A distinct dedupe key per project: P4-D collapses two
			// notifications with no session, no PR and no key into one, which
			// would quietly leave this world with a single notification and
			// make the isolation assertions below vacuous.
			DedupeKey: "seed-" + string(id),
			Type:      domain.NotificationNeedsInput, Title: "work waiting in " + string(id),
			Status: domain.NotificationUnread, Recipient: domain.NotificationRecipientLocal,
			Severity: domain.NotificationSeverityInfo, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed notification for %s: %v", id, err)
		}
	}
	seed("project-a", tenantA, userA)
	seed("project-b", tenantB, userB)

	deps := APIDeps{
		Auth:              authMgr,
		Projects:          &p4cProjectManager{fakeProjectManager: fakeProjectManager{items: projects}, store: st},
		Workflows:         &fakeWorkflowManager{runs: runs},
		ProjectOwnership:  st,
		ProjectTenancy:    st,
		WorkflowOwnership: st,
		SessionOwnership:  st,
		Authz:             authzSvc,
		ProjectScope:      st,
		RBAC:              rbacSvc,
		Notifications:     notificationsvc.New(notificationsvc.Deps{Store: st}),
	}
	srv := httptest.NewServer(NewRouterWithControl(config.Config{TrustedLocalMode: false}, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)

	return &p4cWorld{
		t: t, store: st, rbac: rbacSvc, srv: srv, client: &http.Client{},
		installOwner: installOwner, userA: userA, userB: userB,
		tenantA: tenantA, tenantB: tenantB,
	}
}

func (w *p4cWorld) login(name string) *http.Cookie {
	w.t.Helper()
	_, cookie := loginOK(w.t, w.srv.URL, name+"@example.test", "correct-horse-"+name)
	return cookie
}

func (w *p4cWorld) do(method, path string, cookie *http.Cookie, body string) (int, string) {
	w.t.Helper()
	req, err := http.NewRequest(method, w.srv.URL+path, strings.NewReader(body))
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

func (w *p4cWorld) expect(method, path string, cookie *http.Cookie, body string, want int) string {
	w.t.Helper()
	got, out := w.do(method, path, cookie, body)
	if got != want {
		w.t.Fatalf("%s %s: got %d want %d (%s)", method, path, got, want, out)
	}
	return out
}

// TestP4CUsersSeeOnlyTheirOwnOrganization is Step 15's core proof: A sees A, A
// does not see B, B sees B, B does not see A -- and the projects they cannot
// see are indistinguishable from projects that do not exist, even though both
// callers are told the exact id by this test.
func TestP4CUsersSeeOnlyTheirOwnOrganization(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")
	b := w.login("userb")

	listA := w.expect(http.MethodGet, "/api/v1/projects", a, "", http.StatusOK)
	if !strings.Contains(listA, "project-a") {
		t.Fatalf("user A is missing its own project: %s", listA)
	}
	if strings.Contains(listA, "project-b") {
		t.Fatalf("user A can see the other organization's project: %s", listA)
	}

	listB := w.expect(http.MethodGet, "/api/v1/projects", b, "", http.StatusOK)
	if !strings.Contains(listB, "project-b") {
		t.Fatalf("user B is missing its own project: %s", listB)
	}
	if strings.Contains(listB, "project-a") {
		t.Fatalf("user B can see the other organization's project: %s", listB)
	}

	// Guessing the id changes nothing. This is the whole threat model: the
	// list filter is a convenience, the resolver is the boundary.
	w.expect(http.MethodGet, "/api/v1/projects/project-a", a, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/projects/project-b", a, "", http.StatusNotFound)
	w.expect(http.MethodGet, "/api/v1/projects/project-b", b, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/projects/project-a", b, "", http.StatusNotFound)
}

// Everything that hangs off a project inherits its organization: workflow
// runs, sessions, usage, memory and the code graph are all reached through the
// project, so none of them needed its own tenancy rule and none of them may
// leak without one.
func TestP4CEverythingUnderAProjectIsScopedWithIt(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	// Reachable inside the caller's own organization.
	w.expect(http.MethodGet, "/api/v1/workflows/run-project-a", a, "", http.StatusOK)

	// Not reachable across one, by any door.
	for _, path := range []string{
		"/api/v1/workflows/run-project-b",
		"/api/v1/projects/project-b/memory",
		"/api/v1/projects/project-b/memory/graph",
		"/api/v1/projects/project-b/memory/items",
		"/api/v1/projects/project-b/access",
	} {
		status, body := w.do(http.MethodGet, path, a, "")
		if status == http.StatusOK {
			t.Fatalf("GET %s leaked across organizations: %s", path, body)
		}
		if status == http.StatusForbidden {
			t.Fatalf("GET %s answered 403, which confirms the resource exists; want 404", path)
		}
	}
}

// Notifications are ABOUT projects, so they are scoped with them. P4-B left
// this family open; P4-C closes it, and the badge count is scoped with the
// list so it cannot report activity the caller is not allowed to look at.
func TestP4CNotificationsAreScopedToTheOrganization(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")
	b := w.login("userb")

	listA := w.expect(http.MethodGet, "/api/v1/notifications?status=all", a, "", http.StatusOK)
	if !strings.Contains(listA, "project-a") {
		t.Fatalf("user A is missing its own notification: %s", listA)
	}
	if strings.Contains(listA, "project-b") {
		t.Fatalf("user A can read the other organization's notifications: %s", listA)
	}

	var counts struct {
		UnreadCount int `json:"unreadCount"`
	}
	raw := w.expect(http.MethodGet, "/api/v1/notifications/unread-count", a, "", http.StatusOK)
	if err := json.Unmarshal([]byte(raw), &counts); err != nil {
		t.Fatalf("decode unread count: %v", err)
	}
	if counts.UnreadCount != 1 {
		t.Fatalf("user A's badge counts %d notifications; want 1 (only its own organization's)", counts.UnreadCount)
	}

	// Acknowledging somebody else's notification by id must be refused BEFORE
	// the write, and must look exactly like an id that does not exist.
	w.expect(http.MethodPatch, "/api/v1/notifications/notif-project-b", a, `{"status":"read"}`, http.StatusNotFound)
	rows, err := w.store.ListNotifications(context.Background(), domain.NotificationListUnread, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("re-read notifications: %v", err)
	}
	stillUnread := false
	for _, row := range rows {
		if row.ID == "notif-project-b" {
			stillUnread = true
		}
	}
	if !stillUnread {
		t.Fatal("a refused acknowledgement still marked the other organization's notification read")
	}

	// And B still has its own.
	listB := w.expect(http.MethodGet, "/api/v1/notifications?status=all", b, "", http.StatusOK)
	if !strings.Contains(listB, "project-b") || strings.Contains(listB, "project-a") {
		t.Fatalf("user B's notifications are not scoped: %s", listB)
	}
}

// The organization API itself is scoped: a caller lists the organizations it
// belongs to, and one it does not belong to is a 404 rather than a 403 -- the
// route must not become an oracle for "does this installation have an
// organization with this id".
func TestP4CTenantAPIIsScopedToMembership(t *testing.T) {
	w := newP4CWorld(t)
	a := w.login("usera")

	list := w.expect(http.MethodGet, "/api/v1/tenants", a, "", http.StatusOK)
	if !strings.Contains(list, string(w.tenantA)) {
		t.Fatalf("user A cannot see its own organization: %s", list)
	}
	if strings.Contains(list, string(w.tenantB)) {
		t.Fatalf("user A can see an organization it does not belong to: %s", list)
	}
	if strings.Contains(list, string(domain.DefaultTenantID)) {
		t.Fatalf("user A can still see the default organization it was removed from: %s", list)
	}

	w.expect(http.MethodGet, "/api/v1/tenants/"+string(w.tenantA), a, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/tenants/"+string(w.tenantB), a, "", http.StatusNotFound)
	w.expect(http.MethodGet, "/api/v1/tenants/"+string(w.tenantB)+"/members", a, "", http.StatusNotFound)

	// A member may read its organization but not change it or its membership.
	w.expect(http.MethodPatch, "/api/v1/tenants/"+string(w.tenantA), a, `{"name":"Renamed"}`, http.StatusForbidden)
	w.expect(http.MethodPost, "/api/v1/tenants/"+string(w.tenantA)+"/members", a,
		`{"userId":"`+string(w.userB.ID)+`","role":"admin"}`, http.StatusForbidden)
	// Founding one is installation-wide, and a member does not hold it.
	w.expect(http.MethodPost, "/api/v1/tenants", a, `{"name":"Mine"}`, http.StatusForbidden)
	// Granting itself access to the other organization's project is refused
	// the same way reading it is.
	w.expect(http.MethodPut, "/api/v1/projects/project-b/access", a,
		`{"subjectKind":"user","subjectId":"`+string(w.userA.ID)+`","role":"admin"}`, http.StatusNotFound)
}

// The installation's owner is cross-tenant: they administer the accounts that
// belong to every organization. This is also the single-user desktop
// installation's behavior, which must not have changed at all.
func TestP4CInstallationOwnerReachesEveryOrganization(t *testing.T) {
	w := newP4CWorld(t)
	owner := w.login("owner")

	list := w.expect(http.MethodGet, "/api/v1/projects", owner, "", http.StatusOK)
	if !strings.Contains(list, "project-a") || !strings.Contains(list, "project-b") {
		t.Fatalf("the installation owner must see every project: %s", list)
	}
	w.expect(http.MethodGet, "/api/v1/projects/project-a", owner, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/projects/project-b", owner, "", http.StatusOK)

	tenants := w.expect(http.MethodGet, "/api/v1/tenants", owner, "", http.StatusOK)
	for _, id := range []string{string(w.tenantA), string(w.tenantB), string(domain.DefaultTenantID)} {
		if !strings.Contains(tenants, id) {
			t.Fatalf("the installation owner cannot see organization %s: %s", id, tenants)
		}
	}
	notifications := w.expect(http.MethodGet, "/api/v1/notifications?status=all", owner, "", http.StatusOK)
	if !strings.Contains(notifications, "project-a") || !strings.Contains(notifications, "project-b") {
		t.Fatalf("the installation owner must see every notification: %s", notifications)
	}
}

// An organization administrator administers everything inside it and nothing
// outside it -- including projects they neither own nor were granted.
func TestP4COrganizationAdministratorScope(t *testing.T) {
	w := newP4CWorld(t)
	ctx := context.Background()
	ownerActor := domain.Principal{User: w.installOwner, AuthMethod: domain.AuthMethodPassword}
	if _, err := w.rbac.AddTenantMember(ctx, ownerActor, w.tenantA, w.userA.ID, domain.TenantRoleAdmin); err != nil {
		t.Fatalf("promote user A to organization admin: %v", err)
	}
	// A project in organization A that user A neither owns nor was granted.
	if err := w.store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "project-a2", Path: "/tmp/project-a2", DisplayName: "project-a2",
		RegisteredAt: time.Now().UTC(), TenantID: w.tenantA,
	}); err != nil {
		t.Fatalf("seed second project: %v", err)
	}

	a := w.login("usera")
	w.expect(http.MethodGet, "/api/v1/tenants/"+string(w.tenantA)+"/members", a, "", http.StatusOK)
	w.expect(http.MethodPatch, "/api/v1/tenants/"+string(w.tenantA), a, `{"name":"Acme Renamed"}`, http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/projects/project-a2/access", a, "", http.StatusOK)

	// Still nothing across the boundary.
	w.expect(http.MethodGet, "/api/v1/tenants/"+string(w.tenantB), a, "", http.StatusNotFound)
	w.expect(http.MethodGet, "/api/v1/projects/project-b", a, "", http.StatusNotFound)
}

// Registering a project places it in the caller's organization automatically
// when there is only one it could go in -- the single-organization case must
// never see a picker -- and refuses to guess when there is more than one.
func TestP4CProjectRegistrationLandsInAnOrganization(t *testing.T) {
	w := newP4CWorld(t)
	ctx := context.Background()
	a := w.login("usera")

	body := `{"path":"/tmp/project-a3","projectId":"project-a3","name":"project-a3"}`
	w.expect(http.MethodPost, "/api/v1/projects", a, body, http.StatusCreated)
	tenant, ok, err := w.store.GetProjectTenant(ctx, "project-a3")
	if err != nil || !ok {
		t.Fatalf("read new project tenant: ok=%v err=%v", ok, err)
	}
	if tenant != w.tenantA {
		t.Fatalf("a project registered by user A landed in %q, want %q", tenant, w.tenantA)
	}

	// Put user A in a second organization: now the choice is genuinely
	// ambiguous, and guessing would eventually put somebody's repository in
	// front of the wrong company.
	ownerActor := domain.Principal{User: w.installOwner, AuthMethod: domain.AuthMethodPassword}
	if _, err := w.rbac.AddTenantMember(ctx, ownerActor, w.tenantB, w.userA.ID, domain.TenantRoleMember); err != nil {
		t.Fatalf("add user A to the second organization: %v", err)
	}
	a2 := w.login("usera")
	ambiguous := `{"path":"/tmp/project-a4","projectId":"project-a4","name":"project-a4"}`
	out := w.expect(http.MethodPost, "/api/v1/projects", a2, ambiguous, http.StatusBadRequest)
	if !strings.Contains(out, "TENANT_REQUIRED") {
		t.Fatalf("an ambiguous registration should ask which organization: %s", out)
	}

	// Naming one works; naming one the caller cannot reach does not.
	named := `{"path":"/tmp/project-a5","projectId":"project-a5","name":"project-a5","tenantId":"` + string(w.tenantB) + `"}`
	w.expect(http.MethodPost, "/api/v1/projects", a2, named, http.StatusCreated)
	tenant, _, err = w.store.GetProjectTenant(ctx, "project-a5")
	if err != nil {
		t.Fatalf("read named project tenant: %v", err)
	}
	if tenant != w.tenantB {
		t.Fatalf("a project registered into %q landed in %q", w.tenantB, tenant)
	}
	unreachable := `{"path":"/tmp/project-a6","projectId":"project-a6","name":"project-a6","tenantId":"tnt_default"}`
	w.expect(http.MethodPost, "/api/v1/projects", a2, unreachable, http.StatusNotFound)
}

// Moving a project between organizations hands it over completely: the losing
// organization's members stop seeing it, and the gaining organization's start.
func TestP4CMovingAProjectMovesEverythingWithIt(t *testing.T) {
	w := newP4CWorld(t)
	ctx := context.Background()
	a := w.login("usera")
	b := w.login("userb")

	w.expect(http.MethodGet, "/api/v1/projects/project-a", a, "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/projects/project-a", b, "", http.StatusNotFound)

	ownerActor := domain.Principal{User: w.installOwner, AuthMethod: domain.AuthMethodPassword}
	if err := w.rbac.AssignProjectTenant(ctx, ownerActor, "project-a", w.tenantB); err != nil {
		t.Fatalf("move the project: %v", err)
	}

	// The move takes effect on the very next request: the resolver reads
	// membership and tenancy per request and holds nothing across them. User A
	// owns the project row and still loses it, because ownership only confers
	// access inside an organization the owner belongs to.
	w.expect(http.MethodGet, "/api/v1/projects/project-a", a, "", http.StatusNotFound)

	// User B does NOT automatically gain it. Belonging to the organization a
	// project lives in is not the same as having access to that project --
	// that separation is what makes per-project access meaningful WITHIN an
	// organization, and it is the case most likely to be got wrong by an
	// implementation that treats tenancy as a second kind of grant.
	w.expect(http.MethodGet, "/api/v1/projects/project-a", b, "", http.StatusNotFound)

	// Its organization's administrator does, without a grant per project.
	if _, err := w.rbac.AddTenantMember(ctx, ownerActor, w.tenantB, w.userB.ID, domain.TenantRoleAdmin); err != nil {
		t.Fatalf("promote user B to organization admin: %v", err)
	}
	w.expect(http.MethodGet, "/api/v1/projects/project-a", b, "", http.StatusOK)
	// Its notification went with it.
	listA := w.expect(http.MethodGet, "/api/v1/notifications?status=all", a, "", http.StatusOK)
	if strings.Contains(listA, "project-a") {
		t.Fatalf("user A still reads notifications for a project that left its organization: %s", listA)
	}
}
