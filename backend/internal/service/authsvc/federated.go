package authsvc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// FederatedUserInput provisions an account for an external identity. There is
// no Password field, and that is the point: a federated account has no local
// credential to guess, rotate or leak. The stored password_hash is the empty
// string, which bcrypt can never match (see Authenticate's explicit guard).
type FederatedUserInput struct {
	DisplayName string
	Email       string
	Username    string
	// PreferOwner asks for the installation-owner role. It is granted only
	// when the installation genuinely has no owner; a losing race against the
	// ux_users_single_owner index silently becomes a member instead of
	// failing the login.
	PreferOwner bool
}

// CreateFederatedUser creates a password-less account for a provider-asserted
// identity, resolving username collisions rather than failing the login on
// one: two people at different providers can legitimately present the same
// preferred_username, and neither should be told "sign-in failed".
//
// Email uniqueness is NOT resolved this way. A collision there means an
// account for that address already exists, and silently minting a second one
// under a mangled address would be the wrong answer to a question only the
// linking rules in service/ssosvc may decide.
func (s *Service) CreateFederatedUser(ctx context.Context, in FederatedUserInput) (domain.User, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return domain.User{}, apierr.Invalid("OIDC_NO_EMAIL_CLAIM",
			"the identity provider returned no email claim, so AO cannot create an account for it", nil)
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = email
	}

	username, err := s.availableUsername(ctx, in.Username, email)
	if err != nil {
		return domain.User{}, err
	}

	role := domain.UserRoleMember
	if in.PreferOwner {
		owners, err := s.store.CountOwners(ctx)
		if err != nil {
			return domain.User{}, fmt.Errorf("count owners: %w", err)
		}
		if owners == 0 {
			role = domain.UserRoleOwner
		}
	}

	now := s.now().UTC()
	u := domain.User{
		ID:          domain.UserID(uuid.NewString()),
		DisplayName: displayName,
		Email:       email,
		Username:    username,
		// Deliberately empty: there is no local credential for this account.
		PasswordHash: "",
		Status:       domain.UserStatusActive,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	created, err := s.insertUser(ctx, u)
	if err == nil {
		return created, nil
	}
	if role == domain.UserRoleOwner && isOwnerUniqueConstraintErr(err) {
		// Lost the owner race to a concurrent first login. The account is
		// still legitimate; it is simply not the owner.
		u.Role = domain.UserRoleMember
		created, err = s.insertUser(ctx, u)
		if err == nil {
			return created, nil
		}
	}
	if isUniqueConstraintErr(err) {
		return domain.User{}, apierr.Conflict("USER_ALREADY_EXISTS", "a user with that email or username already exists", nil)
	}
	return domain.User{}, fmt.Errorf("create federated user: %w", err)
}

// maxUsernameAttempts bounds the collision-resolution loop. Reaching it means
// something is pathological, and looping forever inside a login request is a
// worse failure than a clear error.
const maxUsernameAttempts = 20

