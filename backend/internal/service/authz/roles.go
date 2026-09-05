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
		domain.PermTenantCreate,
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
		// Founding an organization is an installation-level act: it creates a
		// boundary the installation's administrators are responsible for. A
		// member who could mint one could mint itself an unsupervised corner.
		domain.PermTenantCreate,
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
		// A viewer sees the external connection and the links, and changes
		// neither. Seeing that a run tracks PROJ-123 is part of reading the
		// project's work.
		domain.PermWorkItemsRead,
	),
	domain.ProjectRoleMember: permSet(
		domain.PermProjectRead,
		domain.PermWorkflowRead, domain.PermWorkflowRun,
		domain.PermWorkflowCancel, domain.PermWorkflowRepair,
		domain.PermSessionRead, domain.PermSessionWrite,
		domain.PermMemoryRead,
		domain.PermUsageRead,
		// A member links the work they are doing to the item that tracks it,
		// but does not decide which external organization this project is
		// connected to or hold its credential.
		domain.PermWorkItemsRead, domain.PermWorkItemsLink,
	),
	domain.ProjectRoleAdmin: permSet(
		domain.PermProjectRead, domain.PermProjectManage,
		domain.PermProjectAccessRead, domain.PermProjectAccessManage,
		domain.PermWorkflowRead, domain.PermWorkflowRun,
		domain.PermWorkflowCancel, domain.PermWorkflowRepair,
		domain.PermSessionRead, domain.PermSessionWrite,
		domain.PermMemoryRead,
		domain.PermUsageRead,
		domain.PermWorkItemsRead, domain.PermWorkItemsLink, domain.PermWorkItemsManage,
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

// tenantRolePermissions maps a role held WITHIN an organization to the
// tenant-scope permissions it grants there. Note what is NOT in this table:
// nothing about projects. Belonging to an organization and being able to reach
// one of its repositories are deliberately two questions, so that an
// organization can have members who see only the projects they were given --
// which is the ordinary case, not an edge case.
var tenantRolePermissions = map[domain.TenantRole]map[domain.Permission]bool{
	// The owner and the administrator hold the same tenant permissions. The
	// difference between them is not a permission: it is the last-owner rule
	// in service/tenants, which refuses to let an administrator demote or
	// remove the owner. Expressing that as a missing permission would be
	// wrong -- an administrator genuinely does manage the membership.
	domain.TenantRoleOwner: permSet(
		domain.PermTenantRead, domain.PermTenantManage,
		domain.PermTenantMembersRead, domain.PermTenantMembersManage,
	),
	domain.TenantRoleAdmin: permSet(
		domain.PermTenantRead, domain.PermTenantManage,
		domain.PermTenantMembersRead, domain.PermTenantMembersManage,
	),
	// A member sees the organization it belongs to and who else is in it --
	// it has to, to ask a colleague for access to a project -- and changes
	// neither.
	domain.TenantRoleMember: permSet(
		domain.PermTenantRead,
		domain.PermTenantMembersRead,
	),
	// A viewer sees that the organization exists. Not its membership: a
	// read-only account has no one to ask and no reason to enumerate people.
	domain.TenantRoleViewer: permSet(
		domain.PermTenantRead,
	),
}

// crossTenantRole is the tenant role an installation-wide role holds in EVERY
// organization. Only the installation's owner and administrators have one, and
// each holds the matching tenant role: the person who administers the accounts
// in an organization cannot be locked out of the organization itself.
//
// This is also what keeps the single-user installation identical to what it
// was before P4-C. Its one account is the installation owner, so it owns every
// organization there will ever be, and nothing it could do yesterday is
// refused today.
func crossTenantRole(role domain.UserRole) domain.TenantRole {
	switch role {
	case domain.UserRoleOwner:
		return domain.TenantRoleOwner
	case domain.UserRoleAdmin:
		return domain.TenantRoleAdmin
	default:
		return ""
	}
}

// tenantRoleCap is the ceiling an installation role puts on any tenant role.
// An installation viewer added to an organization as its administrator is
// still a viewer there, for the reason projectRoleCap gives: the global role
// is a statement about the person, and a scoped membership cannot promote past
// it. Returning TenantRoleOwner means "no ceiling".
func tenantRoleCap(role domain.UserRole) domain.TenantRole {
	if role == domain.UserRoleViewer {
		return domain.TenantRoleViewer
	}
	return domain.TenantRoleOwner
}

// capTenantRole applies the ceiling.
func capTenantRole(role, ceiling domain.TenantRole) domain.TenantRole {
	if role.Rank() > ceiling.Rank() {
		return ceiling
	}
	return role
}

// maxTenantRole is the more generous of two tenant roles.
func maxTenantRole(a, b domain.TenantRole) domain.TenantRole {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// tenantProjectRoleCap is the ceiling an organization role puts on any project
// role held inside that organization. A tenant viewer is a viewer on every
// project there, whatever grant it holds -- the same rule as the installation
// viewer, applied one scope in. Owner, admin and member impose no ceiling:
// what a member may do in a project is decided entirely by its grant, which is
// what makes per-project access meaningful within an organization.
func tenantProjectRoleCap(role domain.TenantRole) domain.ProjectRole {
	if role == domain.TenantRoleViewer {
		return domain.ProjectRoleViewer
	}
	return domain.ProjectRoleAdmin
}

func permSet(perms ...domain.Permission) map[domain.Permission]bool {
	out := make(map[domain.Permission]bool, len(perms))
	for _, p := range perms {
		out[p] = true
	}
	return out
}
