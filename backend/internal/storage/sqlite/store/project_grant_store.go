package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// UpsertProjectGrant grants a subject access to a project, or changes the role
// of an existing grant.
func (s *Store) UpsertProjectGrant(ctx context.Context, g domain.ProjectGrant) (domain.ProjectGrant, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertProjectGrant(ctx, gen.UpsertProjectGrantParams{
		ID:          g.ID,
		ProjectID:   g.ProjectID,
		SubjectKind: g.SubjectKind,
		SubjectID:   g.SubjectID,
		Role:        g.Role,
		CreatedAt:   g.CreatedAt,
		UpdatedAt:   g.UpdatedAt,
		CreatedBy:   g.CreatedBy,
	})
	if err != nil {
		return domain.ProjectGrant{}, fmt.Errorf("upsert project grant: %w", err)
	}
	return grantFromRow(row), nil
}

// DeleteProjectGrant revokes one subject's access to one project. Returns
// false when there was no such grant.
func (s *Store) DeleteProjectGrant(ctx context.Context, project domain.ProjectID, kind domain.GrantSubjectKind, subject string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteProjectGrant(ctx, gen.DeleteProjectGrantParams{
		ProjectID:   project,
		SubjectKind: kind,
		SubjectID:   subject,
	})
	if err != nil {
		return false, fmt.Errorf("delete project grant: %w", err)
	}
	return n > 0, nil
}

// ListProjectGrants returns every grant on one project, newest last.
func (s *Store) ListProjectGrants(ctx context.Context, project domain.ProjectID) ([]domain.ProjectGrant, error) {
	rows, err := s.qr.ListProjectGrantsByProject(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("list project grants: %w", err)
	}
	out := make([]domain.ProjectGrant, 0, len(rows))
	for _, r := range rows {
		out = append(out, grantFromRow(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ListProjectGrantsForUser returns the grants made DIRECTLY to one user. Team
// grants are resolved separately (see ListProjectGrantsForTeams) so the
// resolver can distinguish "granted to this person" from "inherited", which
// is what the access UI has to show.
func (s *Store) ListProjectGrantsForUser(ctx context.Context, user domain.UserID) ([]domain.ProjectGrant, error) {
	rows, err := s.qr.ListProjectGrantsBySubject(ctx, gen.ListProjectGrantsBySubjectParams{
		SubjectKind: domain.GrantSubjectUser,
		SubjectID:   string(user),
	})
	if err != nil {
		return nil, fmt.Errorf("list project grants for user: %w", err)
	}
	out := make([]domain.ProjectGrant, 0, len(rows))
	for _, r := range rows {
		out = append(out, grantFromRow(r))
	}
	return out, nil
}

// ListProjectGrantsForTeams returns the grants held by any of the given teams.
// Queried per team rather than with a generated IN-clause: a user's team count
// is small and bounded by their membership, each lookup is a single indexed
// hit, and the alternative would be scanning every team grant in the
// installation to filter in Go.
func (s *Store) ListProjectGrantsForTeams(ctx context.Context, teams []domain.TeamID) ([]domain.ProjectGrant, error) {
	out := make([]domain.ProjectGrant, 0, len(teams))
	for _, team := range teams {
		rows, err := s.qr.ListProjectGrantsBySubject(ctx, gen.ListProjectGrantsBySubjectParams{
			SubjectKind: domain.GrantSubjectTeam,
			SubjectID:   string(team),
		})
		if err != nil {
			return nil, fmt.Errorf("list project grants for team %s: %w", team, err)
		}
		for _, r := range rows {
			out = append(out, grantFromRow(r))
		}
	}
	return out, nil
}

// DeleteProjectGrantsForUser revokes every direct grant a user holds. Used
// when an account is removed from the installation's access model; disabling
// an account deliberately does NOT call this, so re-enabling it restores what
// it had.
func (s *Store) DeleteProjectGrantsForUser(ctx context.Context, user domain.UserID) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteProjectGrantsBySubject(ctx, gen.DeleteProjectGrantsBySubjectParams{
		SubjectKind: domain.GrantSubjectUser,
		SubjectID:   string(user),
	})
	if err != nil {
		return 0, fmt.Errorf("delete project grants for user: %w", err)
	}
	return n, nil
}

func grantFromRow(row gen.ProjectGrant) domain.ProjectGrant {
	return domain.ProjectGrant{
		ID:          row.ID,
		ProjectID:   row.ProjectID,
		SubjectKind: row.SubjectKind,
		SubjectID:   row.SubjectID,
		Role:        row.Role,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		CreatedBy:   row.CreatedBy,
	}
}
