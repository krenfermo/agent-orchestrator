// Package rbac is the write side of P4-B's authorization layer: managing
// accounts, teams, team membership and project access, plus the owner-safety
// invariants that keep an installation from locking itself out.
//
// The read side -- "may this principal do X?" -- belongs to service/authz and
// is not duplicated here. This package enforces the rules that are NOT
// permission questions: an installation must always have exactly one reachable
// owner, an administrator may not take the owner's account, and nobody may
// disable the account they are currently using.
package rbac

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Store is the durable surface this service needs. Backed by
// storage/sqlite/store.Store in production.
type Store interface {
	GetUserByID(ctx context.Context, id domain.UserID) (domain.User, bool, error)
	ListUsers(ctx context.Context) ([]domain.User, error)
	CountOwners(ctx context.Context) (int64, error)
	UpdateUserRole(ctx context.Context, id domain.UserID, role domain.UserRole, updatedAt time.Time) (bool, error)
	UpdateUserStatus(ctx context.Context, id domain.UserID, status domain.UserStatus, updatedAt time.Time) (bool, error)
	TransferOwnership(ctx context.Context, from, to domain.UserID, updatedAt time.Time) (bool, error)

	InsertTeam(ctx context.Context, t domain.Team) (domain.Team, error)
	GetTeamByID(ctx context.Context, id domain.TeamID) (domain.Team, bool, error)
	ListTeams(ctx context.Context) ([]domain.Team, error)
	UpdateTeam(ctx context.Context, id domain.TeamID, name, slug, description string, updatedAt time.Time) (bool, error)
	UpdateTeamStatus(ctx context.Context, id domain.TeamID, status domain.TeamStatus, updatedAt time.Time) (bool, error)
	DeleteTeam(ctx context.Context, id domain.TeamID) (bool, error)
	UpsertTeamMembership(ctx context.Context, m domain.TeamMembership) (domain.TeamMembership, error)
	DeleteTeamMembership(ctx context.Context, team domain.TeamID, user domain.UserID) (bool, error)
	ListTeamMembers(ctx context.Context, team domain.TeamID) ([]domain.TeamMembership, error)
	ListTeamMembershipsForUser(ctx context.Context, user domain.UserID) ([]domain.TeamMembership, error)

	UpsertProjectGrant(ctx context.Context, g domain.ProjectGrant) (domain.ProjectGrant, error)
	DeleteProjectGrant(ctx context.Context, project domain.ProjectID, kind domain.GrantSubjectKind, subject string) (bool, error)
	ListProjectGrants(ctx context.Context, project domain.ProjectID) ([]domain.ProjectGrant, error)
	GetProjectOwner(ctx context.Context, id domain.ProjectID) (*domain.UserID, error)
}

// UserCreator is the account-creation surface, satisfied by
// service/authsvc.Manager. P4-B does not reimplement password hashing or
// federated provisioning; it reuses the identity layer P4-A finished and adds
// authorization on top.
type UserCreator interface {
	CreateUser(ctx context.Context, in CreateUserInput) (domain.User, error)
}

// CreateUserInput mirrors authsvc.CreateUserInput. It is redeclared rather
// than imported so this package does not depend on the identity service's
// package for a struct shape; the adapter in httpd wires the two.
type CreateUserInput struct {
	DisplayName string
	Email       string
	Username    string
	Password    string
	Role        domain.UserRole
}

// Service is the default implementation.
type Service struct {
	store   Store
	creator UserCreator
	audit   Audit
	now     func() time.Time
}

// New builds a Service. now defaults to time.Now; creator and audit may be nil
// (account creation then reports NOT_IMPLEMENTED, and audit becomes a no-op).
func New(store Store, creator UserCreator, audit Audit, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	if audit == nil {
		audit = NoopAudit{}
	}
	return &Service{store: store, creator: creator, audit: audit, now: now}
}

