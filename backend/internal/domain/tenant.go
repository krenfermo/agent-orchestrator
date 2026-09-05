package domain

import "time"

// A tenant (an organization) is the outermost authority in AO's access model
// and the boundary a person cannot see across. It OWNS projects and teams;
// everything else -- sessions, workflow runs, notifications, usage, memory,
// the code graph -- is reached through a project and inherits that project's
// tenancy rather than carrying a second copy of it.
//
// The distinction from Team is load-bearing and worth stating once here so no
// reader has to infer it: a team is a SUBJECT (a set of users that can hold a
// grant), a tenant is an AUTHORITY (the thing that owns what a grant is made
// on). A team lives inside exactly one tenant.

// DefaultTenantID is the organization created by migration 0156 and used by
// every installation that has never asked for a second one. Its id is fixed
// rather than generated so the migration's backfill, the bootstrap path and
// the tests all name the same row without threading an id between them.
const DefaultTenantID TenantID = "tnt_default"

// TenantID identifies a tenant.
type TenantID string

// TenantStatus is a tenant's lifecycle state. Archived rather than deleted,
// for the same reason a team is: a tenant still owns projects, and revoking
// everyone's access to them must be a deliberate act rather than a side
// effect of tidying a list.
type TenantStatus string

const (
	// TenantStatusActive is a normal tenant.
	TenantStatusActive TenantStatus = "active"
	// TenantStatusArchived is a tenant kept for history. Its memberships no
	// longer confer access to anything it owns.
	TenantStatusArchived TenantStatus = "archived"
)

// Tenant is one organization.
type Tenant struct {
	ID          TenantID
	Name        string
	Slug        string
	Description string
	Status      TenantStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TenantRole is the authority a user holds WITHIN one tenant. It is a
// separate type from UserRole and from ProjectRole on purpose. UserRole is a
// statement about the installation ("this person administers this AO"),
// TenantRole about one organization inside it, and ProjectRole about one
// repository inside that. Sharing a type between any two of them is how a
// tenant membership ends up claiming installation ownership.
type TenantRole string

const (
	// TenantRoleOwner is the organization's owner. Everything an admin can
	// do, plus the one thing an admin cannot: change or remove the owner's own
	// membership. An organization any administrator can take over has no
	// owner, which is the same rule UserRoleAdmin obeys at the installation
	// level.
	TenantRoleOwner TenantRole = "owner"
	// TenantRoleAdmin administers the organization: its settings, its
	// membership, its teams, and every project inside it.
	TenantRoleAdmin TenantRole = "admin"
	// TenantRoleMember belongs to the organization and may create projects in
	// it, but reaches an existing project only through ownership, a grant, or
	// a team. This is what makes "project A allowed, project B denied"
	// expressible WITHIN one tenant, exactly as it already is.
	TenantRoleMember TenantRole = "member"
	// TenantRoleViewer is read-only within the organization. Like the
	// installation-wide viewer it still needs project access to see any
	// project; the role only caps what it may do with the access it has.
	TenantRoleViewer TenantRole = "viewer"
)

// ValidTenantRole reports whether r is a tenant role this build persists.
func ValidTenantRole(r TenantRole) bool {
	switch r {
	case TenantRoleOwner, TenantRoleAdmin, TenantRoleMember, TenantRoleViewer:
		return true
	default:
		return false
	}
}

// Rank orders tenant roles so two sources of tenant authority combine by MAX
// without a table of pairwise cases, matching ProjectRole.Rank.
func (r TenantRole) Rank() int {
	switch r {
	case TenantRoleOwner:
		return 4
	case TenantRoleAdmin:
		return 3
	case TenantRoleMember:
		return 2
	case TenantRoleViewer:
		return 1
	default:
		return 0
	}
}

// TenantMembership is one user's place in one tenant.
type TenantMembership struct {
	ID        string
	TenantID  TenantID
	UserID    UserID
	Role      TenantRole
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TenantRoleForUserRole maps an installation role onto the tenant role an
// account holds in the tenant it is placed in. It is the rule migration 0156's
// backfill encodes in SQL, kept here in Go so the bootstrap path that creates
// the first account and the migration that backfills existing ones cannot
// drift apart. Same-named roles map to each other: the mapping preserves every
// existing authority and widens none.
func TenantRoleForUserRole(r UserRole) TenantRole {
	switch r {
	case UserRoleOwner:
		return TenantRoleOwner
	case UserRoleAdmin:
		return TenantRoleAdmin
	case UserRoleMember:
		return TenantRoleMember
	default:
		// Including a role this build does not recognise. An unreadable role
		// is not a licence.
		return TenantRoleViewer
	}
}
