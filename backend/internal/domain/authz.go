package domain

import "time"

// Permission is a stable identifier for one thing a principal may do. It is
// deliberately a string constant rather than a bit in a flags word or a
// boolean on User: permissions are persisted in audit records and returned to
// the frontend as capabilities, and both of those are contracts that outlive
// any particular role table.
//
// Every permission below corresponds to a product surface that exists TODAY.
// There is no permission here for a feature AO does not ship -- an unenforced
// permission is a false promise to whoever reads the list.
type Permission string

const (
	// PermProjectRead is "see this project and its board at all".
	PermProjectRead Permission = "project.read"
	// PermProjectManage is "change this project's settings, or remove it".
	PermProjectManage Permission = "project.manage"
	// PermProjectCreate is global, not project-scoped: registering, cloning
	// or importing a NEW project. There is no project to be scoped to yet.
	PermProjectCreate Permission = "project.create"
	// PermProjectAccessRead is "see who can reach this project".
	PermProjectAccessRead Permission = "project.access.read"
	// PermProjectAccessManage is "grant or revoke access to this project".
	PermProjectAccessManage Permission = "project.access.manage"

	// PermWorkflowRead is "see runs, boards, plans, questions, usage".
	PermWorkflowRead Permission = "workflow.read"
	// PermWorkflowRun is "start a run, continue it, answer its questions".
	PermWorkflowRun Permission = "workflow.run"
	// PermWorkflowCancel is "cancel or archive a run".
	PermWorkflowCancel Permission = "workflow.cancel"
	// PermWorkflowRepair is "drive the recovery/repair controls".
	PermWorkflowRepair Permission = "workflow.repair"

	// PermSessionRead is "read a session's content".
	PermSessionRead Permission = "session.read"
	// PermSessionWrite is "send input to, control, or kill a session".
	PermSessionWrite Permission = "session.write"

	// PermMemoryRead is "read project memory".
	PermMemoryRead Permission = "memory.read"
	// PermUsageRead is "read usage and cost".
	PermUsageRead Permission = "usage.read"

	// PermProviderRead is "see configured provider profiles".
	PermProviderRead Permission = "provider.read"
	// PermProviderManage is "create, edit, install or delete provider profiles".
	PermProviderManage Permission = "provider.manage"

	// PermSettingsRead is "read installation settings and routing policy".
	PermSettingsRead Permission = "settings.read"
	// PermSettingsManage is "change installation settings and routing policy".
	PermSettingsManage Permission = "settings.manage"

	// PermUsersRead is "list the installation's accounts".
	PermUsersRead Permission = "users.read"
	// PermUsersManage is "create accounts, change roles, disable accounts".
	PermUsersManage Permission = "users.manage"

	// PermTeamsRead is "list teams and their members".
	PermTeamsRead Permission = "teams.read"
	// PermTeamsManage is "create, rename, archive teams and edit membership".
	PermTeamsManage Permission = "teams.manage"

	// PermAuditRead is "read the authorization/identity audit trail".
	//
	// The trail itself is real -- service/rbac and httpd/controllers.AuthAudit
	// both emit to it -- but AO has no route that READS it back yet, so this
	// permission gates nothing today. It is declared rather than deferred
	// because the audit reader is the surface it will gate, and adding the
	// permission alongside that route later would mean deciding the role tables
	// twice. Only the owner and an administrator hold it, which is the answer
	// that will still be right when the reader exists.
	PermAuditRead Permission = "audit.read"
)

// AllPermissions is every permission AO enforces, in a stable order. Used by
// the capability response and by the tests that assert the role tables are
// total (every permission is decided by every role, rather than defaulting).
var AllPermissions = []Permission{
	PermProjectRead, PermProjectManage, PermProjectCreate,
	PermProjectAccessRead, PermProjectAccessManage,
	PermWorkflowRead, PermWorkflowRun, PermWorkflowCancel, PermWorkflowRepair,
	PermSessionRead, PermSessionWrite,
	PermMemoryRead, PermUsageRead,
	PermProviderRead, PermProviderManage,
	PermSettingsRead, PermSettingsManage,
	PermUsersRead, PermUsersManage,
	PermTeamsRead, PermTeamsManage,
	PermAuditRead,
}

// AuthzScope is the kind of resource a permission is evaluated against.
//
// P4-C adds AuthzScopeOrganization here and one more case to the resolver.
// That is the whole reason authorization takes a scope at all rather than a
// bare project id: a third scope must be a new case, not a rewrite.
type AuthzScope string

const (
	// AuthzScopeGlobal is the installation itself: users, teams, settings,
	// providers, the audit trail.
	AuthzScopeGlobal AuthzScope = "global"
	// AuthzScopeProject is one project and everything under it -- its
	// workflows, runs, sessions, memory and usage.
	AuthzScopeProject AuthzScope = "project"
)

// ScopeOf reports which scope a permission is evaluated in. A permission
// belongs to exactly one scope: asking "may they read THE settings of project
// X" when settings are installation-wide is a question with no honest answer,
// so the type system is not asked to express it -- this function is.
func ScopeOf(p Permission) AuthzScope {
	switch p {
	case PermProjectRead, PermProjectManage,
		PermProjectAccessRead, PermProjectAccessManage,
		PermWorkflowRead, PermWorkflowRun, PermWorkflowCancel, PermWorkflowRepair,
		PermSessionRead, PermSessionWrite,
		PermMemoryRead, PermUsageRead:
		return AuthzScopeProject
	default:
		return AuthzScopeGlobal
	}
}

