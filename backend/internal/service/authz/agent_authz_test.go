package authz

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func agentPrincipal(u domain.User, project domain.ProjectID, perms ...domain.Permission) domain.Principal {
	if len(perms) == 0 {
		perms = domain.AgentRoleCeiling(domain.AgentRoleReviewer)
	}
	authority := domain.AgentAuthority{
		Role:          domain.AgentRoleReviewer,
		ProjectID:     project,
		SessionID:     "agent-orchestrator-59",
		WorkflowRunID: "wf-98ab416c",
		Permissions:   perms,
	}
	return domain.Principal{User: u, AuthMethod: domain.AuthMethodAgent, Agent: &authority}
}

// An agent's authority is the INTERSECTION of its own grant and the account's.
// This is the case that makes the mechanism safe rather than merely convenient:
// the OWNER of the installation, who may do everything everywhere, is confined
// by their reviewer's credential to one project and to session read/write.
func TestAgentActingForAnOwnerIsStillConfinedToItsLaunch(t *testing.T) {
	store := newFakeStore()
	store.putProject("proj-1", domain.DefaultTenantID)
	store.putProject("proj-2", domain.DefaultTenantID)
	svc := New(store)
	owner := user("user-owner", domain.UserRoleOwner)
	ctx := context.Background()

	// The person may do all of this.
	for _, tc := range []struct {
		perm domain.Permission
		res  domain.AuthzResource
	}{
		{domain.PermUsersManage, domain.GlobalResource()},
		{domain.PermSettingsManage, domain.GlobalResource()},
		{domain.PermSessionWrite, domain.ProjectResource("proj-2")},
		{domain.PermWorkflowCancel, domain.ProjectResource("proj-1")},
	} {
		if err := svc.Authorize(ctx, principal(owner, domain.AuthMethodOIDC), tc.perm, tc.res); err != nil {
			t.Fatalf("the owner may not %q: %v", tc.perm, err)
		}
		// Their agent may not.
		if err := svc.Authorize(ctx, agentPrincipal(owner, "proj-1"), tc.perm, tc.res); err == nil {
			t.Fatalf("the owner's agent inherited %q on %v", tc.perm, tc.res.Scope)
		}
	}

	// What it MAY do is exactly what it was launched to do.
	for _, perm := range []domain.Permission{domain.PermSessionRead, domain.PermSessionWrite, domain.PermWorkflowRead} {
		if err := svc.Authorize(ctx, agentPrincipal(owner, "proj-1"), perm, domain.ProjectResource("proj-1")); err != nil {
			t.Fatalf("a reviewer may not %q in its own project: %v", perm, err)
		}
	}
}

// The intersection runs in BOTH directions: a credential cannot grant what the
// account behind it does not have. An account whose project access is revoked
// takes its agents with it, immediately.
func TestAgentCannotExceedTheAccountItActsFor(t *testing.T) {
	store := newFakeStore()
	store.putProject("proj-1", domain.DefaultTenantID)
	svc := New(store)
	// A viewer: global read-only, so session.write is not theirs to give.
	viewer := user("user-viewer", domain.UserRoleViewer)
	store.userGrants[viewer.ID] = []domain.ProjectGrant{
		grant("proj-1", domain.GrantSubjectUser, string(viewer.ID), domain.ProjectRoleAdmin),
	}
	ctx := context.Background()

	if err := svc.Authorize(ctx, principal(viewer, domain.AuthMethodOIDC), domain.PermSessionWrite, domain.ProjectResource("proj-1")); err == nil {
		t.Fatalf("the viewer may write to a session; this test's premise is wrong")
	}
	if err := svc.Authorize(ctx, agentPrincipal(viewer, "proj-1"), domain.PermSessionWrite, domain.ProjectResource("proj-1")); err == nil {
		t.Fatalf("an agent held a permission its account does not have")
	}
	// And a credential naming a project the account cannot reach grants nothing.
	unreachable := user("user-stranger", domain.UserRoleMember)
	if err := svc.Authorize(ctx, agentPrincipal(unreachable, "proj-1"), domain.PermSessionRead, domain.ProjectResource("proj-1")); err == nil {
		t.Fatalf("an agent reached a project its account has no grant on")
	}
}

// A narrower credential is honoured: asking for less means holding less.
func TestANarrowerAgentCredentialHoldsLess(t *testing.T) {
	store := newFakeStore()
	store.putProject("proj-1", domain.DefaultTenantID)
	svc := New(store)
	owner := user("user-owner", domain.UserRoleOwner)
	readOnly := agentPrincipal(owner, "proj-1", domain.PermSessionRead)
	ctx := context.Background()

	if err := svc.Authorize(ctx, readOnly, domain.PermSessionRead, domain.ProjectResource("proj-1")); err != nil {
		t.Fatalf("a read-only agent may not read: %v", err)
	}
	if err := svc.Authorize(ctx, readOnly, domain.PermSessionWrite, domain.ProjectResource("proj-1")); err == nil {
		t.Fatalf("a read-only agent wrote")
	}
}

// A human principal carries no agent authority, so every pre-P4-I decision is
// byte-for-byte what it was. The capability lists a human sees must not shrink.
func TestHumanPrincipalsAreUnaffectedByTheAgentCap(t *testing.T) {
	store := newFakeStore()
	store.putProject("proj-1", domain.DefaultTenantID)
	svc := New(store)
	owner := user("user-owner", domain.UserRoleOwner)

	sub, err := svc.Resolve(context.Background(), principal(owner, domain.AuthMethodOIDC))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sub.Agent != nil {
		t.Fatalf("a human subject carries an agent authority")
	}
	if len(sub.GlobalPermissions()) == 0 {
		t.Fatalf("the owner lost every global permission")
	}
	if len(sub.ProjectPermissions("proj-1")) == 0 {
		t.Fatalf("the owner lost every project permission")
	}

	// And the agent's own capability list is the narrow one.
	agentSub, err := svc.Resolve(context.Background(), agentPrincipal(owner, "proj-1"))
	if err != nil {
		t.Fatalf("Resolve(agent): %v", err)
	}
	if len(agentSub.GlobalPermissions()) != 0 {
		t.Fatalf("an agent reports global permissions: %v", agentSub.GlobalPermissions())
	}
	if len(agentSub.ProjectPermissions("proj-2")) != 0 {
		t.Fatalf("an agent reports permissions on a project it was not launched for")
	}
}
