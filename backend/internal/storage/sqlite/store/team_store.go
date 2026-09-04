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

// InsertTeam creates a team row. Slug uniqueness is enforced by ux_teams_slug
// at the SQL layer, so a concurrent duplicate loses the race there rather than
// in an application-level check that two callers can both pass.
func (s *Store) InsertTeam(ctx context.Context, t domain.Team) (domain.Team, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.InsertTeam(ctx, gen.InsertTeamParams{
		ID:          t.ID,
		Name:        t.Name,
		Slug:        t.Slug,
		Description: t.Description,
		Status:      t.Status,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	})
	if err != nil {
		return domain.Team{}, fmt.Errorf("insert team: %w", err)
	}
	return teamFromRow(row), nil
}

// GetTeamByID returns a team, or (zero, false, nil) when none exists.
func (s *Store) GetTeamByID(ctx context.Context, id domain.TeamID) (domain.Team, bool, error) {
	row, err := s.qr.GetTeamByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Team{}, false, nil
		}
		return domain.Team{}, false, fmt.Errorf("get team %s: %w", id, err)
	}
	return teamFromRow(row), true, nil
}

// GetTeamBySlug returns a team by its slug, or (zero, false, nil).
func (s *Store) GetTeamBySlug(ctx context.Context, slug string) (domain.Team, bool, error) {
	row, err := s.qr.GetTeamBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Team{}, false, nil
		}
		return domain.Team{}, false, fmt.Errorf("get team by slug: %w", err)
	}
	return teamFromRow(row), true, nil
}

// ListTeams returns every team, sorted by name. Sorting happens here rather
// than in SQL because sqlc v1.31.1 mis-generates a trailing ORDER BY on a
// SQLite :many query (see queries/users.sql).
func (s *Store) ListTeams(ctx context.Context) ([]domain.Team, error) {
	rows, err := s.qr.ListTeams(ctx)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	out := make([]domain.Team, 0, len(rows))
	for _, r := range rows {
		out = append(out, teamFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdateTeam renames a team and updates its description. Returns false when
// the id doesn't exist.
func (s *Store) UpdateTeam(ctx context.Context, id domain.TeamID, name, slug, description string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateTeam(ctx, gen.UpdateTeamParams{
		Name:        name,
		Slug:        slug,
		Description: description,
		UpdatedAt:   updatedAt,
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("update team %s: %w", id, err)
	}
	return n > 0, nil
}

// UpdateTeamStatus archives or reactivates a team. An archived team keeps its
// membership and its grants; ListActiveTeamIDsForUser simply stops returning
// it, so access stops flowing through it without any row being destroyed.
func (s *Store) UpdateTeamStatus(ctx context.Context, id domain.TeamID, status domain.TeamStatus, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateTeamStatus(ctx, gen.UpdateTeamStatusParams{
		Status:    status,
		UpdatedAt: updatedAt,
		ID:        id,
	})
	if err != nil {
		return false, fmt.Errorf("update team %s status: %w", id, err)
	}
	return n > 0, nil
}

// DeleteTeam removes a team, its memberships (ON DELETE CASCADE) and every
// project grant made to it. The grants are deleted explicitly because
// project_grants.subject_id is polymorphic and therefore cannot carry a
// foreign key -- leaving them behind would let a later team reusing the id
// inherit access nobody granted it.
func (s *Store) DeleteTeam(ctx context.Context, id domain.TeamID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var deleted bool
	err := s.inTx(ctx, "delete team", func(q *gen.Queries) error {
		if _, err := q.DeleteProjectGrantsBySubject(ctx, gen.DeleteProjectGrantsBySubjectParams{
			SubjectKind: domain.GrantSubjectTeam,
			SubjectID:   string(id),
		}); err != nil {
			return err
		}
		n, err := q.DeleteTeam(ctx, id)
		if err != nil {
			return err
		}
		deleted = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// UpsertTeamMembership adds a user to a team, or changes their role if they
// are already in it.
func (s *Store) UpsertTeamMembership(ctx context.Context, m domain.TeamMembership) (domain.TeamMembership, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.InsertTeamMembership(ctx, gen.InsertTeamMembershipParams{
		ID:        m.ID,
		TeamID:    m.TeamID,
		UserID:    m.UserID,
		Role:      m.Role,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	})
	if err != nil {
		return domain.TeamMembership{}, fmt.Errorf("upsert team membership: %w", err)
	}
	return membershipFromRow(row), nil
}

// DeleteTeamMembership removes a user from a team. Returns false when the
// pair had no membership row.
func (s *Store) DeleteTeamMembership(ctx context.Context, team domain.TeamID, user domain.UserID) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteTeamMembership(ctx, gen.DeleteTeamMembershipParams{TeamID: team, UserID: user})
	if err != nil {
		return false, fmt.Errorf("delete team membership: %w", err)
	}
	return n > 0, nil
}

// ListTeamMembers returns a team's memberships, oldest first.
func (s *Store) ListTeamMembers(ctx context.Context, team domain.TeamID) ([]domain.TeamMembership, error) {
	rows, err := s.qr.ListTeamMembers(ctx, team)
	if err != nil {
		return nil, fmt.Errorf("list team members: %w", err)
	}
	out := make([]domain.TeamMembership, 0, len(rows))
	for _, r := range rows {
		out = append(out, membershipFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ListTeamMembershipsForUser returns every membership a user holds, including
// archived teams (the caller decides what an archived team means for it).
func (s *Store) ListTeamMembershipsForUser(ctx context.Context, user domain.UserID) ([]domain.TeamMembership, error) {
	rows, err := s.qr.ListTeamMembershipsForUser(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("list team memberships for user: %w", err)
	}
	out := make([]domain.TeamMembership, 0, len(rows))
	for _, r := range rows {
		out = append(out, membershipFromRow(r))
	}
	return out, nil
}

// ListActiveTeamIDsForUser returns the ids of the ACTIVE teams a user belongs
// to. This is the authorization resolver's per-request query; it stays a
// single indexed lookup on purpose.
func (s *Store) ListActiveTeamIDsForUser(ctx context.Context, user domain.UserID) ([]domain.TeamID, error) {
	ids, err := s.qr.ListActiveTeamIDsForUser(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("list active team ids for user: %w", err)
	}
	return ids, nil
}

// CountTeams returns the number of team rows.
func (s *Store) CountTeams(ctx context.Context) (int64, error) {
	n, err := s.qr.CountTeams(ctx)
	if err != nil {
		return 0, fmt.Errorf("count teams: %w", err)
	}
	return n, nil
}

func teamFromRow(row gen.Team) domain.Team {
	return domain.Team{
		ID:          row.ID,
		Name:        row.Name,
		Slug:        row.Slug,
		Description: row.Description,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

func membershipFromRow(row gen.TeamMembership) domain.TeamMembership {
	return domain.TeamMembership{
		ID:        row.ID,
		TeamID:    row.TeamID,
		UserID:    row.UserID,
		Role:      row.Role,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}
