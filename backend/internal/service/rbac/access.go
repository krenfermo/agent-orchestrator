package rbac

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// ProjectAccess is the full picture of who can reach one project: the grants
// on it plus the project's own owner row, which is an implicit administrator
// grant that predates P4-B and is deliberately still honored.
type ProjectAccess struct {
	ProjectID domain.ProjectID
	// OwnerUserID is the account that registered the project, nil for a
	// project that predates ownership.
	OwnerUserID *domain.UserID
	Grants      []domain.ProjectGrant
}

// ListProjectAccess returns the access list for one project.
func (s *Service) ListProjectAccess(ctx context.Context, project domain.ProjectID) (ProjectAccess, error) {
	owner, err := s.store.GetProjectOwner(ctx, project)
	if err != nil {
		return ProjectAccess{}, wrap("get project owner", err)
	}
	grants, err := s.store.ListProjectGrants(ctx, project)
	if err != nil {
		return ProjectAccess{}, wrap("list project grants", err)
	}
	return ProjectAccess{ProjectID: project, OwnerUserID: owner, Grants: grants}, nil
}

// GrantProjectAccess gives a user or a team a role on one project, or changes
// the role of an existing grant.
//
// The subject is verified to exist before the grant is written. A grant to a
// deleted user id would be an access rule nobody can see in any list and
// nobody can revoke by name -- exactly the kind of row that turns into a
// security incident years later.
func (s *Service) GrantProjectAccess(ctx context.Context, actor domain.Principal, project domain.ProjectID, kind domain.GrantSubjectKind, subject string, role domain.ProjectRole) (domain.ProjectGrant, error) {
	if !domain.ValidProjectRole(role) {
		return domain.ProjectGrant{}, apierr.Invalid(CodeInvalidRole, "unknown project role", map[string]any{"role": string(role)})
	}
	switch kind {
	case domain.GrantSubjectUser:
		if _, err := s.GetUser(ctx, domain.UserID(subject)); err != nil {
			return domain.ProjectGrant{}, err
		}
	case domain.GrantSubjectTeam:
		if _, err := s.GetTeam(ctx, domain.TeamID(subject)); err != nil {
			return domain.ProjectGrant{}, err
		}
	default:
		return domain.ProjectGrant{}, apierr.Invalid(CodeInvalidSubject,
			"a grant subject is a user or a team", map[string]any{"subjectKind": string(kind)})
	}

	now := s.at()
	grant, err := s.store.UpsertProjectGrant(ctx, domain.ProjectGrant{
		ID:          newID(),
		ProjectID:   project,
		SubjectKind: kind,
		SubjectID:   subject,
		Role:        role,
		CreatedAt:   now,
		UpdatedAt:   now,
		CreatedBy:   actor.User.ID,
	})
	if err != nil {
		return domain.ProjectGrant{}, fmt.Errorf("grant project access: %w", err)
	}
	s.audit.Record(ctx, Event{
		Name:       EventProjectAccessGranted,
		Actor:      actor,
		TargetKind: "project",
		TargetID:   string(project),
		SubjectID:  subject,
		Detail:     map[string]any{"subjectKind": string(kind), "projectRole": string(role)},
	})
	return grant, nil
}

// RevokeProjectAccess removes one subject's grant on one project. The
// project's owner row is NOT a grant and cannot be revoked here; changing who
// owns a project is a different operation from managing its access list.
func (s *Service) RevokeProjectAccess(ctx context.Context, actor domain.Principal, project domain.ProjectID, kind domain.GrantSubjectKind, subject string) error {
	if kind != domain.GrantSubjectUser && kind != domain.GrantSubjectTeam {
		return apierr.Invalid(CodeInvalidSubject, "a grant subject is a user or a team",
			map[string]any{"subjectKind": string(kind)})
	}
	ok, err := s.store.DeleteProjectGrant(ctx, project, kind, subject)
	if err != nil {
		return fmt.Errorf("revoke project access: %w", err)
	}
	if !ok {
		return notFound("PROJECT_GRANT_NOT_FOUND", "that subject has no grant on this project")
	}
	s.audit.Record(ctx, Event{
		Name:       EventProjectAccessRevoked,
		Actor:      actor,
		TargetKind: "project",
		TargetID:   string(project),
		SubjectID:  subject,
		Detail:     map[string]any{"subjectKind": string(kind)},
	})
	return nil
}
