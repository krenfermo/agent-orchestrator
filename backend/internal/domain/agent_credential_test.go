package domain_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// An agent's grant is a CEILING, and the ceiling is the whole security argument:
// a reviewer launched for an owner must not be able to manage users, reach a
// second project, or touch an organization, however much the owner may.
func TestAgentAuthorityRefusesEverythingOutsideItsBoundProject(t *testing.T) {
	authority := domain.AgentAuthority{
		Role:        domain.AgentRoleReviewer,
		ProjectID:   "proj-1",
		SessionID:   "agent-orchestrator-59",
		Permissions: domain.AgentRoleCeiling(domain.AgentRoleReviewer),
	}

	if !authority.Allows(domain.PermSessionWrite, domain.ProjectResource("proj-1")) {
		t.Fatalf("a reviewer may not write to the session it was launched for; it could not record a verdict")
	}
	if authority.Allows(domain.PermSessionWrite, domain.ProjectResource("proj-2")) {
		t.Fatalf("a reviewer reached a project it was never launched for")
	}
	// Global and tenant scopes are refused by CONSTRUCTION, not by a missing
	// table entry -- so adding one to the role ceiling by accident could not
	// grant it either.
	for _, perm := range []domain.Permission{
		domain.PermUsersManage, domain.PermSettingsManage, domain.PermProviderManage, domain.PermProjectCreate,
	} {
		if authority.Allows(perm, domain.GlobalResource()) {
			t.Fatalf("an agent holds the installation-wide permission %q", perm)
		}
	}
	if authority.Allows(domain.PermTenantManage, domain.TenantResource("tenant-1")) {
		t.Fatalf("an agent holds a tenant-scoped permission")
	}
	// A project-scoped permission asked with no project is a programming error,
	// and "yes" is the dangerous answer to it.
	if authority.Allows(domain.PermSessionRead, domain.AuthzResource{Scope: domain.AuthzScopeProject}) {
		t.Fatalf("an agent was allowed a project permission with no project named")
	}
}

// A credential that is not bound reaches NOTHING. The empty binding must deny,
// never wave through -- that is the difference between "no restriction recorded"
// and "no restriction".
func TestUnboundAgentAuthorityReachesNothing(t *testing.T) {
	var authority domain.AgentAuthority
	if authority.MayReachSession("agent-orchestrator-59") {
		t.Fatalf("an unbound credential reached a session")
	}
	if authority.MayReachSession("") {
		t.Fatalf("an unbound credential matched an empty session id")
	}
	if authority.MayReachWorkflowRun("wf-1") {
		t.Fatalf("an unbound credential reached a workflow run")
	}
	if authority.Allows(domain.PermSessionRead, domain.ProjectResource("proj-1")) {
		t.Fatalf("an unbound credential allowed a project permission")
	}
}

// The binding is one session and one run, not a prefix and not a set.
func TestAgentAuthorityBindingIsExact(t *testing.T) {
	authority := domain.AgentAuthority{
		ProjectID:     "proj-1",
		SessionID:     "agent-orchestrator-59",
		WorkflowRunID: "wf-98ab416c",
	}
	if !authority.MayReachSession("agent-orchestrator-59") || !authority.MayReachWorkflowRun("wf-98ab416c") {
		t.Fatalf("a credential does not reach its own launch")
	}
	if authority.MayReachSession("agent-orchestrator-5") || authority.MayReachSession("agent-orchestrator-599") {
		t.Fatalf("the session binding matched a different session")
	}
	if authority.MayReachWorkflowRun("wf-98ab416") {
		t.Fatalf("the run binding matched a different run")
	}
}

// CapAgentPermissions may only ever narrow. An unknown role holds nothing --
// a credential for a role this build does not understand must be inert, never
// unbounded.
func TestAgentPermissionsCannotExceedTheRoleCeiling(t *testing.T) {
	capped := domain.CapAgentPermissions(domain.AgentRoleReviewer,
		[]domain.Permission{domain.PermSessionWrite, domain.PermUsersManage, domain.PermProjectManage})
	for _, p := range capped {
		if p == domain.PermUsersManage || p == domain.PermProjectManage {
			t.Fatalf("capping let %q through the reviewer ceiling", p)
		}
	}
	if len(capped) != 1 || capped[0] != domain.PermSessionWrite {
		t.Fatalf("capped = %v, want exactly [session.write]", capped)
	}
	if got := domain.CapAgentPermissions("archivist", nil); len(got) != 0 {
		t.Fatalf("an unknown role holds %v, want nothing", got)
	}
	// Empty means "the whole ceiling", which is what every AO launch asks for.
	if got := domain.CapAgentPermissions(domain.AgentRoleReviewer, nil); len(got) == 0 {
		t.Fatalf("an empty request produced no permissions; a reviewer could not record a verdict")
	}
	// And the ceiling itself stays small: nothing outside the project scope.
	for _, p := range domain.AgentRoleCeiling(domain.AgentRoleReviewer) {
		if domain.ScopeOf(p) != domain.AuthzScopeProject {
			t.Fatalf("the reviewer ceiling contains the non-project permission %q", p)
		}
	}
}

// AgentRoleCeiling hands back a copy: a caller that mutates the slice it is
// given must not be able to widen every future credential in the process.
func TestAgentRoleCeilingIsNotAliased(t *testing.T) {
	first := domain.AgentRoleCeiling(domain.AgentRoleReviewer)
	if len(first) == 0 {
		t.Fatalf("the reviewer ceiling is empty")
	}
	first[0] = domain.PermUsersManage
	second := domain.AgentRoleCeiling(domain.AgentRoleReviewer)
	if second[0] == domain.PermUsersManage {
		t.Fatalf("mutating a returned ceiling widened the role table itself")
	}
}
