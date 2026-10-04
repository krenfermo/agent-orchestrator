package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// fakeStore is an in-memory authorization store. It counts reads so the
// per-request memoization can be asserted rather than assumed.
type fakeStore struct {
	users      int64
	owners     int64
	owned      map[domain.UserID][]domain.ProjectID
	teams      map[domain.UserID][]domain.TeamID
	userGrants map[domain.UserID][]domain.ProjectGrant
	teamGrants map[domain.TeamID][]domain.ProjectGrant
	// P4-C tenancy. All three default to the single default organization when
	// a test says nothing, which is exactly the shape migration 0156 leaves an
	// existing installation in: one tenant, everything in it, everyone a
	// member. That default is what lets every pre-P4-C case in this file keep
	// asserting what it always did while still running through the tenant
	// code path.
	tenantsOf     map[domain.UserID][]domain.TenantMembership
	projectTenant map[domain.ProjectID]domain.TenantID
	teamTenant    map[domain.TeamID]domain.TenantID
	reads         int
	err           error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:         1,
		owners:        1,
		owned:         map[domain.UserID][]domain.ProjectID{},
		teams:         map[domain.UserID][]domain.TeamID{},
		userGrants:    map[domain.UserID][]domain.ProjectGrant{},
		teamGrants:    map[domain.TeamID][]domain.ProjectGrant{},
		tenantsOf:     map[domain.UserID][]domain.TenantMembership{},
		projectTenant: map[domain.ProjectID]domain.TenantID{},
		teamTenant:    map[domain.TeamID]domain.TenantID{},
	}
}

// putProject places a project in an organization and records that the
// organization owns it.
func (f *fakeStore) putProject(id domain.ProjectID, tenant domain.TenantID) {
	f.projectTenant[id] = tenant
}

// join makes a user a member of an organization at the given role, replacing
// the default membership.
func (f *fakeStore) join(u domain.UserID, tenant domain.TenantID, role domain.TenantRole) {
	f.tenantsOf[u] = append(f.tenantsOf[u], domain.TenantMembership{
		TenantID: tenant, UserID: u, Role: role,
	})
}

func (f *fakeStore) CountUsers(context.Context) (int64, error)  { return f.users, f.err }
func (f *fakeStore) CountOwners(context.Context) (int64, error) { return f.owners, f.err }

func (f *fakeStore) ListProjectIDsByOwner(_ context.Context, u domain.UserID) ([]domain.ProjectID, error) {
	f.reads++
	return f.owned[u], f.err
}

func (f *fakeStore) ListActiveTeamIDsForUser(_ context.Context, u domain.UserID) ([]domain.TeamID, error) {
	f.reads++
	return f.teams[u], f.err
}

func (f *fakeStore) ListProjectGrantsForUser(_ context.Context, u domain.UserID) ([]domain.ProjectGrant, error) {
	f.reads++
	return f.userGrants[u], f.err
}

func (f *fakeStore) ListProjectGrantsForTeams(_ context.Context, ts []domain.TeamID) ([]domain.ProjectGrant, error) {
	f.reads++
	var out []domain.ProjectGrant
	for _, t := range ts {
		out = append(out, f.teamGrants[t]...)
	}
	return out, f.err
}

func (f *fakeStore) ListActiveTenantMembershipsForUser(_ context.Context, u domain.UserID) ([]domain.TenantMembership, error) {
	f.reads++
	if m, ok := f.tenantsOf[u]; ok {
		return m, f.err
	}
	// The migrated single-tenant installation: everyone is in the default
	// organization. Member, not admin -- so a test that wants organization-wide
	// authority has to ask for it.
	return []domain.TenantMembership{{
		TenantID: domain.DefaultTenantID, UserID: u, Role: domain.TenantRoleMember,
	}}, f.err
}

func (f *fakeStore) tenantOfProject(id domain.ProjectID) domain.TenantID {
	if t, ok := f.projectTenant[id]; ok {
		return t
	}
	// Mirrors projects.tenant_id NOT NULL DEFAULT 'tnt_default': a project
	// nobody placed anywhere is in the default organization, never nowhere.
	return domain.DefaultTenantID
}

