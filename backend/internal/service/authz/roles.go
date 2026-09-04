package authz

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// The role tables. Everything AO decides about authority is decided here and
// nowhere else: there is no `if user.Role == "admin"` anywhere in a service or
// a controller, by design. A reviewer who wants to know what a viewer may do
// reads one map instead of grepping the tree.

// globalRolePermissions maps an installation-wide role to the global-scope
// permissions it grants. Project-scope permissions are NOT here -- they come
// from the project role tables below, because "may run a workflow" is a
// question about a project, never about the installation.
var globalRolePermissions = map[domain.UserRole]map[domain.Permission]bool{
	domain.UserRoleOwner: permSet(
		domain.PermProjectCreate,
		domain.PermProviderRead, domain.PermProviderManage,
		domain.PermSettingsRead, domain.PermSettingsManage,
		domain.PermUsersRead, domain.PermUsersManage,
		domain.PermTeamsRead, domain.PermTeamsManage,
		domain.PermAuditRead,
	),
	// An administrator holds every global permission the owner does. The
	// difference between the two is not a permission at all -- it is the
	// owner-safety rule in service/rbac: an administrator may not demote,
	// disable or take over the owner's account. Expressing that as a missing
	// permission would be wrong; an administrator genuinely does manage users.
	domain.UserRoleAdmin: permSet(
		domain.PermProjectCreate,
		domain.PermProviderRead, domain.PermProviderManage,
		domain.PermSettingsRead, domain.PermSettingsManage,
		domain.PermUsersRead, domain.PermUsersManage,
		domain.PermTeamsRead, domain.PermTeamsManage,
		domain.PermAuditRead,
	),
	// A member can register projects (becoming their project admin) and read
	// the installation's provider/settings surfaces the app needs to render,
	// but changes nothing installation-wide.
	domain.UserRoleMember: permSet(
		domain.PermProjectCreate,
		domain.PermProviderRead,
		domain.PermSettingsRead,
	),
	// A viewer reads. It cannot even create a project, because a created
	// project would make it that project's administrator -- a read-only
	// account that can mint write authority for itself is not read-only.
	domain.UserRoleViewer: permSet(
		domain.PermProviderRead,
		domain.PermSettingsRead,
	),
}

// projectRolePermissions maps a role held WITHIN a project to what it may do
// there. Read-your-work permissions are cumulative by rank, so the table is
// written as an explicit union rather than an inheritance chain: a reader
// should not have to compose three maps in their head to answer "may a viewer
// cancel a run".
var projectRolePermissions = map[domain.ProjectRole]map[domain.Permission]bool{
	domain.ProjectRoleViewer: permSet(
		domain.PermProjectRead,
		domain.PermWorkflowRead,
		domain.PermSessionRead,
		domain.PermMemoryRead,
		domain.PermUsageRead,
	),
	domain.ProjectRoleMember: permSet(
		domain.PermProjectRead,
		domain.PermWorkflowRead, domain.PermWorkflowRun,
		domain.PermWorkflowCancel, domain.PermWorkflowRepair,
		domain.PermSessionRead, domain.PermSessionWrite,
		domain.PermMemoryRead,
		domain.PermUsageRead,
	),
	domain.ProjectRoleAdmin: permSet(
		domain.PermProjectRead, domain.PermProjectManage,
		domain.PermProjectAccessRead, domain.PermProjectAccessManage,
		domain.PermWorkflowRead, domain.PermWorkflowRun,
		domain.PermWorkflowCancel, domain.PermWorkflowRepair,
		domain.PermSessionRead, domain.PermSessionWrite,
		domain.PermMemoryRead,
		domain.PermUsageRead,
	),
}

// universalProjectRole is the project role a global role holds on EVERY
// project without a grant. Only the installation's administrators have one:
// they are expected to see the whole installation, and hiding a project from
// the person who administers the users who can reach it would be theatre.
//
// A member or a viewer has no universal role -- their access to a project
// comes from a grant, from a team, or from owning the project row. That is
// what makes "project A allowed, project B denied" expressible at all.
func universalProjectRole(role domain.UserRole) (domain.ProjectRole, bool) {
	switch role {
	case domain.UserRoleOwner, domain.UserRoleAdmin:
		return domain.ProjectRoleAdmin, true
	default:
		return "", false
	}
}

// projectRoleCap is the ceiling a global role puts on any project role the
// user is granted. A viewer who is added to a project as an admin is still a
// viewer there: the global role is a statement about the person, and a
// per-project grant cannot promote past it.
//
// Returning ProjectRoleAdmin means "no ceiling" -- admin is the top rank.
func projectRoleCap(role domain.UserRole) domain.ProjectRole {
	if role == domain.UserRoleViewer {
		return domain.ProjectRoleViewer
	}
	return domain.ProjectRoleAdmin
}

// capProjectRole applies the ceiling.
func capProjectRole(role, ceiling domain.ProjectRole) domain.ProjectRole {
	if role.Rank() > ceiling.Rank() {
		return ceiling
	}
	return role
}

// maxProjectRole is the more generous of two roles, used to combine a direct
// grant with the grants a user inherits through teams.
func maxProjectRole(a, b domain.ProjectRole) domain.ProjectRole {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

func permSet(perms ...domain.Permission) map[domain.Permission]bool {
	out := make(map[domain.Permission]bool, len(perms))
	for _, p := range perms {
		out[p] = true
	}
	return out
}
