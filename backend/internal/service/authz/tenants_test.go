package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// tenants_test.go -- P4-C's isolation matrix.
//
// The single property everything here circles: a project in an organization
// the caller does not belong to never enters the resolved subject, so every
// question already asked about a project answers "no" without any of those
// call sites knowing organizations exist. These tests ask the questions from
// the outside, the way a controller does.

const (
	tenantA = domain.TenantID("tenant-a")
	tenantB = domain.TenantID("tenant-b")
)

// twoTenantStore is the fixture Step 15 describes, in memory: two
// organizations, one project each, and one account in each.
func twoTenantStore() *fakeStore {
	store := newFakeStore()
	store.putProject("project-a", tenantA)
	store.putProject("project-b", tenantB)
	store.join("user-a", tenantA, domain.TenantRoleMember)
	store.join("user-b", tenantB, domain.TenantRoleMember)
	return store
}

// TestForeignTenantProjectIsUnreachableEvenByID is the headline invariant. The
// caller knows the other organization's project id -- guessing it is the whole
// threat model -- and it makes no difference.
func TestForeignTenantProjectIsUnreachableEvenByID(t *testing.T) {
	store := twoTenantStore()
	// user-a owns project-a outright, and is even GRANTED admin on project-b.
	// The grant is in the fixture on purpose: it must not be enough, because
	// the organization is the outer boundary and a grant cannot cross it.
	store.owned["user-a"] = []domain.ProjectID{"project-a"}
	store.userGrants["user-a"] = []domain.ProjectGrant{
		grant("project-b", domain.GrantSubjectUser, "user-a", domain.ProjectRoleAdmin),
	}
	svc := New(store)
	a := principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword)

	if err := svc.Authorize(context.Background(), a, domain.PermProjectRead, domain.ProjectResource("project-a")); err != nil {
		t.Fatalf("user-a was denied its own organization's project: %v", err)
	}
	for _, perm := range []domain.Permission{
		domain.PermProjectRead, domain.PermProjectManage,
		domain.PermWorkflowRead, domain.PermWorkflowRun,
		domain.PermSessionRead, domain.PermSessionWrite,
		domain.PermMemoryRead, domain.PermUsageRead,
		domain.PermProjectAccessRead, domain.PermProjectAccessManage,
	} {
		if err := svc.Authorize(context.Background(), a, perm, domain.ProjectResource("project-b")); err == nil {
			t.Fatalf("a direct admin grant let user-a %s across an organization boundary", perm)
		}
	}
}

// A team's grants stop at its own organization. A team moved into another
// organization after its grants were written must stop conferring them at
// once, which is why the resolver re-checks rather than trusting the row.
func TestTeamGrantsDoNotCrossOrganizations(t *testing.T) {
	store := twoTenantStore()
	store.teams["user-a"] = []domain.TeamID{"team-1"}
	store.teamTenant["team-1"] = tenantA
	store.teamGrants["team-1"] = []domain.ProjectGrant{
		grant("project-a", domain.GrantSubjectTeam, "team-1", domain.ProjectRoleMember),
		grant("project-b", domain.GrantSubjectTeam, "team-1", domain.ProjectRoleAdmin),
	}
	svc := New(store)
	a := principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword)

	if err := svc.Authorize(context.Background(), a, domain.PermWorkflowRun, domain.ProjectResource("project-a")); err != nil {
		t.Fatalf("the team grant inside its own organization was denied: %v", err)
	}
	if err := svc.Authorize(context.Background(), a, domain.PermProjectRead, domain.ProjectResource("project-b")); err == nil {
		t.Fatal("a team grant reached a project in another organization")
	}

	// Now move the team into organization B while its grant on project-a
	// stays. The grant is on a project the caller can still SEE (they are in
	// organization A), but the team no longer lives there, so it confers
	// nothing.
	store.teamTenant["team-1"] = tenantB
	if err := svc.Authorize(context.Background(), a, domain.PermWorkflowRun, domain.ProjectResource("project-a")); err == nil {
		t.Fatal("a team that moved organizations kept conferring its old grants")
	}
}

// Owning a project row is an administrator grant on it -- but only inside an
// organization the owner belongs to. An administrator who moves a project to
// another organization takes it away from its original owner, which is the
// entire point of being able to move one.
func TestProjectOwnershipDoesNotSurviveLeavingTheOrganization(t *testing.T) {
	store := twoTenantStore()
	store.owned["user-a"] = []domain.ProjectID{"project-a"}
	svc := New(store)
	a := principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword)

	if err := svc.Authorize(context.Background(), a, domain.PermProjectManage, domain.ProjectResource("project-a")); err != nil {
		t.Fatalf("the owner was denied their own project: %v", err)
	}
	store.putProject("project-a", tenantB)
	if err := svc.Authorize(context.Background(), a, domain.PermProjectRead, domain.ProjectResource("project-a")); err == nil {
		t.Fatal("a project moved to another organization stayed visible to its former owner")
	}
}