func (f *fakeStore) ListProjectIDsInTenants(_ context.Context, tenants []domain.TenantID) (map[domain.ProjectID]domain.TenantID, error) {
	f.reads++
	want := map[domain.TenantID]bool{}
	for _, t := range tenants {
		want[t] = true
	}
	out := map[domain.ProjectID]domain.TenantID{}
	for id, tenant := range f.projectTenant {
		if want[tenant] {
			out[id] = tenant
		}
	}
	return out, f.err
}

func (f *fakeStore) ListProjectTenancy(_ context.Context, ids []domain.ProjectID) (map[domain.ProjectID]domain.TenantID, error) {
	f.reads++
	out := make(map[domain.ProjectID]domain.TenantID, len(ids))
	for _, id := range ids {
		out[id] = f.tenantOfProject(id)
	}
	return out, f.err
}

func (f *fakeStore) ListTeamTenancy(_ context.Context, ids []domain.TeamID) (map[domain.TeamID]domain.TenantID, error) {
	f.reads++
	out := make(map[domain.TeamID]domain.TenantID, len(ids))
	for _, id := range ids {
		if t, ok := f.teamTenant[id]; ok {
			out[id] = t
			continue
		}
		out[id] = domain.DefaultTenantID
	}
	return out, f.err
}

func user(id string, role domain.UserRole) domain.User {
	return domain.User{ID: domain.UserID(id), Role: role, Status: domain.UserStatusActive}
}

func principal(u domain.User, method domain.AuthMethod) domain.Principal {
	return domain.Principal{User: u, AuthMethod: method}
}

func grant(project domain.ProjectID, kind domain.GrantSubjectKind, subject string, role domain.ProjectRole) domain.ProjectGrant {
	return domain.ProjectGrant{ProjectID: project, SubjectKind: kind, SubjectID: subject, Role: role}
}

