// Package authz is AO's single authorization authority. P4-A answered "who
// authenticated?"; this package answers "what may this principal do?", and it
// is the only place in the daemon that answers it.
//
// The contract is one function -- Authorize(ctx, principal, permission,
// resource) -- and two facts it derives from durable state:
//
//   - the principal's installation-wide role, from users.role;
//   - the principal's role WITHIN an organization, from tenant_memberships;
//   - the principal's role WITHIN a project, from the project's owner column,
//     from a direct project grant, and from grants held by the teams they
//     belong to -- each of which counts only inside an organization the
//     principal actually belongs to.
//
// P4-C made the organization the outermost of those, and did it by narrowing
// what the resolver will put in ProjectRoles rather than by adding a check
// somewhere later. A project in an organization the caller does not belong to
// never enters the map, so every question already asked about a project --
// may I read it, may I run in it, may I see its sessions, its notifications,
// its usage, its memory, its code graph -- answers "no" for a foreign
// organization without a single one of those call sites learning that tenants
// exist. That is the whole design: tenancy is enforced once, at resolution,
// and it fails closed because absence from a map is the denial.
//
// Nothing here reads an OIDC claim. P4-A resolved the identity once, at the
// edge, and authorization deliberately consumes only the resolved AO user --
// which is what makes a password login, an SSO login and a trusted-local
// desktop request carry exactly the same authority for the same account.
package authz

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Store is the durable state the evaluator reads. Deliberately read-only:
// authorization never writes, so a bug here can deny or allow but can never
// corrupt. Backed by storage/sqlite/store.Store in production.
type Store interface {
	CountUsers(ctx context.Context) (int64, error)
	CountOwners(ctx context.Context) (int64, error)
	ListProjectIDsByOwner(ctx context.Context, owner domain.UserID) ([]domain.ProjectID, error)
	ListActiveTeamIDsForUser(ctx context.Context, userID domain.UserID) ([]domain.TeamID, error)
	ListProjectGrantsForUser(ctx context.Context, userID domain.UserID) ([]domain.ProjectGrant, error)
	ListProjectGrantsForTeams(ctx context.Context, teams []domain.TeamID) ([]domain.ProjectGrant, error)

	// P4-C. ListActiveTenantMembershipsForUser is the organizations the
	// account belongs to; the two tenancy lookups answer "which organization
	// owns this project / this team" for a bounded set, so the resolver never
	// loads more of the installation than the caller could reach anyway.
	ListActiveTenantMembershipsForUser(ctx context.Context, user domain.UserID) ([]domain.TenantMembership, error)
	ListProjectIDsInTenants(ctx context.Context, tenants []domain.TenantID) (map[domain.ProjectID]domain.TenantID, error)
	ListProjectTenancy(ctx context.Context, ids []domain.ProjectID) (map[domain.ProjectID]domain.TenantID, error)
	ListTeamTenancy(ctx context.Context, ids []domain.TeamID) (map[domain.TeamID]domain.TenantID, error)
}

// Service is the evaluator.
type Service struct {
	store Store
	// claimed latches once the installation has at least one account -- see
	// InstallationUnclaimed.
	claimed atomic.Bool
}

// New builds a Service over the given store.
func New(store Store) *Service { return &Service{store: store} }

// Subject is a principal's fully resolved authority, computed once per
// request. It is what makes §28's performance requirement structural rather
// than a discipline: three bounded queries produce this, and every
// authorization question in the request is then answered from memory.
type Subject struct {
	User domain.User
	// Method is how the request authenticated. Recorded for audit and for the
	// recovery rule below; it never widens or narrows what the account may do.
	Method domain.AuthMethod
	// Role is the effective installation-wide role.
	Role domain.UserRole
	// TeamIDs are the active teams the user belongs to.
	TeamIDs []domain.TeamID
	// TenantRoles is the role held in each active organization the user
	// belongs to. Absent from the map means not a member, which for everyone
	// but an installation administrator means the organization and everything
	// in it is invisible.
	TenantRoles map[domain.TenantID]domain.TenantRole
	// ProjectRoles is the resolved role per project the user can reach.
	// Absent from the map means no access at all. Every id in it has passed
	// the tenant check already -- see resolve.
	ProjectRoles map[domain.ProjectID]domain.ProjectRole
	// ProjectTenants is the organization owning each project in ProjectRoles.
	// Carried on the subject so the tenant ceiling can be applied with no
	// further I/O, and so a caller can name a project's organization without
	// a second query.
	ProjectTenants map[domain.ProjectID]domain.TenantID
	// CrossTenant records that this subject's authority spans every
	// organization, which only an installation owner or administrator has. It
	// is what keeps a single-user desktop install -- and the person who
	// administers a multi-tenant one -- behaving exactly as it did before
	// P4-C.
	CrossTenant bool
	// UniversalProject is the role held on EVERY project, empty when none.
	UniversalProject domain.ProjectRole
	// RecoveredOwner records that the trusted-local recovery rule applied.
	RecoveredOwner bool
}

