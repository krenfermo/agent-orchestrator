// Package authsvc implements Checkpoint 8P-A's user identity, credential,
// and session lifecycle: bcrypt password hashing, opaque session tokens
// (only their SHA-256 hash is ever persisted), login lockout, and the
// one-time bootstrap-admin creation + ownership backfill run at daemon
// startup. It never receives a client-supplied user id — every "current
// user" resolution goes through ResolveSession, off a server-issued cookie.
package authsvc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// SessionTTL is how long an issued session cookie stays valid.
const SessionTTL = 30 * 24 * time.Hour

// loginLockoutLimit/loginLockoutCooldown mirror httpd/auth.go's mobile
// pairing lockout (5 attempts, cooldown) for this second credential surface.
const (
	loginLockoutLimit    = 5
	loginLockoutCooldown = 5 * time.Minute
)

// Store is the durable persistence surface Manager depends on. Backed by
// storage/sqlite/store.Store in production.
type Store interface {
	InsertUser(ctx context.Context, u domain.User) (domain.User, error)
	GetUserByID(ctx context.Context, id domain.UserID) (domain.User, bool, error)
	GetUserByEmail(ctx context.Context, email string) (domain.User, bool, error)
	GetUserByUsername(ctx context.Context, username string) (domain.User, bool, error)
	CountUsers(ctx context.Context) (int64, error)
	ListUsers(ctx context.Context) ([]domain.User, error)

	UpdateUserPasswordHash(ctx context.Context, id domain.UserID, hash string, updatedAt time.Time) (bool, error)
	UpdateUserRole(ctx context.Context, id domain.UserID, role domain.UserRole, updatedAt time.Time) (bool, error)
	CountOwners(ctx context.Context) (int64, error)

	InsertAuthSession(ctx context.Context, sess domain.AuthSession) (domain.AuthSession, error)
	GetAuthSessionByTokenHash(ctx context.Context, tokenHash string) (domain.AuthSession, bool, error)
	TouchAuthSessionLastSeen(ctx context.Context, id string, at time.Time) (bool, error)
	RevokeAuthSessionByTokenHash(ctx context.Context, tokenHash string, at time.Time) (bool, error)
	RevokeAllAuthSessionsForUser(ctx context.Context, userID domain.UserID, at time.Time) (int64, error)

	BackfillProjectOwners(ctx context.Context, owner domain.UserID) (int64, error)
	BackfillWorkflowRunOwners(ctx context.Context, owner domain.UserID) (int64, error)
}

// CreateUserInput is the input to CreateUser. Password is plaintext in
// memory only for the duration of the bcrypt hash call; it is never stored
// or logged.
type CreateUserInput struct {
	DisplayName string
	Email       string
	Username    string
	Password    string
	// Role defaults to domain.UserRoleMember when zero-valued. Callers must
	// opt in to domain.UserRoleOwner explicitly (Bootstrap, RegisterFirstUser)
	// -- it is never inferred.
	Role domain.UserRole
}

// BootstrapResult reports what Bootstrap did, for startup logging.
type BootstrapResult struct {
	// Created is true when a bootstrap admin user was created this run.
	Created bool
	// Skipped is true when zero users existed but no bootstrap env vars were
	// set, so the daemon started with no admin at all (a valid, logged state).
	Skipped bool
	// AdminID is set when Created is true.
	AdminID domain.UserID
	// BackfilledProjects/BackfilledWorkflowRuns count rows whose owner was
	// NULL and got stamped to AdminID.
	BackfilledProjects     int64
	BackfilledWorkflowRuns int64
}

