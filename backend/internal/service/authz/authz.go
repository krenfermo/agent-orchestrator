// Package authz is AO's single authorization authority. P4-A answered "who
// authenticated?"; this package answers "what may this principal do?", and it
// is the only place in the daemon that answers it.
//
// The contract is one function -- Authorize(ctx, principal, permission,
// resource) -- and two facts it derives from durable state:
//
//   - the principal's installation-wide role, from users.role;
//   - the principal's role WITHIN a project, from the project's owner column,
//     from a direct project grant, and from grants held by the teams they
//     belong to.
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
	// ProjectRoles is the resolved role per project the user can reach.
	// Absent from the map means no access at all.
	ProjectRoles map[domain.ProjectID]domain.ProjectRole
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

// ProjectRole reports the subject's effective role in one project.
func (s Subject) ProjectRole(id domain.ProjectID) (domain.ProjectRole, bool) {
	best := s.UniversalProject
	if granted, ok := s.ProjectRoles[id]; ok {
		best = maxProjectRole(best, granted)
	}
	if best == "" {
		return "", false
	}
	return capProjectRole(best, projectRoleCap(s.Role)), true
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
		User:         p.User,
		Method:       p.AuthMethod,
		Role:         p.User.Role,
		ProjectRoles: map[domain.ProjectID]domain.ProjectRole{},
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

	if role, ok := universalProjectRole(sub.Role); ok {
		sub.UniversalProject = role
		// An administrator reaches every project already; loading their grants
		// would change no answer and would scale with the installation.
		return sub, nil
	}

	// Owning a project row (8P-A's owner_user_id, stamped on every project the
	// user registered) is itself an administrator grant on that project. This
	// is the compatibility bridge: every project an existing account created
	// stays fully theirs without a migration inventing grant rows for it.
	owned, err := s.store.ListProjectIDsByOwner(ctx, sub.User.ID)
	if err != nil {
		return Subject{}, fmt.Errorf("list owned projects: %w", err)
	}
	for _, id := range owned {
		sub.ProjectRoles[id] = domain.ProjectRoleAdmin
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
	applyGrants(sub.ProjectRoles, direct)

	if len(teams) > 0 {
		inherited, err := s.store.ListProjectGrantsForTeams(ctx, teams)
		if err != nil {
			return Subject{}, fmt.Errorf("list project grants for teams: %w", err)
		}
		applyGrants(sub.ProjectRoles, inherited)
	}
	return sub, nil
}

func applyGrants(into map[domain.ProjectID]domain.ProjectRole, grants []domain.ProjectGrant) {
	for _, g := range grants {
		if !domain.ValidProjectRole(g.Role) {
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