// TestSecurityMatrix is P4-B section 26's matrix expressed as one table. Each
// row is a (principal, permission, resource) triple and the answer the
// evaluator must give; a regression anywhere in the role tables fails here
// with the exact row that changed.
func TestSecurityMatrix(t *testing.T) {
	const (
		projectA = domain.ProjectID("project-a")
		projectB = domain.ProjectID("project-b")
	)

	owner := user("owner", domain.UserRoleOwner)
	admin := user("admin", domain.UserRoleAdmin)
	member := user("member", domain.UserRoleMember)
	viewer := user("viewer", domain.UserRoleViewer)
	disabled := domain.User{ID: "gone", Role: domain.UserRoleAdmin, Status: domain.UserStatusDisabled}

	store := newFakeStore()
	// member reaches project A by a direct grant, and nothing else.
	store.userGrants[member.ID] = []domain.ProjectGrant{grant(projectA, domain.GrantSubjectUser, "member", domain.ProjectRoleMember)}
	// viewer is granted ADMIN on project A -- the global viewer role must cap
	// that back down to read-only.
	store.userGrants[viewer.ID] = []domain.ProjectGrant{grant(projectA, domain.GrantSubjectUser, "viewer", domain.ProjectRoleAdmin)}

	svc := New(store)

	cases := []struct {
		name  string
		who   domain.Principal
		perm  domain.Permission
		res   domain.AuthzResource
		allow bool
	}{
		{"owner manages users", principal(owner, domain.AuthMethodPassword), domain.PermUsersManage, domain.GlobalResource(), true},
		{"owner manages settings", principal(owner, domain.AuthMethodOIDC), domain.PermSettingsManage, domain.GlobalResource(), true},
		{"owner reaches every project", principal(owner, domain.AuthMethodPassword), domain.PermWorkflowRun, domain.ProjectResource(projectB), true},
		{"admin manages users", principal(admin, domain.AuthMethodOIDC), domain.PermUsersManage, domain.GlobalResource(), true},
		{"admin manages providers", principal(admin, domain.AuthMethodPassword), domain.PermProviderManage, domain.GlobalResource(), true},
		{"admin reads audit", principal(admin, domain.AuthMethodPassword), domain.PermAuditRead, domain.GlobalResource(), true},

		{"member cannot manage users", principal(member, domain.AuthMethodPassword), domain.PermUsersManage, domain.GlobalResource(), false},
		{"member cannot read users", principal(member, domain.AuthMethodPassword), domain.PermUsersRead, domain.GlobalResource(), false},
		{"member cannot manage teams", principal(member, domain.AuthMethodPassword), domain.PermTeamsManage, domain.GlobalResource(), false},
		{"member cannot manage settings", principal(member, domain.AuthMethodPassword), domain.PermSettingsManage, domain.GlobalResource(), false},
		{"member cannot manage providers", principal(member, domain.AuthMethodPassword), domain.PermProviderManage, domain.GlobalResource(), false},
		{"member cannot read audit", principal(member, domain.AuthMethodPassword), domain.PermAuditRead, domain.GlobalResource(), false},
		{"member reads granted project", principal(member, domain.AuthMethodPassword), domain.PermProjectRead, domain.ProjectResource(projectA), true},
		{"member runs in granted project", principal(member, domain.AuthMethodPassword), domain.PermWorkflowRun, domain.ProjectResource(projectA), true},
		{"member cancels in granted project", principal(member, domain.AuthMethodPassword), domain.PermWorkflowCancel, domain.ProjectResource(projectA), true},
		{"member repairs in granted project", principal(member, domain.AuthMethodPassword), domain.PermWorkflowRepair, domain.ProjectResource(projectA), true},
		{"member cannot manage granted project access", principal(member, domain.AuthMethodPassword), domain.PermProjectAccessManage, domain.ProjectResource(projectA), false},
		{"member denied other project", principal(member, domain.AuthMethodPassword), domain.PermProjectRead, domain.ProjectResource(projectB), false},
		{"member denied run in other project", principal(member, domain.AuthMethodPassword), domain.PermWorkflowRun, domain.ProjectResource(projectB), false},

		{"viewer reads granted project", principal(viewer, domain.AuthMethodPassword), domain.PermProjectRead, domain.ProjectResource(projectA), true},
		{"viewer cannot run despite admin grant", principal(viewer, domain.AuthMethodPassword), domain.PermWorkflowRun, domain.ProjectResource(projectA), false},
		{"viewer cannot cancel", principal(viewer, domain.AuthMethodPassword), domain.PermWorkflowCancel, domain.ProjectResource(projectA), false},
		{"viewer cannot manage project despite admin grant", principal(viewer, domain.AuthMethodPassword), domain.PermProjectManage, domain.ProjectResource(projectA), false},
		{"viewer cannot create projects", principal(viewer, domain.AuthMethodPassword), domain.PermProjectCreate, domain.GlobalResource(), false},

		{"disabled account is denied everything", principal(disabled, domain.AuthMethodPassword), domain.PermSettingsRead, domain.GlobalResource(), false},
		{"disabled account cannot read a project", principal(disabled, domain.AuthMethodPassword), domain.PermProjectRead, domain.ProjectResource(projectA), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.Authorize(WithCache(context.Background()), tc.who, tc.perm, tc.res)
			if tc.allow && err != nil {
				t.Fatalf("expected allow, got %v", err)
			}
			if !tc.allow && err == nil {
				t.Fatal("expected denial, got allow")
			}
		})
	}
}