// Manager is the service-facing identity surface.
type Manager interface {
	CreateUser(ctx context.Context, in CreateUserInput) (domain.User, error)
	// Authenticate verifies a username-or-email + password pair, honoring the
	// per-source lockout. sourceKey identifies the caller (e.g. remote IP) for
	// throttling purposes only. Returns a generic invalid-credentials error
	// both when the account doesn't exist and when the password is wrong —
	// never distinguishing the two on the wire.
	Authenticate(ctx context.Context, usernameOrEmail, password, sourceKey string) (domain.User, error)
	// CreateSession issues a new session for userID and returns the RAW token
	// (only ever returned here, for the Set-Cookie response) plus the
	// persisted session record.
	CreateSession(ctx context.Context, userID domain.UserID) (string, domain.AuthSession, error)
	// ResolveSession looks up the user for a raw cookie token. Returns
	// apierr.NotFound-kind error when the token is missing, unknown, revoked,
	// or expired.
	ResolveSession(ctx context.Context, rawToken string) (domain.User, error)
	// RevokeSession revokes the session for a raw cookie token. A no-op
	// (nil error) if the token doesn't match any active session.
	RevokeSession(ctx context.Context, rawToken string) error
	// Bootstrap runs the one-time first-admin creation + ownership backfill.
	// Safe to call every daemon startup: it only acts when zero users exist.
	Bootstrap(ctx context.Context, email, password string) (BootstrapResult, error)
	// SetupRequired reports whether the installation has zero users yet --
	// the canonical signal the frontend uses to decide whether to render the
	// first-run "Create your account" screen instead of Sign in.
	SetupRequired(ctx context.Context) (bool, error)
	// RegisterFirstUser creates the installation owner from the in-product
	// first-run flow (as opposed to Bootstrap's env-var path). Always
	// assigns domain.UserRoleOwner. Safe under concurrent calls: the
	// users(role) WHERE role='owner' partial unique index lets at most one
	// call ever succeed, and a losing call gets a distinct
	// SETUP_ALREADY_COMPLETED error rather than a generic conflict.
	RegisterFirstUser(ctx context.Context, in CreateUserInput) (domain.User, error)
	// ResetPassword re-hashes a user's password by email and revokes every
	// existing session for that user. Local-recovery tool only -- callers
	// must restrict this to a trusted-loopback surface; Manager itself
	// enforces no additional authorization.
	ResetPassword(ctx context.Context, email, newPassword string) error
	// EnsureOwnerExists is a one-time startup safeguard (Checkpoint 8P-E.8.1)
	// for installations whose sole user predates the role column (backfilled
	// to 'member' by migration 0116, never auto-promoted -- avoiding exactly
	// the "earliest-created user = admin" heuristic 8P-A flagged as debt).
	// It only acts when zero owners exist AND exactly one active user
	// exists, promoting that user via the same ux_users_single_owner-guarded
	// path every other owner-assignment uses. Safe to call every daemon
	// startup, same as Bootstrap. A no-op once an owner exists or when
	// zero/multiple users exist (the latter needs a human decision, not an
	// automatic promotion).
	EnsureOwnerExists(ctx context.Context) (bool, error)
	// BootstrapAdmin returns the identity trusted-local mode synthesizes when
	// no session cookie is present: the earliest-created active user. There is
	// no separate "is bootstrap admin" flag — the first account is, by
	// construction, the one Bootstrap created (or, if none was ever
	// bootstrapped but a user was created via other tooling, the oldest
	// account, which is the closest honest analogue). Returns ok=false when
	// no active user exists at all.
	BootstrapAdmin(ctx context.Context) (domain.User, bool, error)
}

// Service is the default Manager implementation.
type Service struct {
	store Store
	now   func() time.Time
	lock  *lockout
}

// New builds a Service. now defaults to time.Now when nil (tests may override).
func New(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now, lock: newLockout(loginLockoutLimit, loginLockoutCooldown, now)}
}

var _ Manager = (*Service)(nil)

