package rbac

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// ListTeams returns every team, active and archived.
func (s *Service) ListTeams(ctx context.Context) ([]domain.Team, error) {
	teams, err := s.store.ListTeams(ctx)
	if err != nil {
		return nil, wrap("list teams", err)
	}
	return teams, nil
}

// GetTeam returns one team.
func (s *Service) GetTeam(ctx context.Context, id domain.TeamID) (domain.Team, error) {
	t, ok, err := s.store.GetTeamByID(ctx, id)
	if err != nil {
		return domain.Team{}, wrap("get team", err)
	}
	if !ok {
		return domain.Team{}, notFound(CodeTeamNotFound, "team not found")
	}
	return t, nil
}

// CreateTeam creates an empty active team.
func (s *Service) CreateTeam(ctx context.Context, actor domain.Principal, name, description string) (domain.Team, error) {
	name = strings.TrimSpace(name)
	slug := slugify(name)
	if name == "" || slug == "" {
		return domain.Team{}, apierr.Invalid("TEAM_INVALID_NAME", "a team needs a name with at least one letter or digit", nil)
	}
	now := s.at()
	team := domain.Team{
		ID:          domain.TeamID(newID()),
		Name:        name,
		Slug:        slug,
		Description: strings.TrimSpace(description),
		Status:      domain.TeamStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	created, err := s.store.InsertTeam(ctx, team)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return domain.Team{}, apierr.Conflict(CodeTeamExists, "a team with that name already exists", nil)
		}
		return domain.Team{}, fmt.Errorf("create team: %w", err)
	}
	s.audit.Record(ctx, Event{
		Name:       EventTeamCreated,
		Actor:      actor,
		TargetKind: "team",
		TargetID:   string(created.ID),
		Detail:     map[string]any{"slug": created.Slug},
	})
	return created, nil
}

// UpdateTeam renames a team and/or replaces its description.
func (s *Service) UpdateTeam(ctx context.Context, actor domain.Principal, id domain.TeamID, name, description string) (domain.Team, error) {
	current, err := s.GetTeam(ctx, id)
	if err != nil {
		return domain.Team{}, err
	}
	name = strings.TrimSpace(name)
	slug := slugify(name)
	if name == "" || slug == "" {
		return domain.Team{}, apierr.Invalid("TEAM_INVALID_NAME", "a team needs a name with at least one letter or digit", nil)
	}
	ok, err := s.store.UpdateTeam(ctx, id, name, slug, strings.TrimSpace(description), s.at())
	if err != nil {
		if isUniqueConstraintErr(err) {
			return domain.Team{}, apierr.Conflict(CodeTeamExists, "a team with that name already exists", nil)
		}
		return domain.Team{}, fmt.Errorf("update team: %w", err)
	}
	if !ok {
		return domain.Team{}, notFound(CodeTeamNotFound, "team not found")
	}
	updated, err := s.GetTeam(ctx, id)
	if err != nil {
		return domain.Team{}, err
	}
	s.audit.Record(ctx, Event{
		Name:       EventTeamUpdated,
		Actor:      actor,
		TargetKind: "team",
		TargetID:   string(id),
		Detail:     map[string]any{"from": current.Name, "to": updated.Name},
	})
	return updated, nil
}

// ArchiveTeam archives or reactivates a team. Archiving is the safe form of
// deletion: the team's grants stop conferring access immediately (the
// resolver only reads ACTIVE teams) while every membership and grant row
// survives, so reactivating restores exactly what was there.
func (s *Service) ArchiveTeam(ctx context.Context, actor domain.Principal, id domain.TeamID, archived bool) (domain.Team, error) {
	if _, err := s.GetTeam(ctx, id); err != nil {
		return domain.Team{}, err
	}
	status := domain.TeamStatusActive
	if archived {
		status = domain.TeamStatusArchived
	}
	ok, err := s.store.UpdateTeamStatus(ctx, id, status, s.at())
	if err != nil {
		return domain.Team{}, fmt.Errorf("archive team: %w", err)
	}
	if !ok {
		return domain.Team{}, notFound(CodeTeamNotFound, "team not found")
	}
	updated, err := s.GetTeam(ctx, id)
	if err != nil {
		return domain.Team{}, err
	}
	s.audit.Record(ctx, Event{
		Name:       EventTeamUpdated,
		Actor:      actor,
		TargetKind: "team",
		TargetID:   string(id),
		Detail:     map[string]any{"status": string(status)},
	})
	return updated, nil
}