// AuthzResource names what a permission is being checked against. The zero
// value is the global scope, which is what every installation-wide check
// passes.
type AuthzResource struct {
	Scope AuthzScope
	// Project is set when Scope is AuthzScopeProject.
	Project ProjectID
}

// GlobalResource is the installation itself.
func GlobalResource() AuthzResource { return AuthzResource{Scope: AuthzScopeGlobal} }

// ProjectResource is one project.
func ProjectResource(id ProjectID) AuthzResource {
	return AuthzResource{Scope: AuthzScopeProject, Project: id}
}

// Additional UserRole values introduced by P4-B. UserRoleOwner and
// UserRoleMember are unchanged from 8P-E.8 and keep their exact persisted
// spelling; nothing that already exists is renamed or removed.
const (
	// UserRoleAdmin is a delegated installation administrator: everything the
	// owner can do EXCEPT transferring ownership and touching the owner's own
	// account. That exception is the whole point of the role -- an
	// installation that can be taken over by any administrator has no owner.
	UserRoleAdmin UserRole = "admin"
	// UserRoleViewer is a global read-only account. It still needs project
	// access to see any project; the role only caps what it may do with the
	// access it has.
	UserRoleViewer UserRole = "viewer"
)

// ValidUserRole reports whether r is a role this build persists.
func ValidUserRole(r UserRole) bool {
	switch r {
	case UserRoleOwner, UserRoleAdmin, UserRoleMember, UserRoleViewer:
		return true
	default:
		return false
	}
}

// ProjectRole is the authority a subject holds WITHIN one project. It is a
// separate type from UserRole on purpose: "owner" is an installation-wide
// singleton and has no meaning inside a project, and letting the two share a
// type is how a project grant ends up claiming installation ownership.
type ProjectRole string

const (
	// ProjectRoleAdmin can manage the project and its access list.
	ProjectRoleAdmin ProjectRole = "admin"
	// ProjectRoleMember can run, cancel and repair work in the project.
	ProjectRoleMember ProjectRole = "member"
	// ProjectRoleViewer can read the project and everything under it.
	ProjectRoleViewer ProjectRole = "viewer"
)

// ValidProjectRole reports whether r is a project role this build persists.
func ValidProjectRole(r ProjectRole) bool {
	switch r {
	case ProjectRoleAdmin, ProjectRoleMember, ProjectRoleViewer:
		return true
	default:
		return false
	}
}

// Rank orders project roles so two sources of access can be combined without
// a table of pairwise special cases: several grants for the same subject
// combine by MAX (the most generous grant wins, which is what a person means
// by "I also added them to a team"), and a global read-only role caps the
// result by MIN.
func (r ProjectRole) Rank() int {
	switch r {
	case ProjectRoleAdmin:
		return 3
	case ProjectRoleMember:
		return 2
	case ProjectRoleViewer:
		return 1
	default:
		return 0
	}
}

// GrantSubjectKind distinguishes the two things a project grant can be made
// to.
type GrantSubjectKind string

const (
	// GrantSubjectUser grants one account access directly.
	GrantSubjectUser GrantSubjectKind = "user"
	// GrantSubjectTeam grants every member of a team access.
	GrantSubjectTeam GrantSubjectKind = "team"
)

// TeamID identifies a team.
type TeamID string

// TeamStatus is a team's lifecycle state. A team is archived rather than
// deleted when it still holds project grants, so that revoking access is a
// deliberate act rather than a side effect of tidying up a list.
type TeamStatus string

const (
	// TeamStatusActive is a normal team.
	TeamStatusActive TeamStatus = "active"
	// TeamStatusArchived is a team kept for history. Its grants no longer
	// confer access.
	TeamStatusArchived TeamStatus = "archived"
)

// Team is a durable group of users that can hold project access as one
// subject.
type Team struct {
	ID          TeamID
	Name        string
	Slug        string
	Description string
	Status      TeamStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TeamRole is a member's standing within a team. P4-B records it but gates
// every team mutation on the installation-wide PermTeamsManage; delegating
// team administration to a maintainer is the extension point this field
// exists for, not a behavior this slice ships.
type TeamRole string

const (
	// TeamRoleMaintainer is a member who is intended to administer the team.
	TeamRoleMaintainer TeamRole = "maintainer"
	// TeamRoleMember is an ordinary member.
	TeamRoleMember TeamRole = "member"
)

// ValidTeamRole reports whether r is a team role this build persists.
func ValidTeamRole(r TeamRole) bool {
	return r == TeamRoleMaintainer || r == TeamRoleMember
}

// TeamMembership is one user's place in one team.
type TeamMembership struct {
	ID        string
	TeamID    TeamID
	UserID    UserID
	Role      TeamRole
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ProjectGrant is one subject's access to one project.
type ProjectGrant struct {
	ID          string
	ProjectID   ProjectID
	SubjectKind GrantSubjectKind
	SubjectID   string
	Role        ProjectRole
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CreatedBy   UserID
}
