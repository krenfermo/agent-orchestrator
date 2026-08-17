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

	InsertAuthSession(ctx context.Context, sess domain.AuthSession) (domain.AuthSession, error)
	GetAuthSessionByTokenHash(ctx context.Context, tokenHash string) (domain.AuthSession, bool, error)
	TouchAuthSessionLastSeen(ctx context.Context, id string, at time.Time) (bool, error)
	RevokeAuthSessionByTokenHash(ctx context.Context, tokenHash string, at time.Time) (bool, error)

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
	if len(in.Password) < 8 {
		return domain.User{}, apierr.Invalid("USER_PASSWORD_TOO_SHORT", "password must be at least 8 characters", nil)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return domain.User{}, fmt.Errorf("hash password: %w", err)
	}
	now := s.now().UTC()
	u := domain.User{
		ID:           domain.UserID(uuid.NewString()),
		DisplayName:  displayName,
		Email:        email,
		Username:     username,
		PasswordHash: string(hash),
		Status:       domain.UserStatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	created, err := s.store.InsertUser(ctx, u)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return domain.User{}, apierr.Conflict("USER_ALREADY_EXISTS", "a user with that email or username already exists", nil)
		}
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	return created, nil
}

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

var errUniqueViolation = errors.New("unique constraint violation")