// TestAuthorityDoesNotDependOnLoginMethod is section 27: the SAME account
// authenticated three different ways must receive identical authority.
// Authorization reads the resolved AO user and nothing about how it was
// resolved, and this is the test that says so out loud.
func TestAuthorityDoesNotDependOnLoginMethod(t *testing.T) {
	store := newFakeStore()
	store.userGrants["u"] = []domain.ProjectGrant{grant("p1", domain.GrantSubjectUser, "u", domain.ProjectRoleMember)}
	svc := New(store)
	account := user("u", domain.UserRoleMember)

	methods := []domain.AuthMethod{domain.AuthMethodPassword, domain.AuthMethodOIDC, domain.AuthMethodTrustedLocal}
	perms := []struct {
		perm domain.Permission
		res  domain.AuthzResource
	}{
		{domain.PermWorkflowRun, domain.ProjectResource("p1")},
		{domain.PermWorkflowRun, domain.ProjectResource("p2")},
		{domain.PermUsersManage, domain.GlobalResource()},
		{domain.PermSettingsRead, domain.GlobalResource()},
	}

	for _, p := range perms {
		var first bool
		for i, method := range methods {
			err := svc.Authorize(WithCache(context.Background()), principal(account, method), p.perm, p.res)
			allowed := err == nil
			if i == 0 {
				first = allowed
				continue
			}
			if allowed != first {
				t.Fatalf("%s on %v: method %s allowed=%v but %s allowed=%v",
					p.perm, p.res.Project, methods[0], first, method, allowed)
			}
		}
	}
}

// TestTeamMembershipGrantsAndRevokesAccess covers "team membership grants
// access" and "removed membership removes access" as one story, because they
// are one story: the resolver reads membership per request and holds nothing
// across them.
func TestTeamMembershipGrantsAndRevokesAccess(t *testing.T) {
	store := newFakeStore()
	store.teamGrants["team-1"] = []domain.ProjectGrant{grant("p1", domain.GrantSubjectTeam, "team-1", domain.ProjectRoleMember)}
	svc := New(store)
	account := principal(user("u", domain.UserRoleMember), domain.AuthMethodPassword)

	if err := svc.Authorize(WithCache(context.Background()), account, domain.PermWorkflowRun, domain.ProjectResource("p1")); err == nil {
		t.Fatal("a non-member must not inherit the team's grant")
	}

	store.teams["u"] = []domain.TeamID{"team-1"}
	if err := svc.Authorize(WithCache(context.Background()), account, domain.PermWorkflowRun, domain.ProjectResource("p1")); err != nil {
		t.Fatalf("team membership must grant the team's access: %v", err)
	}

	store.teams["u"] = nil
	if err := svc.Authorize(WithCache(context.Background()), account, domain.PermWorkflowRun, domain.ProjectResource("p1")); err == nil {
		t.Fatal("removing the membership must remove the access")
	}
}

// TestGrantsCombineByMostGenerous: a direct viewer grant plus a team member
// grant is member access, not viewer access. Combining by maximum is what
// makes "I also added them to the team" mean what the person intended.
func TestGrantsCombineByMostGenerous(t *testing.T) {
	store := newFakeStore()
	store.userGrants["u"] = []domain.ProjectGrant{grant("p1", domain.GrantSubjectUser, "u", domain.ProjectRoleViewer)}
	store.teams["u"] = []domain.TeamID{"t1"}
	store.teamGrants["t1"] = []domain.ProjectGrant{grant("p1", domain.GrantSubjectTeam, "t1", domain.ProjectRoleMember)}

	sub, err := New(store).Resolve(context.Background(), principal(user("u", domain.UserRoleMember), domain.AuthMethodPassword))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	role, ok := sub.ProjectRole("p1")
	if !ok || role != domain.ProjectRoleMember {
		t.Fatalf("combined role = %q (ok=%v), want member", role, ok)
	}
}

// TestProjectOwnerKeepsAdminAccess is the 8P-A compatibility bridge: a project
// registered before P4-B has an owner_user_id and no grant row, and that owner
// must keep full authority over it without any migration inventing grants.
func TestProjectOwnerKeepsAdminAccess(t *testing.T) {
	store := newFakeStore()
	store.owned["u"] = []domain.ProjectID{"legacy"}
	svc := New(store)
	account := principal(user("u", domain.UserRoleMember), domain.AuthMethodPassword)

	for _, perm := range []domain.Permission{
		domain.PermProjectRead, domain.PermProjectManage,
		domain.PermWorkflowRun, domain.PermProjectAccessManage,
	} {
		if err := svc.Authorize(WithCache(context.Background()), account, perm, domain.ProjectResource("legacy")); err != nil {
			t.Fatalf("project owner denied %s: %v", perm, err)
		}
	}
}

