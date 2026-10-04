package rbac

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// P4-C tenant administration. It lives in this package rather than a new one
// because this is already "the write side of the authorization layer", and an
// organization is an authorization object: putting it anywhere else would
// create the second authorization stack P4-C is specifically not supposed to
// build. The read side -- may this principal do X in organization Y -- stays
// in service/authz, unduplicated.
//
// Every method here enforces the invariants that are NOT permission questions.
// Whether the caller may act at all is decided before the call, by Guard.

// ListTenants returns every organization in the installation, active and
// archived. It is deliberately unfiltered: the caller filters by what the
// subject can see, because this service has no opinion about who is asking and
// service/authz is the only thing that does.
func (s *Service) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	tenants, err := s.store.ListTenants(ctx)
	if err != nil {
		return nil, wrap("list tenants", err)
	}
	return tenants, nil
}

// GetTenant returns one organization.
func (s *Service) GetTenant(ctx context.Context, id domain.TenantID) (domain.Tenant, error) {
	t, ok, err := s.store.GetTenantByID(ctx, id)
	if err != nil {
		return domain.Tenant{}, wrap("get tenant", err)
	}
	if !ok {
		return domain.Tenant{}, notFound(CodeTenantNotFound, "organization not found")
	}
	return t, nil
}

// CreateTenant founds an organization and makes the actor its owner in the
// same call.
//
// The two halves are not separable. An organization with no owner is one
// nobody can administer -- not even the person who just created it, since
// tenant permissions come from membership -- so creating one without the
// membership would produce an object that is immediately stuck. An
// installation administrator can still reach it either way, being
// cross-tenant, but that is a recovery path, not the design.
func (s *Service) CreateTenant(ctx context.Context, actor domain.Principal, name, description string) (domain.Tenant, error) {
	name = strings.TrimSpace(name)
	slug := slugify(name)
	if name == "" || slug == "" {
		return domain.Tenant{}, apierr.Invalid(CodeTenantInvalidName,
			"an organization needs a name with at least one letter or digit", nil)
	}
	now := s.at()
	created, err := s.store.InsertTenant(ctx, domain.Tenant{
		ID:          domain.TenantID(newID()),
		Name:        name,
		Slug:        slug,
		Description: strings.TrimSpace(description),
		Status:      domain.TenantStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		if isUniqueConstraintErr(err) {
			return domain.Tenant{}, apierr.Conflict(CodeTenantExists, "an organization with that name already exists", nil)
		}
		return domain.Tenant{}, fmt.Errorf("create tenant: %w", err)
	}
	if actor.User.ID != "" {
		if _, err := s.store.UpsertTenantMembership(ctx, domain.TenantMembership{
			ID:        newID(),
			TenantID:  created.ID,
			UserID:    actor.User.ID,
			Role:      domain.TenantRoleOwner,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			return domain.Tenant{}, fmt.Errorf("make the creator the organization's owner: %w", err)
		}
	}
	s.audit.Record(ctx, Event{
		Name:       EventTenantCreated,
		Actor:      actor,
		TargetKind: "tenant",
		TargetID:   string(created.ID),
		Detail:     map[string]any{"slug": created.Slug},
	})
	return created, nil
}

// UpdateTenant renames an organization and/or replaces its description.
func (s *Service) UpdateTenant(ctx context.Context, actor domain.Principal, id domain.TenantID, name, description string) (domain.Tenant, error) {
	current, err := s.GetTenant(ctx, id)
	if err != nil {
		return domain.Tenant{}, err
	}
	name = strings.TrimSpace(name)
	slug := slugify(name)
	if name == "" || slug == "" {
		return domain.Tenant{}, apierr.Invalid(CodeTenantInvalidName,
			"an organization needs a name with at least one letter or digit", nil)
	}
	ok, err := s.store.UpdateTenant(ctx, id, name, slug, strings.TrimSpace(description), s.at())
	if err != nil {
		if isUniqueConstraintErr(err) {
			return domain.Tenant{}, apierr.Conflict(CodeTenantExists, "an organization with that name already exists", nil)
		}
		return domain.Tenant{}, fmt.Errorf("update tenant: %w", err)
	}
	if !ok {
		return domain.Tenant{}, notFound(CodeTenantNotFound, "organization not found")
	}
	updated, err := s.GetTenant(ctx, id)
	if err != nil {
		return domain.Tenant{}, err
	}
	s.audit.Record(ctx, Event{
		Name:       EventTenantUpdated,
		Actor:      actor,
		TargetKind: "tenant",
		TargetID:   string(id),
		Detail:     map[string]any{"from": current.Name, "to": updated.Name},
	})
	return updated, nil
}

// ArchiveTenant archives or reactivates an organization. Archiving is the only
// form of removal AO offers, for a stronger version of the reason archiving a
// team is: an organization OWNS projects, and deleting the row would either
// orphan them or delete them with it. Archived, its memberships stop conferring
// access at once (the resolver reads only ACTIVE organizations) while every
// project, grant and membership row survives, so reactivating restores exactly
// what was there.
//
// The default organization cannot be archived. Every account is placed in it
// when it is created and every project lands in it unless told otherwise;
// archiving it would leave a working installation with nothing visible in it
// and no obvious way back.
func (s *Service) ArchiveTenant(ctx context.Context, actor domain.Principal, id domain.TenantID, archived bool) (domain.Tenant, error) {
	if _, err := s.GetTenant(ctx, id); err != nil {
		return domain.Tenant{}, err
	}
	if archived && id == domain.DefaultTenantID {
		return domain.Tenant{}, apierr.Conflict(CodeTenantDefaultProtected,
			"the default organization cannot be archived", nil)
	}
	status := domain.TenantStatusActive
	if archived {
		status = domain.TenantStatusArchived
	}
	ok, err := s.store.UpdateTenantStatus(ctx, id, status, s.at())
	if err != nil {
		return domain.Tenant{}, fmt.Errorf("archive tenant: %w", err)
	}
	if !ok {
		return domain.Tenant{}, notFound(CodeTenantNotFound, "organization not found")
	}
	updated, err := s.GetTenant(ctx, id)
	if err != nil {
		return domain.Tenant{}, err
	}
	s.audit.Record(ctx, Event{
		Name:       EventTenantUpdated,
		Actor:      actor,
		TargetKind: "tenant",
		TargetID:   string(id),
		Detail:     map[string]any{"status": string(status)},
	})
	return updated, nil
}

// ListTenantMembers returns an organization's membership rows.
func (s *Service) ListTenantMembers(ctx context.Context, id domain.TenantID) ([]domain.TenantMembership, error) {
	if _, err := s.GetTenant(ctx, id); err != nil {
		return nil, err
	}
	members, err := s.store.ListTenantMembers(ctx, id)
	if err != nil {
		return nil, wrap("list tenant members", err)
	}
	return members, nil
}

// ListTenantsForUser returns every organization membership one account holds.
func (s *Service) ListTenantsForUser(ctx context.Context, user domain.UserID) ([]domain.TenantMembership, error) {
	memberships, err := s.store.ListActiveTenantMembershipsForUser(ctx, user)
	if err != nil {
		return nil, wrap("list tenant memberships for user", err)
	}
	return memberships, nil
}

// AddTenantMember adds an account to an organization, or changes the role it
// holds there.
//
// Owner-safety mirrors the installation rule in users.go exactly: only an
// organization's own owner may change that owner's membership. An organization
// any administrator can quietly demote the owner of has no owner, and the
// takeover would be indistinguishable from ordinary administration in the
// audit trail.
func (s *Service) AddTenantMember(ctx context.Context, actor domain.Principal, tenant domain.TenantID, user domain.UserID, role domain.TenantRole) (domain.TenantMembership, error) {
	if role == "" {
		role = domain.TenantRoleMember
	}
	if !domain.ValidTenantRole(role) {
		return domain.TenantMembership{}, apierr.Invalid(CodeInvalidRole, "unknown organization role", map[string]any{"role": string(role)})
	}
	if _, err := s.GetTenant(ctx, tenant); err != nil {
		return domain.TenantMembership{}, err
	}
	if _, err := s.GetUser(ctx, user); err != nil {
		return domain.TenantMembership{}, err
	}
	if err := s.guardTenantOwnerChange(ctx, actor, tenant, user); err != nil {
		return domain.TenantMembership{}, err
	}
	now := s.at()
	m, err := s.store.UpsertTenantMembership(ctx, domain.TenantMembership{
		ID:        newID(),
		TenantID:  tenant,
		UserID:    user,
		Role:      role,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		return domain.TenantMembership{}, fmt.Errorf("add tenant member: %w", err)
	}
	s.audit.Record(ctx, Event{
		Name:       EventTenantMemberAdded,
		Actor:      actor,
		TargetKind: "tenant",
		TargetID:   string(tenant),
		SubjectID:  string(user),
		Detail:     map[string]any{"tenantRole": string(role)},
	})
	return m, nil
}

// RemoveTenantMember removes an account from an organization. Everything that
// flowed through the membership -- every project in the organization, and
// every grant on one -- stops on the member's very next request: the resolver
// reads membership per request and holds nothing across them.
func (s *Service) RemoveTenantMember(ctx context.Context, actor domain.Principal, tenant domain.TenantID, user domain.UserID) error {
	if _, err := s.GetTenant(ctx, tenant); err != nil {
		return err
	}
	if err := s.guardTenantOwnerChange(ctx, actor, tenant, user); err != nil {
		return err
	}
	if err := s.guardLastTenantOwner(ctx, tenant, user); err != nil {
		return err
	}
	ok, err := s.store.DeleteTenantMembership(ctx, tenant, user)
	if err != nil {
		return fmt.Errorf("remove tenant member: %w", err)
	}
	if !ok {
		return notFound(CodeTenantMembershipNotFound, "that account is not in this organization")
	}
	s.audit.Record(ctx, Event{
		Name:       EventTenantMemberRemoved,
		Actor:      actor,
		TargetKind: "tenant",
		TargetID:   string(tenant),
		SubjectID:  string(user),
	})
	return nil
}

// AssignProjectTenant moves a project into an organization.
//
// This is the most dangerous write in the package and reads like the least:
// it hands a project, its sessions, its runs, its notifications, its usage and
// its memory to a different set of people, and takes them away from the set
// that had them. So it is gated on tenant.manage in BOTH organizations -- the
// one losing the project and the one gaining it -- by the controller, and here
// it refuses an archived destination, which would make the project reachable
// by nobody.
func (s *Service) AssignProjectTenant(ctx context.Context, actor domain.Principal, project domain.ProjectID, tenant domain.TenantID) error {
	target, err := s.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	if target.Status != domain.TenantStatusActive {
		return apierr.Conflict(CodeTenantArchived,
			"a project cannot be moved into an archived organization", nil)
	}
	ok, err := s.store.SetProjectTenant(ctx, project, tenant)
	if err != nil {
		return fmt.Errorf("assign project tenant: %w", err)
	}
	if !ok {
		return notFound("PROJECT_NOT_FOUND", "project not found")
	}
	s.audit.Record(ctx, Event{
		Name:       EventProjectTenantAssigned,
		Actor:      actor,
		TargetKind: "project",
		TargetID:   string(project),
		SubjectID:  string(tenant),
	})
	return nil
}

// AssignTeamTenant moves a team into an organization. A team is a subject that
// holds project grants, so a team whose organization no longer matches a
// project it was granted stops conferring that access immediately -- the
// resolver checks it on every request rather than trusting the grant row.
func (s *Service) AssignTeamTenant(ctx context.Context, actor domain.Principal, team domain.TeamID, tenant domain.TenantID) error {
	target, err := s.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	if target.Status != domain.TenantStatusActive {
		return apierr.Conflict(CodeTenantArchived,
			"a team cannot be moved into an archived organization", nil)
	}
	if _, err := s.GetTeam(ctx, team); err != nil {
		return err
	}
	ok, err := s.store.SetTeamTenant(ctx, team, tenant)
	if err != nil {
		return fmt.Errorf("assign team tenant: %w", err)
	}
	if !ok {
		return notFound(CodeTeamNotFound, "team not found")
	}
	s.audit.Record(ctx, Event{
		Name:       EventTeamTenantAssigned,
		Actor:      actor,
		TargetKind: "team",
		TargetID:   string(team),
		SubjectID:  string(tenant),
	})
	return nil
}

// guardTenantOwnerChange refuses to let anyone but an organization's own owner
// alter that owner's membership.
func (s *Service) guardTenantOwnerChange(ctx context.Context, actor domain.Principal, tenant domain.TenantID, target domain.UserID) error {
	current, ok, err := s.store.GetTenantMembership(ctx, tenant, target)
	if err != nil {
		return wrap("get tenant membership", err)
	}
	if !ok || current.Role != domain.TenantRoleOwner {
		return nil
	}
	if actor.User.ID == target {
		return nil
	}
	return apierr.Forbidden(CodeTenantOwnerProtected,
		"only the organization's owner may change the owner's own membership")
}

// guardLastTenantOwner refuses a removal that would leave an organization with
// no owner at all. The installation's administrators could still reach it,
// being cross-tenant, but the organization would have nobody responsible for
// it -- and "an administrator can fix it" is not a reason to let the UI create
// the situation.
func (s *Service) guardLastTenantOwner(ctx context.Context, tenant domain.TenantID, target domain.UserID) error {
	current, ok, err := s.store.GetTenantMembership(ctx, tenant, target)
	if err != nil {
		return wrap("get tenant membership", err)
	}
	if !ok || current.Role != domain.TenantRoleOwner {
		return nil
	}
	owners, err := s.store.CountTenantOwners(ctx, tenant)
	if err != nil {
		return wrap("count tenant owners", err)
	}
	if owners > 1 {
		return nil
	}
	return apierr.Conflict(CodeLastTenantOwner,
		"this is the organization's last owner; make somebody else an owner first", nil)
}