// An organization's owner or administrator reaches every project inside it
// without a grant per project, and nothing outside it.
func TestTenantAdminReachesItsOwnOrganizationOnly(t *testing.T) {
	store := twoTenantStore()
	store.putProject("project-a2", tenantA)
	store.tenantsOf["boss"] = []domain.TenantMembership{
		{TenantID: tenantA, UserID: "boss", Role: domain.TenantRoleAdmin},
	}
	svc := New(store)
	boss := principal(user("boss", domain.UserRoleMember), domain.AuthMethodPassword)

	for _, id := range []domain.ProjectID{"project-a", "project-a2"} {
		if err := svc.Authorize(context.Background(), boss, domain.PermProjectManage, domain.ProjectResource(id)); err != nil {
			t.Fatalf("the organization administrator was denied %s: %v", id, err)
		}
	}
	if err := svc.Authorize(context.Background(), boss, domain.PermProjectRead, domain.ProjectResource("project-b")); err == nil {
		t.Fatal("an organization administrator reached another organization's project")
	}
}

// The organization role is a ceiling on any grant held inside it, exactly as
// the installation role is. A tenant viewer granted admin on a project is
// still a viewer there.
func TestTenantViewerCapsEveryGrantInThatOrganization(t *testing.T) {
	store := twoTenantStore()
	store.tenantsOf["reader"] = []domain.TenantMembership{
		{TenantID: tenantA, UserID: "reader", Role: domain.TenantRoleViewer},
	}
	store.userGrants["reader"] = []domain.ProjectGrant{
		grant("project-a", domain.GrantSubjectUser, "reader", domain.ProjectRoleAdmin),
	}
	svc := New(store)
	reader := principal(user("reader", domain.UserRoleMember), domain.AuthMethodPassword)

	if err := svc.Authorize(context.Background(), reader, domain.PermProjectRead, domain.ProjectResource("project-a")); err != nil {
		t.Fatalf("the organization viewer was denied a read: %v", err)
	}
	for _, perm := range []domain.Permission{
		domain.PermProjectManage, domain.PermWorkflowRun,
		domain.PermSessionWrite, domain.PermProjectAccessManage,
	} {
		if err := svc.Authorize(context.Background(), reader, perm, domain.ProjectResource("project-a")); err == nil {
			t.Fatalf("an organization viewer was allowed to %s", perm)
		}
	}
}

// The installation's owner and administrators are cross-tenant by design: they
// administer the accounts that belong to every organization, and hiding an
// organization from them would be theatre. This is also what keeps the
// single-user installation -- one account, which is the owner -- behaving
// exactly as it did before P4-C.
func TestInstallationAdministratorsAreCrossTenant(t *testing.T) {
	store := twoTenantStore()
	svc := New(store)

	for _, role := range []domain.UserRole{domain.UserRoleOwner, domain.UserRoleAdmin} {
		p := principal(user("boss", role), domain.AuthMethodTrustedLocal)
		sub, err := svc.Resolve(context.Background(), p)
		if err != nil {
			t.Fatalf("resolve %s: %v", role, err)
		}
		if !sub.CrossTenant {
			t.Fatalf("the installation %s did not resolve as cross-tenant", role)
		}
		for _, id := range []domain.ProjectID{"project-a", "project-b"} {
			if err := svc.Authorize(context.Background(), p, domain.PermProjectManage, domain.ProjectResource(id)); err != nil {
				t.Fatalf("the installation %s was denied %s: %v", role, id, err)
			}
		}
		for _, tenant := range []domain.TenantID{tenantA, tenantB, "an-organization-nobody-mentioned"} {
			if err := svc.Authorize(context.Background(), p, domain.PermTenantManage, domain.TenantResource(tenant)); err != nil {
				t.Fatalf("the installation %s was denied tenant.manage on %s: %v", role, tenant, err)
			}
		}
	}
}