// TestTrustedLocalRecoversAnOwnerlessInstallation covers section 12's
// "trusted-local bootstrap remains recoverable" and, just as importantly, that
// the rule STOPS once an owner exists -- otherwise it would be a permanent
// backdoor rather than a recovery path.
func TestTrustedLocalRecoversAnOwnerlessInstallation(t *testing.T) {
	store := newFakeStore()
	store.owners = 0
	svc := New(store)
	local := principal(user("u", domain.UserRoleMember), domain.AuthMethodTrustedLocal)

	if err := svc.Authorize(WithCache(context.Background()), local, domain.PermUsersManage, domain.GlobalResource()); err != nil {
		t.Fatalf("an ownerless installation must stay recoverable from loopback: %v", err)
	}

	store.owners = 1
	if err := svc.Authorize(WithCache(context.Background()), local, domain.PermUsersManage, domain.GlobalResource()); err == nil {
		t.Fatal("once an owner exists, a trusted-local member must be only a member")
	}
}

// TestTrustedLocalOwnerHasFullAuthority is the desktop case: the synthesized
// bootstrap admin IS the owner, so nothing about the single-user install
// changes under P4-B.
func TestTrustedLocalOwnerHasFullAuthority(t *testing.T) {
	svc := New(newFakeStore())
	local := principal(user("owner", domain.UserRoleOwner), domain.AuthMethodTrustedLocal)
	for _, perm := range domain.AllPermissions {
		res := domain.GlobalResource()
		switch domain.ScopeOf(perm) {
		case domain.AuthzScopeProject:
			res = domain.ProjectResource("anything")
		case domain.AuthzScopeTenant:
			// Any organization at all, including one the owner holds no
			// membership row in: the installation owner is cross-tenant.
			res = domain.TenantResource("some-other-org")
		}
		if err := svc.Authorize(WithCache(context.Background()), local, perm, res); err != nil {
			t.Fatalf("the trusted-local owner was denied %s: %v", perm, err)
		}
	}
}

// TestUnauthenticatedIsNotForbidden keeps section 23's distinction honest: a
// request with no identity is 401, never 403. Collapsing the two would make an
// anonymous caller indistinguishable from a denied one in every log and every
// client branch.
func TestUnauthenticatedIsNotForbidden(t *testing.T) {
	err := New(newFakeStore()).Authorize(context.Background(), domain.Principal{}, domain.PermSettingsRead, domain.GlobalResource())
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindUnauthorized {
		t.Fatalf("no principal must be 401-class, got %v", err)
	}
}

// TestUnknownRoleIsLeastPrivilege: a role this build cannot interpret must not
// be a licence. A forward-migrated database or a hand-edited row degrades to
// read-only, never to admin.
func TestUnknownRoleIsLeastPrivilege(t *testing.T) {
	svc := New(newFakeStore())
	odd := principal(user("u", domain.UserRole("superuser")), domain.AuthMethodPassword)
	if err := svc.Authorize(context.Background(), odd, domain.PermUsersManage, domain.GlobalResource()); err == nil {
		t.Fatal("an unknown role must not grant installation management")
	}
	if err := svc.Authorize(context.Background(), odd, domain.PermSettingsRead, domain.GlobalResource()); err != nil {
		t.Fatalf("an unknown role should still read what a viewer reads: %v", err)
	}
}

