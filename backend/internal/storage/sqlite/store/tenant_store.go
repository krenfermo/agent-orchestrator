package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// P4-C tenant storage. Reads that the authorization resolver depends on are
// marked as such: they run on every authenticated request, so each is a single
// indexed lookup and none of them scans a table whose size grows with the
// installation rather than with the caller's own access.

// InsertTenant creates an organization. Slug uniqueness is enforced by
// ux_tenants_slug at the SQL layer, so a concurrent duplicate loses the race
// there rather than in an application check two callers can both pass.
func (s *Store) InsertTenant(ctx context.Context, t domain.Tenant) (domain.Tenant, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.InsertTenant(ctx, gen.InsertTenantParams{
		ID:          t.ID,
		Name:        t.Name,
		Slug:        t.Slug,
		Description: t.Description,
		Status:      t.Status,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	})
	if err != nil {
		return domain.Tenant{}, fmt.Errorf("insert tenant: %w", err)
	}
	return tenantFromRow(row), nil
}

// GetTenantByID returns an organization, or (zero, false, nil) when none
// exists.
func (s *Store) GetTenantByID(ctx context.Context, id domain.TenantID) (domain.Tenant, bool, error) {
	row, err := s.qr.GetTenantByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Tenant{}, false, nil
		}
		return domain.Tenant{}, false, fmt.Errorf("get tenant %s: %w", id, err)
	}
	return tenantFromRow(row), true, nil
}

// GetTenantBySlug returns an organization by slug, or (zero, false, nil).
func (s *Store) GetTenantBySlug(ctx context.Context, slug string) (domain.Tenant, bool, error) {
	row, err := s.qr.GetTenantBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Tenant{}, false, nil
		}
		return domain.Tenant{}, false, fmt.Errorf("get tenant by slug: %w", err)
	}
	return tenantFromRow(row), true, nil
}

// ListTenants returns every organization, sorted by name. Sorted here rather
// than in SQL because sqlc v1.31.1 mis-generates a trailing ORDER BY on a
// SQLite :many query (see queries/users.sql).
func (s *Store) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	rows, err := s.qr.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	out := make([]domain.Tenant, 0, len(rows))
	for _, r := range rows {
		out = append(out, tenantFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdateTenant renames an organization and updates its description. Returns
// false when the id doesn't exist.
func (s *Store) UpdateTenant(ctx context.Context, id domain.TenantID, name, slug, description string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateTenant(ctx, gen.UpdateTenantParams{
		Name:        name,
		Slug:        slug,
		Description: description,
		UpdatedAt:   updatedAt,
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("update tenant %s: %w", id, err)
	}
	return n > 0, nil
}

// UpdateTenantStatus archives or reactivates an organization.
func (s *Store) UpdateTenantStatus(ctx context.Context, id domain.TenantID, status domain.TenantStatus, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateTenantStatus(ctx, gen.UpdateTenantStatusParams{
		Status:    status,
		UpdatedAt: updatedAt,
		ID:        id,
	})
	if err != nil {
		return false, fmt.Errorf("update tenant status %s: %w", id, err)
	}
	return n > 0, nil
}

// CountTenants reports how many organizations exist. The frontend uses it to
// decide whether an installation needs an organization selector at all.
func (s *Store) CountTenants(ctx context.Context) (int64, error) {
	n, err := s.qr.CountTenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("count tenants: %w", err)
	}
	return n, nil
}

// UpsertTenantMembership adds a user to an organization, or changes the role
// they already hold there.
func (s *Store) UpsertTenantMembership(ctx context.Context, m domain.TenantMembership) (domain.TenantMembership, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertTenantMembership(ctx, gen.UpsertTenantMembershipParams{
		ID:        m.ID,
		TenantID:  m.TenantID,
		UserID:    m.UserID,
		Role:      m.Role,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	})
	if err != nil {
		return domain.TenantMembership{}, fmt.Errorf("upsert tenant membership: %w", err)
	}
	return tenantMembershipFromRow(row), nil
}

// DeleteTenantMembership removes a user from an organization.
func (s *Store) DeleteTenantMembership(ctx context.Context, tenant domain.TenantID, user domain.UserID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteTenantMembership(ctx, gen.DeleteTenantMembershipParams{TenantID: tenant, UserID: user})
	if err != nil {
		return false, fmt.Errorf("delete tenant membership: %w", err)
	}
	return n > 0, nil
}

// DeleteTenantMembershipsForUser removes an account from every organization.
// Used when the account is removed from the installation's access model, in
// the same breath as its project grants; disabling an account deliberately
// does NOT call this, so re-enabling restores what it had.
func (s *Store) DeleteTenantMembershipsForUser(ctx context.Context, user domain.UserID) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteTenantMembershipsForUser(ctx, user)
	if err != nil {
		return 0, fmt.Errorf("delete tenant memberships for user: %w", err)
	}
	return n, nil
}

// GetTenantMembership returns one user's standing in one organization.
func (s *Store) GetTenantMembership(ctx context.Context, tenant domain.TenantID, user domain.UserID) (domain.TenantMembership, bool, error) {
	row, err := s.qr.GetTenantMembership(ctx, gen.GetTenantMembershipParams{TenantID: tenant, UserID: user})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.TenantMembership{}, false, nil
		}
		return domain.TenantMembership{}, false, fmt.Errorf("get tenant membership: %w", err)
	}
	return tenantMembershipFromRow(row), true, nil
}