// DeleteTeam removes a team, its memberships and every grant made to it.
func (s *Service) DeleteTeam(ctx context.Context, actor domain.Principal, id domain.TeamID) error {
	if _, err := s.GetTeam(ctx, id); err != nil {
		return err
	}
	ok, err := s.store.DeleteTeam(ctx, id)
	if err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	if !ok {
		return notFound(CodeTeamNotFound, "team not found")
	}
	s.audit.Record(ctx, Event{
		Name:       EventTeamDeleted,
		Actor:      actor,
		TargetKind: "team",
		TargetID:   string(id),
	})
	return nil
}

// ListTeamMembers returns a team's membership rows.
func (s *Service) ListTeamMembers(ctx context.Context, id domain.TeamID) ([]domain.TeamMembership, error) {
	if _, err := s.GetTeam(ctx, id); err != nil {
		return nil, err
	}
	members, err := s.store.ListTeamMembers(ctx, id)
	if err != nil {
		return nil, wrap("list team members", err)
	}
	return members, nil
}

// ListTeamsForUser returns every membership one account holds.
func (s *Service) ListTeamsForUser(ctx context.Context, user domain.UserID) ([]domain.TeamMembership, error) {
	memberships, err := s.store.ListTeamMembershipsForUser(ctx, user)
	if err != nil {
		return nil, wrap("list team memberships for user", err)
	}
	return memberships, nil
}

// AddTeamMember adds an account to a team, or changes its role in it.
func (s *Service) AddTeamMember(ctx context.Context, actor domain.Principal, team domain.TeamID, user domain.UserID, role domain.TeamRole) (domain.TeamMembership, error) {
	if role == "" {
		role = domain.TeamRoleMember
	}
	if !domain.ValidTeamRole(role) {
		return domain.TeamMembership{}, apierr.Invalid(CodeInvalidRole, "unknown team role", map[string]any{"role": string(role)})
	}
	if _, err := s.GetTeam(ctx, team); err != nil {
		return domain.TeamMembership{}, err
	}
	if _, err := s.GetUser(ctx, user); err != nil {
		return domain.TeamMembership{}, err
	}
	now := s.at()
	m, err := s.store.UpsertTeamMembership(ctx, domain.TeamMembership{
		ID:        newID(),
		TeamID:    team,
		UserID:    user,
		Role:      role,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		return domain.TeamMembership{}, fmt.Errorf("add team member: %w", err)
	}
	s.audit.Record(ctx, Event{
		Name:       EventTeamMemberAdded,
		Actor:      actor,
		TargetKind: "team",
		TargetID:   string(team),
		SubjectID:  string(user),
		Detail:     map[string]any{"teamRole": string(role)},
	})
	return m, nil
}

// RemoveTeamMember removes an account from a team. Access that flowed through
// the team's grants stops on the member's very next request: the resolver
// reads membership per request and holds nothing across them.
func (s *Service) RemoveTeamMember(ctx context.Context, actor domain.Principal, team domain.TeamID, user domain.UserID) error {
	if _, err := s.GetTeam(ctx, team); err != nil {
		return err
	}
	ok, err := s.store.DeleteTeamMembership(ctx, team, user)
	if err != nil {
		return fmt.Errorf("remove team member: %w", err)
	}
	if !ok {
		return notFound("TEAM_MEMBERSHIP_NOT_FOUND", "that account is not in this team")
	}
	s.audit.Record(ctx, Event{
		Name:       EventTeamMemberRemoved,
		Actor:      actor,
		TargetKind: "team",
		TargetID:   string(team),
		SubjectID:  string(user),
	})
	return nil
}