// TestResolutionIsMemoizedPerRequest is section 28: a handler that asks many
// questions must pay for one resolution. Without the cache this is a
// fresh round of reads per question.
func TestResolutionIsMemoizedPerRequest(t *testing.T) {
	store := newFakeStore()
	svc := New(store)
	ctx := WithCache(context.Background())
	account := principal(user("u", domain.UserRoleMember), domain.AuthMethodPassword)

	for i := 0; i < 25; i++ {
		_ = svc.Authorize(ctx, account, domain.PermProjectRead, domain.ProjectResource("p1"))
	}
	// One resolution reads: tenant memberships, owned projects, teams, direct
	// grants, and the tenancy of the projects those point at. Five is the
	// whole budget for a request, however many questions it asks.
	if store.reads > 5 {
		t.Fatalf("resolved the subject %d times across one request; want at most 5 reads total", store.reads)
	}
	if store.reads == 0 {
		t.Fatal("expected the subject to be resolved at least once")
	}
}

// TestAdministratorsSkipGrantLoading proves the universal-access shortcut is
// real: an administrator reaches every project in every organization, so
// loading their grants would scale with the installation and change no answer.
//
// The one read that remains is the administrator's own tenant memberships,
// which is bounded by their membership rather than by the installation, and
// which the UI needs in order to show them which organizations they are
// actually in as opposed to which ones they can reach.
func TestAdministratorsSkipGrantLoading(t *testing.T) {
	store := newFakeStore()
	svc := New(store)
	admin := principal(user("a", domain.UserRoleAdmin), domain.AuthMethodPassword)

	if err := svc.Authorize(context.Background(), admin, domain.PermProjectRead, domain.ProjectResource("anything")); err != nil {
		t.Fatalf("administrator denied a project: %v", err)
	}
	if store.reads != 1 {
		t.Fatalf("administrator resolution made %d reads; want exactly 1 (its own tenant memberships)", store.reads)
	}
}

// TestStoreFailureDeniesRatherThanAllows: an authorization lookup that cannot
// complete must fail closed.
func TestStoreFailureDeniesRatherThanAllows(t *testing.T) {
	store := newFakeStore()
	store.err = errors.New("database is gone")
	err := New(store).Authorize(context.Background(),
		principal(user("u", domain.UserRoleMember), domain.AuthMethodPassword),
		domain.PermProjectRead, domain.ProjectResource("p1"))
	if err == nil {
		t.Fatal("a failed authorization lookup must deny")
	}
}

// TestEveryPermissionHasAScope guards the role tables against a permission
// that is added to the vocabulary and then never decided by anything.
func TestEveryPermissionHasAScope(t *testing.T) {
	for _, perm := range domain.AllPermissions {
		scope := domain.ScopeOf(perm)
		switch scope {
		case domain.AuthzScopeGlobal:
			if !globalRolePermissions[domain.UserRoleOwner][perm] {
				t.Errorf("global permission %s is granted to no role, not even the owner", perm)
			}
		case domain.AuthzScopeProject:
			if !projectRolePermissions[domain.ProjectRoleAdmin][perm] {
				t.Errorf("project permission %s is granted by no project role, not even admin", perm)
			}
		case domain.AuthzScopeTenant:
			if !tenantRolePermissions[domain.TenantRoleOwner][perm] {
				t.Errorf("tenant permission %s is granted by no tenant role, not even owner", perm)
			}
		default:
			t.Errorf("permission %s has an unknown scope %q", perm, scope)
		}
	}
}

// TestGlobalPermissionsAreReportableCapabilities checks the shape the frontend
// consumes: an ordered, role-appropriate list, and never a project permission
// (which has no meaning without a project).
func TestGlobalPermissionsAreReportableCapabilities(t *testing.T) {
	sub, err := New(newFakeStore()).Resolve(context.Background(),
		principal(user("m", domain.UserRoleMember), domain.AuthMethodPassword))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, p := range sub.GlobalPermissions() {
		if domain.ScopeOf(p) != domain.AuthzScopeGlobal {
			t.Fatalf("capability list leaked the project-scoped %s", p)
		}
		if p == domain.PermUsersManage {
			t.Fatal("a member must not report users.manage as a capability")
		}
	}
}