// CreateUser creates a user with the given role and credentials, rejecting a
// username or email already in use.
func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (domain.User, error) {
	displayName := strings.TrimSpace(in.DisplayName)
	email := strings.ToLower(strings.TrimSpace(in.Email))
	username := strings.ToLower(strings.TrimSpace(in.Username))
	if email == "" || username == "" || in.Password == "" {
		return domain.User{}, apierr.Invalid("USER_INVALID_INPUT", "email, username, and password are required", nil)
	}
	if displayName == "" {
		displayName = username
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		return domain.User{}, err
	}
	role := in.Role
	if role == "" {
		role = domain.UserRoleMember
	}
	now := s.now().UTC()
	u := domain.User{
		ID:           domain.UserID(uuid.NewString()),
		DisplayName:  displayName,
		Email:        email,
		Username:     username,
		PasswordHash: hash,
		Status:       domain.UserStatusActive,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	created, err := s.store.InsertUser(ctx, u)
	if err != nil {
		if isOwnerUniqueConstraintErr(err) {
			return domain.User{}, apierr.Conflict("SETUP_ALREADY_COMPLETED", "this installation already has an owner account", nil)
		}
		if isUniqueConstraintErr(err) {
			return domain.User{}, apierr.Conflict("USER_ALREADY_EXISTS", "a user with that email or username already exists", nil)
		}
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	return created, nil
}

// Authenticate verifies a password against the user identified by username or
// email. Every failure returns the same INVALID_CREDENTIALS error, so a caller
// cannot use the response to learn whether an account exists.
func (s *Service) Authenticate(ctx context.Context, usernameOrEmail, password, sourceKey string) (domain.User, error) {
	invalid := apierr.Unauthorized("INVALID_CREDENTIALS", "invalid username/email or password")

	if s.lock.blocked(sourceKey) {
		return domain.User{}, apierr.New(apierr.KindConflict, "TOO_MANY_ATTEMPTS", "too many failed attempts; try again shortly", nil)
	}

	candidate := strings.ToLower(strings.TrimSpace(usernameOrEmail))
	if candidate == "" || password == "" {
		s.lock.fail(sourceKey)
		return domain.User{}, invalid
	}

	u, ok, err := s.store.GetUserByEmail(ctx, candidate)
	if err != nil {
		return domain.User{}, fmt.Errorf("lookup user by email: %w", err)
	}
	if !ok {
		u, ok, err = s.store.GetUserByUsername(ctx, candidate)
		if err != nil {
			return domain.User{}, fmt.Errorf("lookup user by username: %w", err)
		}
	}
	if !ok || u.Status != domain.UserStatusActive {
		// Same generic failure whether the account doesn't exist, or exists
		// but is disabled — existence must never leak on the wire.
		s.lock.fail(sourceKey)
		return domain.User{}, invalid
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		s.lock.fail(sourceKey)
		return domain.User{}, invalid
	}
	s.lock.reset(sourceKey)
	return u, nil
}

// CreateSession issues a new session for a user and returns the raw token
// alongside the stored record. The raw token is returned here and never again:
// only its hash is persisted.
func (s *Service) CreateSession(ctx context.Context, userID domain.UserID) (string, domain.AuthSession, error) {
	raw, err := randomToken()
	if err != nil {
		return "", domain.AuthSession{}, fmt.Errorf("generate session token: %w", err)
	}
	now := s.now().UTC()
	sess := domain.AuthSession{
		ID:         uuid.NewString(),
		UserID:     userID,
		TokenHash:  hashToken(raw),
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

// ResolveSession returns the user behind a raw session token, rejecting a
// token that is empty, unknown, revoked or expired.
func (s *Service) ResolveSession(ctx context.Context, rawToken string) (domain.User, error) {
	if strings.TrimSpace(rawToken) == "" {
		return domain.User{}, apierr.NotFound("SESSION_NOT_FOUND", "no session")
	}
	sess, ok, err := s.store.GetAuthSessionByTokenHash(ctx, hashToken(rawToken))
	if err != nil {
		return domain.User{}, fmt.Errorf("lookup session: %w", err)
	}
	now := s.now().UTC()
	if !ok || sess.RevokedAt != nil || !sess.ExpiresAt.After(now) {
		return domain.User{}, apierr.NotFound("SESSION_NOT_FOUND", "session not found, revoked, or expired")
	}
	u, ok, err := s.store.GetUserByID(ctx, sess.UserID)
	if err != nil {
		return domain.User{}, fmt.Errorf("lookup session user: %w", err)
	}
	if !ok || u.Status != domain.UserStatusActive {
		return domain.User{}, apierr.NotFound("SESSION_NOT_FOUND", "session user not found or disabled")
	}
	// Best-effort last-seen touch; a failure here must never fail resolution.
	_, _ = s.store.TouchAuthSessionLastSeen(ctx, sess.ID, now)
	return u, nil
}

// RevokeSession invalidates a raw session token. Revoking an unknown token is
// not an error: the postcondition the caller wants -- that the token no longer
// authenticates -- already holds.
func (s *Service) RevokeSession(ctx context.Context, rawToken string) error {
	if strings.TrimSpace(rawToken) == "" {
		return nil
	}
	_, err := s.store.RevokeAuthSessionByTokenHash(ctx, hashToken(rawToken), s.now().UTC())
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// Bootstrap creates the first owner on an empty instance. It refuses once any
// user exists, so it cannot be used to add a privileged account later.
func (s *Service) Bootstrap(ctx context.Context, email, password string) (BootstrapResult, error) {
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return BootstrapResult{}, nil
	}
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return BootstrapResult{Skipped: true}, nil
	}
	admin, err := s.CreateUser(ctx, CreateUserInput{
		DisplayName: "Admin",
		Email:       email,
		Username:    email,
		Password:    password,
		Role:        domain.UserRoleOwner,
	})
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("create bootstrap admin: %w", err)
	}
	projects, err := s.store.BackfillProjectOwners(ctx, admin.ID)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("backfill project owners: %w", err)
	}
	runs, err := s.store.BackfillWorkflowRunOwners(ctx, admin.ID)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("backfill workflow run owners: %w", err)
	}
	return BootstrapResult{
		Created:                true,
		AdminID:                admin.ID,
		BackfilledProjects:     projects,
		BackfilledWorkflowRuns: runs,
	}, nil
}

