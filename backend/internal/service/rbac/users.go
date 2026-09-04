package rbac

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// ListUsers returns every account, owner first and then by display name, so
// the administration screen always opens on the account that matters most.
func (s *Service) ListUsers(ctx context.Context) ([]domain.User, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, wrap("list users", err)
	}
	sort.SliceStable(users, func(i, j int) bool {
		ri, rj := roleOrder(users[i].Role), roleOrder(users[j].Role)
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(users[i].DisplayName) < strings.ToLower(users[j].DisplayName)
	})
	return users, nil
}

func roleOrder(r domain.UserRole) int {
	switch r {
	case domain.UserRoleOwner:
		return 0
	case domain.UserRoleAdmin:
		return 1
	case domain.UserRoleMember:
		return 2
	default:
		return 3
	}
}

// GetUser returns one account.
func (s *Service) GetUser(ctx context.Context, id domain.UserID) (domain.User, error) {
	u, ok, err := s.store.GetUserByID(ctx, id)
	if err != nil {
		return domain.User{}, wrap("get user", err)
	}
	if !ok {
		return domain.User{}, notFound(CodeUserNotFound, "user not found")
	}
	return u, nil
}

// CreateUser provisions a local (password) account with a non-owner role.
//
// Ownership is deliberately not creatable: an installation has exactly one
// owner and it changes by transfer, never by minting a second one. Inviting a
// user by email is equally deliberately absent -- AO has no mail delivery it
// can rely on until P4-D lands, and shipping an "invite sent" button that
// silently sends nothing would be worse than not having one. See
// docs/rbac.md's extension point.
func (s *Service) CreateUser(ctx context.Context, actor domain.Principal, in CreateUserInput) (domain.User, error) {
	if s.creator == nil {
		return domain.User{}, apierr.New(apierr.KindInternal, "USER_CREATE_UNAVAILABLE",
			"this daemon is not configured to create accounts", nil)
	}
	role := in.Role
	if role == "" {
		role = domain.UserRoleMember
	}
	if !domain.ValidUserRole(role) {
		return domain.User{}, apierr.Invalid(CodeInvalidRole, "unknown role", map[string]any{"role": string(role)})
	}
	if role == domain.UserRoleOwner {
		return domain.User{}, apierr.Invalid(CodeOwnerProtected,
			"ownership is transferred to an existing account, never assigned at creation", nil)
	}
	in.Role = role
	if strings.TrimSpace(in.Username) == "" {
		in.Username = strings.TrimSpace(in.Email)
	}
	created, err := s.creator.CreateUser(ctx, in)
	if err != nil {
		return domain.User{}, err
	}
	s.audit.Record(ctx, Event{
		Name:       EventUserCreated,
		Actor:      actor,
		TargetKind: "user",
		TargetID:   string(created.ID),
		Detail:     map[string]any{"role": string(created.Role)},
	})
	return created, nil
}

// SetUserRole changes an account's installation-wide role, enforcing the two
// rules that keep an installation reachable:
//
//  1. The owner cannot be demoted; ownership is TRANSFERRED by promoting
//     somebody else, which demotes the previous owner in the same
//     transaction. There is therefore no moment, even under concurrent
//     requests, when the installation has no owner.
//  2. Only the owner may transfer ownership. An administrator manages
//     accounts; an administrator who could also seize the installation would
//     make the owner role decorative.
func (s *Service) SetUserRole(ctx context.Context, actor domain.Principal, target domain.UserID, role domain.UserRole) (domain.User, error) {
	if !domain.ValidUserRole(role) {
		return domain.User{}, apierr.Invalid(CodeInvalidRole, "unknown role", map[string]any{"role": string(role)})
	}
	current, err := s.GetUser(ctx, target)
	if err != nil {
		return domain.User{}, err
	}
	if current.Role == role {
		return current, nil
	}

	owners, err := s.store.CountOwners(ctx)
	if err != nil {
		return domain.User{}, wrap("count owners", err)
	}

	switch {
	case role == domain.UserRoleOwner && owners > 0:
		// A transfer. Only the sitting owner may perform it.
		if actor.User.Role != domain.UserRoleOwner {
			return domain.User{}, apierr.Forbidden(CodeOwnerProtected,
				"only the installation owner can transfer ownership")
		}
		ok, err := s.store.TransferOwnership(ctx, actor.User.ID, target, s.at())
		if err != nil {
			if isOwnerUniqueConstraintErr(err) || isUniqueConstraintErr(err) {
				// Another transfer committed first. Nothing was changed here.
				return domain.User{}, apierr.Conflict(CodeLastOwner,
					"ownership changed concurrently; reload and try again", nil)
			}
			return domain.User{}, fmt.Errorf("transfer ownership: %w", err)
		}
		if !ok {
			return domain.User{}, notFound(CodeUserNotFound, "user not found")
		}
	case current.Role == domain.UserRoleOwner:
		// Demoting the sitting owner without naming a successor.
		return domain.User{}, apierr.Conflict(CodeLastOwner,
			"the installation owner cannot be demoted; transfer ownership to another account first", nil)
	default:
		if _, err := s.store.UpdateUserRole(ctx, target, role, s.at()); err != nil {
			if isOwnerUniqueConstraintErr(err) {
				return domain.User{}, apierr.Conflict(CodeLastOwner,
					"this installation already has an owner", nil)
			}
			return domain.User{}, fmt.Errorf("update user role: %w", err)
		}
	}

	updated, err := s.GetUser(ctx, target)
	if err != nil {
		return domain.User{}, err
	}
	s.audit.Record(ctx, Event{
		Name:       EventUserRoleChanged,
		Actor:      actor,
		TargetKind: "user",
		TargetID:   string(target),
		Detail:     map[string]any{"from": string(current.Role), "to": string(updated.Role)},
	})
	return updated, nil
}

// SetUserStatus enables or disables an account.
//
// The owner can never be disabled, and no actor can disable themselves. Both
// are lockout guards rather than permission checks: an administrator who
// disables their own only-administrator account has not done something they
// lacked the authority for, they have done something nobody wants to have
// done.
func (s *Service) SetUserStatus(ctx context.Context, actor domain.Principal, target domain.UserID, status domain.UserStatus) (domain.User, error) {
	if status != domain.UserStatusActive && status != domain.UserStatusDisabled {
		return domain.User{}, apierr.Invalid("INVALID_STATUS", "unknown status", map[string]any{"status": string(status)})
	}
	current, err := s.GetUser(ctx, target)
	if err != nil {
		return domain.User{}, err
	}
	if status == domain.UserStatusDisabled {
		if current.Role == domain.UserRoleOwner {
			return domain.User{}, apierr.Conflict(CodeLastOwner,
				"the installation owner cannot be disabled; transfer ownership first", nil)
		}
		if actor.User.ID == target {
			return domain.User{}, apierr.Conflict(CodeSelfDisable,
				"an account cannot disable itself", nil)
		}
	}
	if current.Status == status {
		return current, nil
	}
	if _, err := s.store.UpdateUserStatus(ctx, target, status, s.at()); err != nil {
		return domain.User{}, fmt.Errorf("update user status: %w", err)
	}
	updated, err := s.GetUser(ctx, target)
	if err != nil {
		return domain.User{}, err
	}
	name := EventUserEnabled
	if status == domain.UserStatusDisabled {
		name = EventUserDisabled
	}
	s.audit.Record(ctx, Event{
		Name:       name,
		Actor:      actor,
		TargetKind: "user",
		TargetID:   string(target),
		Detail:     map[string]any{"status": string(status)},
	})
	return updated, nil
}