// Tenant-scope permissions follow membership, and an organization the caller
// does not belong to answers no to all of them.
func TestTenantPermissionsFollowMembership(t *testing.T) {
	store := twoTenantStore()
	store.tenantsOf["boss-a"] = []domain.TenantMembership{
		{TenantID: tenantA, UserID: "boss-a", Role: domain.TenantRoleAdmin},
	}
	svc := New(store)

	cases := []struct {
		name  string
		who   domain.Principal
		perm  domain.Permission
		where domain.TenantID
		allow bool
	}{
		{"a member reads its own organization", principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword), domain.PermTenantRead, tenantA, true},
		{"a member sees who else is in it", principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword), domain.PermTenantMembersRead, tenantA, true},
		{"a member cannot rename it", principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword), domain.PermTenantManage, tenantA, false},
		{"a member cannot re-role anyone", principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword), domain.PermTenantMembersManage, tenantA, false},
		{"a member cannot even read another organization", principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword), domain.PermTenantRead, tenantB, false},
		{"an organization admin renames its own", principal(user("boss-a", domain.UserRoleMember), domain.AuthMethodPassword), domain.PermTenantManage, tenantA, true},
		{"an organization admin manages its membership", principal(user("boss-a", domain.UserRoleMember), domain.AuthMethodPassword), domain.PermTenantMembersManage, tenantA, true},
		{"an organization admin is nobody elsewhere", principal(user("boss-a", domain.UserRoleMember), domain.AuthMethodPassword), domain.PermTenantRead, tenantB, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.Authorize(context.Background(), tc.who, tc.perm, domain.TenantResource(tc.where))
			if tc.allow && err != nil {
				t.Fatalf("denied: %v", err)
			}
			if !tc.allow && err == nil {
				t.Fatal("allowed, want denied")
			}
		})
	}
}

// A tenant-scope permission asked without naming an organization must deny.
// "Yes" is the dangerous answer to a malformed question, and this is the shape
// a caller that forgot to pass the resource would produce.
func TestTenantPermissionWithoutATenantDenies(t *testing.T) {
	svc := New(twoTenantStore())
	a := principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword)

	if err := svc.Authorize(context.Background(), a, domain.PermTenantRead, domain.GlobalResource()); err == nil {
		t.Fatal("a tenant permission was granted against the global resource")
	}
	if err := svc.Authorize(context.Background(), a, domain.PermTenantRead, domain.TenantResource("")); err == nil {
		t.Fatal("a tenant permission was granted against an empty organization id")
	}
}

// An account in no organization at all sees nothing, rather than everything.
// This is the failure mode a "no scope means no filter" implementation has,
// and the one the resolver's absence-is-denial shape rules out.
func TestAnAccountInNoOrganizationSeesNothing(t *testing.T) {
	store := twoTenantStore()
	store.tenantsOf["drifter"] = []domain.TenantMembership{}
	store.owned["drifter"] = []domain.ProjectID{"project-a"}
	store.userGrants["drifter"] = []domain.ProjectGrant{
		grant("project-b", domain.GrantSubjectUser, "drifter", domain.ProjectRoleAdmin),
	}
	svc := New(store)
	drifter := principal(user("drifter", domain.UserRoleMember), domain.AuthMethodPassword)

	sub, err := svc.Resolve(context.Background(), drifter)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(sub.AccessibleProjectIDs()) != 0 {
		t.Fatalf("an account in no organization reached %v", sub.AccessibleProjectIDs())
	}
	for _, id := range []domain.ProjectID{"project-a", "project-b"} {
		if err := svc.Authorize(context.Background(), drifter, domain.PermProjectRead, domain.ProjectResource(id)); err == nil {
			t.Fatalf("an account in no organization read %s", id)
		}
	}
}

// The authority an account carries is the same whichever way it signed in.
// P4-A resolved the identity once, at the edge; organization membership hangs
// off the resolved account, so a password login, an SSO login and a
// trusted-local desktop request must agree exactly.
func TestOrganizationReachIsIndependentOfLoginMethod(t *testing.T) {
	store := twoTenantStore()
	store.owned["user-a"] = []domain.ProjectID{"project-a"}
	svc := New(store)

	for _, method := range []domain.AuthMethod{
		domain.AuthMethodPassword,
		domain.AuthMethodOIDC,
		domain.AuthMethodTrustedLocal,
	} {
		p := principal(user("user-a", domain.UserRoleMember), method)
		if err := svc.Authorize(context.Background(), p, domain.PermProjectRead, domain.ProjectResource("project-a")); err != nil {
			t.Fatalf("%s login was denied its own organization's project: %v", method, err)
		}
		if err := svc.Authorize(context.Background(), p, domain.PermProjectRead, domain.ProjectResource("project-b")); err == nil {
			t.Fatalf("%s login reached another organization's project", method)
		}
	}
}

// A store failure while resolving organizations must deny, not allow. It is
// the same rule the rest of the resolver follows, restated for the read P4-C
// added, because a lookup that cannot complete is the moment a scoped system
// is most tempted to fall back to "unscoped".
func TestTenantLookupFailureDeniesRatherThanAllows(t *testing.T) {
	store := twoTenantStore()
	store.err = errors.New("database is gone")
	svc := New(store)
	a := principal(user("user-a", domain.UserRoleMember), domain.AuthMethodPassword)

	if err := svc.Authorize(context.Background(), a, domain.PermProjectRead, domain.ProjectResource("project-a")); err == nil {
		t.Fatal("an unreadable organization table allowed access")
	}
	if err := svc.Authorize(context.Background(), a, domain.PermTenantRead, domain.TenantResource(tenantA)); err == nil {
		t.Fatal("an unreadable organization table allowed a tenant read")
	}
}