// ErrUnauthenticated is the 401 an unresolved principal produces.
func ErrUnauthenticated() error {
	return apierr.Unauthorized("NOT_AUTHENTICATED", "authentication required")
}

// ErrForbidden is the 403 a resolved principal without the permission
// produces. The permission is named in the code so an operator reading a log
// or a developer reading a failing test knows which one was missing; nothing
// about the resource's existence is disclosed.
func ErrForbidden(p domain.Permission) error {
	return apierr.Forbidden("FORBIDDEN", "this account is not permitted to "+string(p))
}

// Authorize is the canonical decision. It returns nil when the principal may
// perform permission on resource, an apierr 401 when no principal resolved at
// all, and an apierr 403 otherwise.
//
// Callers that must not disclose a resource's existence (a project the caller
// cannot see) translate the 403 into their surface's own 404 -- see
// httpd/controllers' project and session gates. That translation is a
// presentation decision and deliberately does not live here: the evaluator
// says what is true, the transport decides what to reveal.
func (s *Service) Authorize(ctx context.Context, p domain.Principal, perm domain.Permission, res domain.AuthzResource) error {
	sub, err := s.Resolve(ctx, p)
	if err != nil {
		return err
	}
	if sub.Allows(perm, res) {
		return nil
	}
	return ErrForbidden(perm)
}

// Allows answers a single question from an already-resolved subject, with no
// I/O. Every Authorize call funnels through here, so there is exactly one
// implementation of the rule.
func (s Subject) Allows(perm domain.Permission, res domain.AuthzResource) bool {
	if s.User.ID == "" || s.User.Status != domain.UserStatusActive {
		return false
	}
	scope := domain.ScopeOf(perm)
	if scope == domain.AuthzScopeGlobal {
		return globalRolePermissions[s.Role][perm]
	}
	if scope == domain.AuthzScopeTenant {
		// Same rule as below: a tenant-scope permission asked without a tenant
		// is a programming error, and "yes" is the dangerous answer to it.
		if res.Scope != domain.AuthzScopeTenant || res.Tenant == "" {
			return false
		}
		role, ok := s.TenantRole(res.Tenant)
		if !ok {
			return false
		}
		return tenantRolePermissions[role][perm]
	}
	// A project-scope permission asked without a project is a programming
	// error, and answering "yes" to it would be the dangerous direction.
	if res.Scope != domain.AuthzScopeProject || res.Project == "" {
		return false
	}
	role, ok := s.ProjectRole(res.Project)
	if !ok {
		return false
	}
	return projectRolePermissions[role][perm]
}

// TenantRole reports the subject's effective role in one organization.
//
// An installation owner or administrator holds one in EVERY organization,
// whether or not a membership row says so -- they administer the accounts that
// belong to it, and hiding the organization from them would be theatre. For
// everyone else, membership is the only source, so a tenant they do not belong
// to reports ("", false) and every tenant-scoped permission then denies.
func (s Subject) TenantRole(id domain.TenantID) (domain.TenantRole, bool) {
	if id == "" {
		return "", false
	}
	var best domain.TenantRole
	if s.CrossTenant {
		best = crossTenantRole(s.Role)
	}
	if held, ok := s.TenantRoles[id]; ok {
		best = maxTenantRole(best, held)
	}
	if best == "" {
		return "", false
	}
	return capTenantRole(best, tenantRoleCap(s.Role)), true
}