// availableUsername picks the first unused username from the preferred
// candidate, then the email, then numbered suffixes.
func (s *Service) availableUsername(ctx context.Context, preferred, email string) (string, error) {
	base := strings.ToLower(strings.TrimSpace(preferred))
	if base == "" {
		base = email
	}
	for attempt := 0; attempt < maxUsernameAttempts; attempt++ {
		candidate := base
		if attempt == 1 {
			candidate = email
		} else if attempt > 1 {
			candidate = fmt.Sprintf("%s-%d", base, attempt)
		}
		_, taken, err := s.store.GetUserByUsername(ctx, candidate)
		if err != nil {
			return "", fmt.Errorf("lookup user by username: %w", err)
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", apierr.Conflict("USERNAME_UNAVAILABLE", "could not derive an unused username for this identity", nil)
}

// CreateSessionAs issues a session that records HOW it was authenticated.
// issuer/subject are stored only for domain.AuthMethodOIDC; they are the
// federated identity behind the session, and they are what lets a later
// authorization decision (P4-B) or an audit reader tell a provider-backed
// session from a local one without re-parsing anything.
//
// The provider's own access and refresh tokens are deliberately not accepted
// here and not stored anywhere: AO issues its own opaque session token and
// never re-presents a provider token as a browser session.
func (s *Service) CreateSessionAs(ctx context.Context, userID domain.UserID, method domain.AuthMethod, issuer, subject string) (string, domain.AuthSession, error) {
	raw, err := randomToken()
	if err != nil {
		return "", domain.AuthSession{}, fmt.Errorf("generate session token: %w", err)
	}
	if method == "" {
		method = domain.AuthMethodPassword
	}
	if method != domain.AuthMethodOIDC {
		issuer, subject = "", ""
	}
	now := s.now().UTC()
	sess := domain.AuthSession{
		ID:         uuid.NewString(),
		UserID:     userID,
		TokenHash:  hashToken(raw),
		AuthMethod: method,
		Issuer:     issuer,
		Subject:    subject,
		CreatedAt:  now,
		ExpiresAt:  now.Add(SessionTTL),
		LastSeenAt: now,
	}
	created, err := s.store.InsertAuthSession(ctx, sess)
	if err != nil {
		return "", domain.AuthSession{}, fmt.Errorf("create session: %w", err)
	}
	return raw, created, nil
}

// ResolvePrincipal is the canonical "who is making this request" resolution:
// one place that turns a session token into a resolved user PLUS the method,
// session and federated identity behind it. Everything above the middleware
// reads domain.Principal and never re-derives any of this.
func (s *Service) ResolvePrincipal(ctx context.Context, rawToken string) (domain.Principal, error) {
	notFound := apierr.NotFound("SESSION_NOT_FOUND", "session not found, revoked, or expired")

	if strings.TrimSpace(rawToken) == "" {
		return domain.Principal{}, apierr.NotFound("SESSION_NOT_FOUND", "no session")
	}
	sess, ok, err := s.store.GetAuthSessionByTokenHash(ctx, hashToken(rawToken))
	if err != nil {
		return domain.Principal{}, fmt.Errorf("lookup session: %w", err)
	}
	now := s.now().UTC()
	if !ok || sess.RevokedAt != nil {
		return domain.Principal{}, notFound
	}
	if !sess.ExpiresAt.After(now) {
		return domain.Principal{}, apierr.NotFound("SESSION_EXPIRED", "session expired")
	}
	u, ok, err := s.store.GetUserByID(ctx, sess.UserID)
	if err != nil {
		return domain.Principal{}, fmt.Errorf("lookup session user: %w", err)
	}
	if !ok || u.Status != domain.UserStatusActive {
		return domain.Principal{}, apierr.NotFound("SESSION_NOT_FOUND", "session user not found or disabled")
	}
	// Best-effort last-seen touch; a failure here must never fail resolution.
	_, _ = s.store.TouchAuthSessionLastSeen(ctx, sess.ID, now)

	method := sess.AuthMethod
	if method == "" {
		// Rows written before migration 0151 backfilled the column.
		method = domain.AuthMethodPassword
	}
	return domain.Principal{
		User:       u,
		AuthMethod: method,
		SessionID:  sess.ID,
		Issuer:     sess.Issuer,
		Subject:    sess.Subject,
	}, nil
}

// TrustedLocalPrincipal wraps a synthesized trusted-local identity in the same
// shape a real session produces, so no caller has to special-case it. It has
// no session id, because no session was ever issued: that is a fact about the
// request, recorded rather than hidden.
func TrustedLocalPrincipal(u domain.User) domain.Principal {
	return domain.Principal{User: u, AuthMethod: domain.AuthMethodTrustedLocal}
}

// SessionExpiry reports when a freshly issued session would expire, for the
// callers that need to stamp a cookie without minting one first.
func SessionExpiry(now time.Time) time.Time { return now.Add(SessionTTL) }
