package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// InsertUser creates a new user row. Callers (authsvc) are responsible for
// hashing the password before it reaches here — this layer never sees a
// plaintext credential.
func (s *Store) InsertUser(ctx context.Context, u domain.User) (domain.User, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.InsertUser(ctx, gen.InsertUserParams{
		ID:           u.ID,
		DisplayName:  u.DisplayName,
		Email:        u.Email,
		Username:     u.Username,
		PasswordHash: u.PasswordHash,
		Status:       u.Status,
		Role:         u.Role,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("insert user: %w", err)
	}
	return userFromRow(row), nil
}

// GetUserByID returns a user by id, or (zero, false, nil) if none exists.
func (s *Store) GetUserByID(ctx context.Context, id domain.UserID) (domain.User, bool, error) {
	row, err := s.qr.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, false, nil
		}
		return domain.User{}, false, fmt.Errorf("get user %s: %w", id, err)
	}
	return userFromRow(row), true, nil
}

// GetUserByEmail returns a user by exact email match, or (zero, false, nil).
func (s *Store) GetUserByEmail(ctx context.Context, email string) (domain.User, bool, error) {
	row, err := s.qr.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, false, nil
		}
		return domain.User{}, false, fmt.Errorf("get user by email: %w", err)
	}
	return userFromRow(row), true, nil
}

// GetUserByUsername returns a user by exact username match, or (zero, false, nil).
func (s *Store) GetUserByUsername(ctx context.Context, username string) (domain.User, bool, error) {
	row, err := s.qr.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, false, nil
		}
		return domain.User{}, false, fmt.Errorf("get user by username: %w", err)
	}
	return userFromRow(row), true, nil
}

// CountUsers returns the total number of user rows — used at daemon startup
// to decide whether bootstrap admin creation applies (zero rows only).
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	n, err := s.qr.CountUsers(ctx)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// ListUsers returns every user row. Used by admin tooling only; there is no
// pagination because operator-scale user counts are expected.
func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.qr.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	out := make([]domain.User, 0, len(rows))
	for _, r := range rows {
		out = append(out, userFromRow(r))
	}
	return out, nil
}

// UpdateUserPasswordHash sets a new bcrypt hash for a user (e.g. the
// loopback-only admin password-reset path). Returns false if the user id
// doesn't exist.
func (s *Store) UpdateUserPasswordHash(ctx context.Context, id domain.UserID, hash string, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateUserPasswordHash(ctx, gen.UpdateUserPasswordHashParams{
		PasswordHash: hash,
		UpdatedAt:    updatedAt,
		ID:           id,
	})
	if err != nil {
		return false, fmt.Errorf("update user password hash %s: %w", id, err)
	}
	return n > 0, nil
}

// UpdateUserRole sets a user's role. Returns false if the user id doesn't
// exist. Callers are responsible for the single-owner invariant -- the
// ux_users_single_owner partial unique index enforces it at the SQL layer.
func (s *Store) UpdateUserRole(ctx context.Context, id domain.UserID, role domain.UserRole, updatedAt time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateUserRole(ctx, gen.UpdateUserRoleParams{
		Role:      role,
		UpdatedAt: updatedAt,
		ID:        id,
	})
	if err != nil {
		return false, fmt.Errorf("update user role %s: %w", id, err)
	}
	return n > 0, nil
}

// CountOwners returns how many users currently hold UserRoleOwner -- at most
// 1, enforced by ux_users_single_owner.
func (s *Store) CountOwners(ctx context.Context) (int64, error) {
	n, err := s.qr.CountOwners(ctx)
	if err != nil {
		return 0, fmt.Errorf("count owners: %w", err)
	}
	return n, nil
}

func userFromRow(row gen.User) domain.User {
	return domain.User{
		ID:           row.ID,
		DisplayName:  row.DisplayName,
		Email:        row.Email,
		Username:     row.Username,
		PasswordHash: row.PasswordHash,
		Status:       row.Status,
		Role:         row.Role,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
}