// TenantIDs lists the organizations the subject belongs to, sorted. It does
// NOT include the organizations an installation administrator can reach
// without belonging to them -- callers that need those enumerate the
// installation's tenants and filter with TenantRole, because "every tenant"
// is not a set this subject carries.
func (s Subject) TenantIDs() []domain.TenantID {
	out := make([]domain.TenantID, 0, len(s.TenantRoles))
	for id := range s.TenantRoles {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ProjectTenant reports which organization owns a project the subject can
// reach. It is false for a project the subject cannot reach, and for any
// project at all when the subject's access is universal rather than
// enumerated.
func (s Subject) ProjectTenant(id domain.ProjectID) (domain.TenantID, bool) {
	t, ok := s.ProjectTenants[id]
	return t, ok
}

// ProjectRole reports the subject's effective role in one project.
//
// Three ceilings apply, in this order, and they only ever narrow: the roles a
// subject has been GRANTED combine by MAX (the most generous grant wins, which
// is what a person means by "I also added them to a team"), then the
// installation role caps the result, then the organization role caps it again.
// A tenant viewer is a viewer on every project in that organization no matter
// what grant it holds, for the same reason an installation viewer is.
func (s Subject) ProjectRole(id domain.ProjectID) (domain.ProjectRole, bool) {
	best := s.UniversalProject
	if granted, ok := s.ProjectRoles[id]; ok {
		best = maxProjectRole(best, granted)
	}
	if best == "" {
		return "", false
	}
	role := capProjectRole(best, projectRoleCap(s.Role))
	// The tenant ceiling applies only where the project's organization is
	// known, which is every project in ProjectRoles. A cross-tenant subject
	// resolves through UniversalProject and has no per-project tenancy to cap
	// against -- by definition, since its authority is not organization-bound.
	if tenant, ok := s.ProjectTenants[id]; ok {
		if tenantRole, held := s.TenantRoles[tenant]; held {
			role = capProjectRole(role, tenantProjectRoleCap(tenantRole))
		}
	}
	return role, true
}

// CanSeeProject is the read gate every project-scoped list and lookup shares.
func (s Subject) CanSeeProject(id domain.ProjectID) bool {
	return s.Allows(domain.PermProjectRead, domain.ProjectResource(id))
}

// GlobalPermissions lists the global-scope permissions the subject holds, in
// AllPermissions order. This is what the frontend receives as capabilities.
func (s Subject) GlobalPermissions() []domain.Permission {
	out := make([]domain.Permission, 0, len(domain.AllPermissions))
	for _, p := range domain.AllPermissions {
		if domain.ScopeOf(p) != domain.AuthzScopeGlobal {
			continue
		}
		if s.Allows(p, domain.GlobalResource()) {
			out = append(out, p)
		}
	}
	return out
}

// ProjectPermissions lists the permissions the subject holds in one project.
func (s Subject) ProjectPermissions(id domain.ProjectID) []domain.Permission {
	out := make([]domain.Permission, 0, len(domain.AllPermissions))
	res := domain.ProjectResource(id)
	for _, p := range domain.AllPermissions {
		if domain.ScopeOf(p) != domain.AuthzScopeProject {
			continue
		}
		if s.Allows(p, res) {
			out = append(out, p)
		}
	}
	return out
}

// Resolve computes (and per-request caches) a principal's authority.
func (s *Service) Resolve(ctx context.Context, p domain.Principal) (Subject, error) {
	if p.User.ID == "" {
		return Subject{}, ErrUnauthenticated()
	}
	if c := cacheFrom(ctx); c != nil {
		return c.get(p.User.ID, func() (Subject, error) { return s.resolve(ctx, p) })
	}
	return s.resolve(ctx, p)
}

func (s *Service) resolve(ctx context.Context, p domain.Principal) (Subject, error) {
	sub := Subject{
		User:           p.User,
		Method:         p.AuthMethod,
		Role:           p.User.Role,
		TenantRoles:    map[domain.TenantID]domain.TenantRole{},
		ProjectRoles:   map[domain.ProjectID]domain.ProjectRole{},
		ProjectTenants: map[domain.ProjectID]domain.TenantID{},
	}
	if !domain.ValidUserRole(sub.Role) {
		// A role this build does not know is not a licence. Falling back to
		// the least-privileged real role keeps a forward-migrated database
		// (or a hand-edited row) from becoming an escalation.
		sub.Role = domain.UserRoleViewer
	}
	if sub.User.Status != domain.UserStatusActive {
		// A disabled account resolves to no authority at all. ResolvePrincipal
		// already refuses to mint a principal for one; this is the second lock
		// on the same door, for the trusted-local synthesis path.
		return sub, nil
	}

	// Trusted-local recovery. The loopback listener is unauthenticated by
	// design (see AGENTS.md), so anyone who can reach it can already reset the
	// admin password; refusing them the owner's authority on an installation
	// that HAS NO OWNER would lock the box without protecting anything. Once
	// an owner exists the rule stops applying, and the local desktop request
	// carries exactly the authority of the account it resolved to -- which is
	// what keeps authority independent of login method (§27).
	if p.AuthMethod == domain.AuthMethodTrustedLocal && sub.Role != domain.UserRoleOwner {
		owners, err := s.store.CountOwners(ctx)
		if err != nil {
			return Subject{}, fmt.Errorf("count owners: %w", err)
		}
		if owners == 0 {
			sub.Role = domain.UserRoleOwner
			sub.RecoveredOwner = true
		}
	}

	// The organizations this account belongs to. Read for EVERY subject,
	// including installation administrators, because "which organizations am
	// I in" is a question the UI asks of everyone -- an administrator's
	// cross-tenant reach is recorded separately, in CrossTenant.
	memberships, err := s.store.ListActiveTenantMembershipsForUser(ctx, sub.User.ID)
	if err != nil {
		return Subject{}, fmt.Errorf("list tenant memberships: %w", err)
	}
	for _, m := range memberships {
		if !domain.ValidTenantRole(m.Role) {
			// A tenant role this build does not know is not a licence, for
			// the same reason an unknown installation role is not.
			continue
		}
		sub.TenantRoles[m.TenantID] = maxTenantRole(sub.TenantRoles[m.TenantID], m.Role)
	}

	if role, ok := universalProjectRole(sub.Role); ok {
		sub.UniversalProject = role
		sub.CrossTenant = true
		// An installation administrator reaches every project in every
		// organization already; loading their grants would change no answer
		// and would scale with the installation rather than with them.
		return sub, nil
	}

	// From here the subject is organization-bound, and everything below is
	// written so that a project can only ever enter ProjectRoles THROUGH
	// ProjectTenants -- which is populated exclusively from organizations the
	// subject belongs to. That ordering is the enforcement point: there is no
	// later check to forget, because a foreign-tenant project has no way in.

	// Organizations this account owns or administers. Every project inside one
	// is theirs to administer without a grant per project, which is what makes
	// "give Ana the run of this organization" one row instead of one per
	// repository. Bounded by the account's own membership, so this is the same
	// cost class as the owned-projects lookup below, not an installation scan.
	var adminTenants []domain.TenantID
	for id, role := range sub.TenantRoles {
		if role == domain.TenantRoleOwner || role == domain.TenantRoleAdmin {
			adminTenants = append(adminTenants, id)
		}
	}
	if len(adminTenants) > 0 {
		sort.Slice(adminTenants, func(i, j int) bool { return adminTenants[i] < adminTenants[j] })
		inTenants, err := s.store.ListProjectIDsInTenants(ctx, adminTenants)
		if err != nil {
			return Subject{}, fmt.Errorf("list projects in administered tenants: %w", err)
		}
		for id, tenant := range inTenants {
			sub.ProjectTenants[id] = tenant
			sub.ProjectRoles[id] = domain.ProjectRoleAdmin
		}
	}

	// Owning a project row (8P-A's owner_user_id, stamped on every project the
	// user registered) is itself an administrator grant on that project. This
	// is the compatibility bridge: every project an existing account created
	// stays fully theirs without a migration inventing grant rows for it.
	owned, err := s.store.ListProjectIDsByOwner(ctx, sub.User.ID)
	if err != nil {
		return Subject{}, fmt.Errorf("list owned projects: %w", err)
	}

	teams, err := s.store.ListActiveTeamIDsForUser(ctx, sub.User.ID)
	if err != nil {
		return Subject{}, fmt.Errorf("list teams for user: %w", err)
	}
	sub.TeamIDs = teams

	direct, err := s.store.ListProjectGrantsForUser(ctx, sub.User.ID)
	if err != nil {
		return Subject{}, fmt.Errorf("list project grants for user: %w", err)
	}

	var inherited []domain.ProjectGrant
	if len(teams) > 0 {
		inherited, err = s.store.ListProjectGrantsForTeams(ctx, teams)
		if err != nil {
			return Subject{}, fmt.Errorf("list project grants for teams: %w", err)
		}
	}

	// Resolve the organization of every project ownership or a grant points
	// at, in one round trip, and admit only those the subject belongs to.
	// Everything a foreign organization owns is dropped here, silently and
	// permanently: it cannot be listed, fetched by a guessed id, or reached
	// through anything that hangs off it.
	candidates := make([]domain.ProjectID, 0, len(owned)+len(direct)+len(inherited))
	seen := make(map[domain.ProjectID]bool, cap(candidates))
	consider := func(id domain.ProjectID) {
		if id == "" || seen[id] {
			return
		}
		if _, known := sub.ProjectTenants[id]; known {
			return
		}
		seen[id] = true
		candidates = append(candidates, id)
	}
	for _, id := range owned {
		consider(id)
	}
	for _, g := range direct {
		consider(g.ProjectID)
	}
	for _, g := range inherited {
		consider(g.ProjectID)
	}
	if len(candidates) > 0 {
		tenancy, err := s.store.ListProjectTenancy(ctx, candidates)
		if err != nil {
			return Subject{}, fmt.Errorf("list project tenancy: %w", err)
		}
		for id, tenant := range tenancy {
			if _, member := sub.TenantRoles[tenant]; !member {
				continue
			}
			sub.ProjectTenants[id] = tenant
		}
	}

	// A team confers access only inside its OWN organization. The write path
	// already refuses to grant a team access to a project in another one; this
	// is the read-side half of that promise, so a team whose organization was
	// changed after its grants were written stops carrying them across
	// immediately rather than at the next write.
	if len(inherited) > 0 {
		teamTenancy, err := s.store.ListTeamTenancy(ctx, teams)
		if err != nil {
			return Subject{}, fmt.Errorf("list team tenancy: %w", err)
		}
		kept := inherited[:0]
		for _, g := range inherited {
			if teamTenancy[domain.TeamID(g.SubjectID)] != sub.ProjectTenants[g.ProjectID] {
				continue
			}
			kept = append(kept, g)
		}
		inherited = kept
	}

	for _, id := range owned {
		if _, inReach := sub.ProjectTenants[id]; !inReach {
			continue
		}
		sub.ProjectRoles[id] = maxProjectRole(sub.ProjectRoles[id], domain.ProjectRoleAdmin)
	}
	applyGrants(sub.ProjectRoles, sub.ProjectTenants, direct)
	applyGrants(sub.ProjectRoles, sub.ProjectTenants, inherited)
	return sub, nil
}

// applyGrants folds grants into the resolved role map, skipping any whose
// project is not in an organization the subject belongs to. The tenancy map is
// a required argument rather than an optional filter precisely so that adding
// a new source of grants later cannot accidentally skip the check.
func applyGrants(
	into map[domain.ProjectID]domain.ProjectRole,
	tenancy map[domain.ProjectID]domain.TenantID,
	grants []domain.ProjectGrant,
) {
	for _, g := range grants {
		if !domain.ValidProjectRole(g.Role) {
			continue
		}
		if _, inReach := tenancy[g.ProjectID]; !inReach {
			continue
		}
		into[g.ProjectID] = maxProjectRole(into[g.ProjectID], g.Role)
	}
}

// InstallationUnclaimed reports whether the installation has no accounts at
// all. Before the first account exists there is nobody to authorize and
// nothing yet worth protecting: a fresh desktop install must reach its
// first-run screen, and every route it touches on the way must keep behaving
// as it did before P4-B. The answer is latched once it turns false, because
// AO never deletes the last account -- so this costs one query per daemon
// lifetime, not one per request.
func (s *Service) InstallationUnclaimed(ctx context.Context) (bool, error) {
	if s.claimed.Load() {
		return false, nil
	}
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		s.claimed.Store(true)
		return false, nil
	}
	return true, nil
}

// VisibleProjectIDs returns the subset of ids the subject may read, preserving
// input order. Callers filtering a list use this instead of asking per id, so
// the cost of a list response stays linear in the list, not in the database.
func (s Subject) VisibleProjectIDs(ids []domain.ProjectID) []domain.ProjectID {
	out := make([]domain.ProjectID, 0, len(ids))
	for _, id := range ids {
		if s.CanSeeProject(id) {
			out = append(out, id)
		}
	}
	return out
}

// AccessibleProjectIDs lists every project the subject has an explicit or
// inherited grant on, sorted for stable output. It reports nothing for an
// administrator, whose access is universal rather than enumerated -- callers
// must check UniversalProject before treating an empty result as "no access".
func (s Subject) AccessibleProjectIDs() []domain.ProjectID {
	out := make([]domain.ProjectID, 0, len(s.ProjectRoles))
	for id := range s.ProjectRoles {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- per-request memoization -------------------------------------------

type cacheKey struct{}

type requestCache struct {
	mu   sync.Mutex
	user domain.UserID
	sub  Subject
	err  error
	done bool
}

// WithCache installs a per-request resolution cache. Without it every
// Authorize call re-reads the same three tables; with it a request resolves
// its subject once no matter how many permissions it checks.
func WithCache(ctx context.Context) context.Context {
	if cacheFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, cacheKey{}, &requestCache{})
}

func cacheFrom(ctx context.Context) *requestCache {
	c, _ := ctx.Value(cacheKey{}).(*requestCache)
	return c
}

func (c *requestCache) get(id domain.UserID, compute func() (Subject, error)) (Subject, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done && c.user == id {
		return c.sub, c.err
	}
	sub, err := compute()
	c.user, c.sub, c.err, c.done = id, sub, err, true
	return sub, err
}