// ListTenantMembers returns every membership in one organization, sorted by
// user id for stable output.
func (s *Store) ListTenantMembers(ctx context.Context, tenant domain.TenantID) ([]domain.TenantMembership, error) {
	rows, err := s.qr.ListTenantMembers(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("list tenant members: %w", err)
	}
	out := make([]domain.TenantMembership, 0, len(rows))
	for _, r := range rows {
		out = append(out, tenantMembershipFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

// CountTenantOwners reports how many owners an organization has. The
// membership service reads it before demoting or removing one, so the last
// owner cannot be taken away and leave the organization unownable.
func (s *Store) CountTenantOwners(ctx context.Context, tenant domain.TenantID) (int64, error) {
	n, err := s.qr.CountTenantMembersWithRole(ctx, gen.CountTenantMembersWithRoleParams{
		TenantID: tenant,
		Role:     domain.TenantRoleOwner,
	})
	if err != nil {
		return 0, fmt.Errorf("count tenant owners: %w", err)
	}
	return n, nil
}

// ListActiveTenantMembershipsForUser is the authorization resolver's hot path:
// the organizations this account belongs to, and its role in each. Archived
// organizations are excluded in SQL so an archived one stops conferring access
// at the source rather than by a filter somebody could forget.
func (s *Store) ListActiveTenantMembershipsForUser(ctx context.Context, user domain.UserID) ([]domain.TenantMembership, error) {
	rows, err := s.qr.ListActiveTenantMembershipsForUser(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("list active tenant memberships: %w", err)
	}
	out := make([]domain.TenantMembership, 0, len(rows))
	for _, r := range rows {
		out = append(out, tenantMembershipFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	return out, nil
}

// GetProjectTenant returns the organization that owns a project, or
// (zero, false, nil) when the project does not exist.
func (s *Store) GetProjectTenant(ctx context.Context, id domain.ProjectID) (domain.TenantID, bool, error) {
	tenant, err := s.qr.GetProjectTenant(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get project tenant %s: %w", id, err)
	}
	return tenant, true, nil
}

// SetProjectTenant moves a project into an organization. Returns false when
// the project id doesn't exist.
func (s *Store) SetProjectTenant(ctx context.Context, id domain.ProjectID, tenant domain.TenantID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.SetProjectTenant(ctx, gen.SetProjectTenantParams{TenantID: tenant, ID: id})
	if err != nil {
		return false, fmt.Errorf("set project tenant %s: %w", id, err)
	}
	return n > 0, nil
}

// ListProjectTenancy resolves the organization of a bounded set of projects in
// one round trip. This is what lets the resolver check that a grant it just
// read is even in an organization the caller belongs to, without a query per
// grant and without loading every project in the installation.
func (s *Store) ListProjectTenancy(ctx context.Context, ids []domain.ProjectID) (map[domain.ProjectID]domain.TenantID, error) {
	out := make(map[domain.ProjectID]domain.TenantID, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.qr.ListProjectTenancyByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list project tenancy: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = r.TenantID
	}
	return out, nil
}

// ListProjectIDsInTenants lists every project owned by any of the given
// organizations, with the organization that owns it. Queried per tenant rather
// than with a generated IN-clause for the reason ListProjectGrantsForTeams
// gives: a user's tenant count is small and bounded by their membership, and
// each lookup is one indexed hit.
func (s *Store) ListProjectIDsInTenants(ctx context.Context, tenants []domain.TenantID) (map[domain.ProjectID]domain.TenantID, error) {
	out := map[domain.ProjectID]domain.TenantID{}
	for _, tenant := range tenants {
		ids, err := s.qr.ListProjectIDsByTenant(ctx, tenant)
		if err != nil {
			return nil, fmt.Errorf("list project ids in tenant %s: %w", tenant, err)
		}
		for _, id := range ids {
			out[id] = tenant
		}
	}
	return out, nil
}

// CountProjectsInTenant reports how many projects an organization owns.
func (s *Store) CountProjectsInTenant(ctx context.Context, tenant domain.TenantID) (int64, error) {
	n, err := s.qr.CountProjectsInTenant(ctx, tenant)
	if err != nil {
		return 0, fmt.Errorf("count projects in tenant %s: %w", tenant, err)
	}
	return n, nil
}

// GetTeamTenant returns the organization a team belongs to.
func (s *Store) GetTeamTenant(ctx context.Context, id domain.TeamID) (domain.TenantID, bool, error) {
	tenant, err := s.qr.GetTeamTenant(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get team tenant %s: %w", id, err)
	}
	return tenant, true, nil
}

// SetTeamTenant moves a team into an organization.
func (s *Store) SetTeamTenant(ctx context.Context, id domain.TeamID, tenant domain.TenantID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.SetTeamTenant(ctx, gen.SetTeamTenantParams{TenantID: tenant, ID: id})
	if err != nil {
		return false, fmt.Errorf("set team tenant %s: %w", id, err)
	}
	return n > 0, nil
}

// ListTeamTenancy resolves the organization of a bounded set of teams in one
// round trip, the team-side counterpart of ListProjectTenancy.
func (s *Store) ListTeamTenancy(ctx context.Context, ids []domain.TeamID) (map[domain.TeamID]domain.TenantID, error) {
	out := make(map[domain.TeamID]domain.TenantID, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.qr.ListTeamTenancyByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list team tenancy: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = r.TenantID
	}
	return out, nil
}

func tenantFromRow(row gen.Tenant) domain.Tenant {
	return domain.Tenant{
		ID:          row.ID,
		Name:        row.Name,
		Slug:        row.Slug,
		Description: row.Description,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

func tenantMembershipFromRow(row gen.TenantMembership) domain.TenantMembership {
	return domain.TenantMembership{
		ID:        row.ID,
		TenantID:  row.TenantID,
		UserID:    row.UserID,
		Role:      row.Role,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}