// Manager is the service-facing surface every transport calls.
type Manager interface {
	ListUsers(ctx context.Context) ([]domain.User, error)
	GetUser(ctx context.Context, id domain.UserID) (domain.User, error)
	CreateUser(ctx context.Context, actor domain.Principal, in CreateUserInput) (domain.User, error)
	SetUserRole(ctx context.Context, actor domain.Principal, target domain.UserID, role domain.UserRole) (domain.User, error)
	SetUserStatus(ctx context.Context, actor domain.Principal, target domain.UserID, status domain.UserStatus) (domain.User, error)

	ListTeams(ctx context.Context) ([]domain.Team, error)
	GetTeam(ctx context.Context, id domain.TeamID) (domain.Team, error)
	CreateTeam(ctx context.Context, actor domain.Principal, name, description string) (domain.Team, error)
	UpdateTeam(ctx context.Context, actor domain.Principal, id domain.TeamID, name, description string) (domain.Team, error)
	ArchiveTeam(ctx context.Context, actor domain.Principal, id domain.TeamID, archived bool) (domain.Team, error)
	DeleteTeam(ctx context.Context, actor domain.Principal, id domain.TeamID) error
	ListTeamMembers(ctx context.Context, id domain.TeamID) ([]domain.TeamMembership, error)
	ListTeamsForUser(ctx context.Context, user domain.UserID) ([]domain.TeamMembership, error)
	AddTeamMember(ctx context.Context, actor domain.Principal, team domain.TeamID, user domain.UserID, role domain.TeamRole) (domain.TeamMembership, error)
	RemoveTeamMember(ctx context.Context, actor domain.Principal, team domain.TeamID, user domain.UserID) error

	ListProjectAccess(ctx context.Context, project domain.ProjectID) (ProjectAccess, error)
	GrantProjectAccess(ctx context.Context, actor domain.Principal, project domain.ProjectID, kind domain.GrantSubjectKind, subject string, role domain.ProjectRole) (domain.ProjectGrant, error)
	RevokeProjectAccess(ctx context.Context, actor domain.Principal, project domain.ProjectID, kind domain.GrantSubjectKind, subject string) error
}

var _ Manager = (*Service)(nil)

// Stable error codes. They are part of the API contract: the frontend and the
// CLI both branch on them, and an operator greps for them.
const (
	// CodeLastOwner is returned when a change would leave the installation
	// with no owner.
	CodeLastOwner = "LAST_OWNER_PROTECTED"
	// CodeOwnerProtected is returned when a non-owner tries to alter the
	// owner's account.
	CodeOwnerProtected = "OWNER_ACCOUNT_PROTECTED"
	// CodeSelfDisable is returned when an actor tries to disable themselves.
	CodeSelfDisable = "SELF_DISABLE_FORBIDDEN"
	// CodeUserNotFound is returned for an unknown account id.
	CodeUserNotFound = "USER_NOT_FOUND"
	// CodeTeamNotFound is returned for an unknown team id.
	CodeTeamNotFound = "TEAM_NOT_FOUND"
	// CodeInvalidRole is returned for a role this build does not persist.
	CodeInvalidRole = "INVALID_ROLE"
	// CodeTeamExists is returned when a team name collides.
	CodeTeamExists = "TEAM_ALREADY_EXISTS"
	// CodeInvalidSubject is returned for a grant subject that does not exist.
	CodeInvalidSubject = "GRANT_SUBJECT_NOT_FOUND"
)

func (s *Service) at() time.Time { return s.now().UTC() }

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify derives a team's stable, comparable identifier from its name. The
// slug exists so "Platform" and "platform " cannot become two teams that look
// identical in a member list; the unique index on it is what enforces that.
func slugify(name string) string {
	s := slugUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	return strings.Trim(s, "-")
}

func newID() string { return uuid.NewString() }

func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isOwnerUniqueConstraintErr(err error) bool {
	return err != nil &&
		(strings.Contains(err.Error(), "users.role") || strings.Contains(err.Error(), "ux_users_single_owner"))
}

func notFound(code, msg string) error { return apierr.NotFound(code, msg) }

func wrap(what string, err error) error {
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}