// BootstrapAdmin promotes an existing user to owner when an instance has users
// but no owner. The bool reports whether a promotion actually happened.
func (s *Service) BootstrapAdmin(ctx context.Context) (domain.User, bool, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return domain.User{}, false, fmt.Errorf("list users: %w", err)
	}
	var earliest domain.User
	found := false
	for _, u := range users {
		if u.Status != domain.UserStatusActive {
			continue
		}
		if !found || u.CreatedAt.Before(earliest.CreatedAt) || (u.CreatedAt.Equal(earliest.CreatedAt) && u.ID < earliest.ID) {
			earliest = u
			found = true
		}
	}
	return earliest, found, nil
}

// EnsureOwnerExists reports whether the instance has at least one owner,
// creating none: it is a check, not a repair.
func (s *Service) EnsureOwnerExists(ctx context.Context) (bool, error) {
	owners, err := s.store.CountOwners(ctx)
	if err != nil {
		return false, fmt.Errorf("count owners: %w", err)
	}
	if owners > 0 {
		return false, nil
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("list users: %w", err)
	}
	var active []domain.User
	for _, u := range users {
		if u.Status == domain.UserStatusActive {
			active = append(active, u)
		}
	}
	if len(active) != 1 {
		return false, nil
	}
	if _, err := s.store.UpdateUserRole(ctx, active[0].ID, domain.UserRoleOwner, s.now().UTC()); err != nil {
		return false, fmt.Errorf("promote sole user to owner: %w", err)
	}
	return true, nil
}

// SetupRequired reports whether the instance still has no users and therefore
// needs first-run setup.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return count == 0, nil
}

// RegisterFirstUser creates the instance's first user as an owner. The role is
// forced here rather than taken from the input, so the first-run endpoint
// cannot be used to create a non-owner and leave the instance ownerless.
func (s *Service) RegisterFirstUser(ctx context.Context, in CreateUserInput) (domain.User, error) {
	in.Role = domain.UserRoleOwner
	u, err := s.CreateUser(ctx, in)
	if err != nil {
		return domain.User{}, err
	}
	if _, err := s.store.BackfillProjectOwners(ctx, u.ID); err != nil {
		return domain.User{}, fmt.Errorf("backfill project owners: %w", err)
	}
	if _, err := s.store.BackfillWorkflowRunOwners(ctx, u.ID); err != nil {
		return domain.User{}, fmt.Errorf("backfill workflow run owners: %w", err)
	}
	return u, nil
}

// ResetPassword replaces the password of the user with the given email.
func (s *Service) ResetPassword(ctx context.Context, email, newPassword string) error {
	notFound := apierr.NotFound("USER_NOT_FOUND", "no user with that email")

	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return notFound
	}
	u, ok, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("lookup user by email: %w", err)
	}
	if !ok {
		return notFound
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	if _, err := s.store.UpdateUserPasswordHash(ctx, u.ID, hash, now); err != nil {
		return fmt.Errorf("update password hash: %w", err)
	}
	if _, err := s.store.RevokeAllAuthSessionsForUser(ctx, u.ID, now); err != nil {
		return fmt.Errorf("revoke sessions after password reset: %w", err)
	}
	return nil
}

// hashPassword validates and bcrypt-hashes a plaintext password, shared by
// CreateUser and ResetPassword so the length rule and cost factor never
// drift between the two paths.
func hashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", apierr.Invalid("USER_PASSWORD_TOO_SHORT", "password must be at least 8 characters", nil)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// isUniqueConstraintErr reports whether err looks like a SQLite UNIQUE
// constraint violation (email/username collision). Matched by substring
// since the sqlite driver's error type isn't imported here.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") || errors.Is(err, errUniqueViolation)
}

// isOwnerUniqueConstraintErr reports whether err is specifically the
// ux_users_single_owner partial unique index rejecting a second
// role='owner' insert -- distinct from the email/username collision
// isUniqueConstraintErr also matches, so RegisterFirstUser can return
// SETUP_ALREADY_COMPLETED instead of a generic USER_ALREADY_EXISTS.
func isOwnerUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "users.role") || strings.Contains(err.Error(), "ux_users_single_owner")
}

var errUniqueViolation = errors.New("unique constraint violation")
