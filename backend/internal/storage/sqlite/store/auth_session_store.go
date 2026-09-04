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

// InsertAuthSession creates a new session row. sess.TokenHash must already be
// the SHA-256 hash of the raw token — the raw token itself is never stored.
func (s *Store) InsertAuthSession(ctx context.Context, sess domain.AuthSession) (domain.AuthSession, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.InsertAuthSession(ctx, gen.InsertAuthSessionParams{
		ID:         sess.ID,
		UserID:     sess.UserID,
		TokenHash:  sess.TokenHash,
		AuthMethod: sess.AuthMethod,
		Issuer:     sess.Issuer,
		Subject:    sess.Subject,
		CreatedAt:  sess.CreatedAt,
		ExpiresAt:  sess.ExpiresAt,
		LastSeenAt: sess.LastSeenAt,
		RevokedAt:  timePtrToNullTime(sess.RevokedAt),
	})
	if err != nil {
		return domain.AuthSession{}, fmt.Errorf("insert auth session: %w", err)
	}
	return authSessionFromRow(row), nil
}

// GetAuthSessionByTokenHash returns the session row for a raw token's
// SHA-256 hash, or (zero, false, nil) if none exists. The caller is
// responsible for checking RevokedAt/ExpiresAt against "now" — this method
// does not filter, so ResolveSession can distinguish "no such session" from
// "session exists but is revoked/expired" for cleanup/telemetry purposes.
func (s *Store) GetAuthSessionByTokenHash(ctx context.Context, tokenHash string) (domain.AuthSession, bool, error) {
	row, err := s.qr.GetAuthSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.AuthSession{}, false, nil
		}
		return domain.AuthSession{}, false, fmt.Errorf("get auth session by token hash: %w", err)
	}
	return authSessionFromRow(row), true, nil
}

// TouchAuthSessionLastSeen updates last_seen_at for a still-active (non-revoked)
// session. Returns false if the session id doesn't exist or is already revoked.
func (s *Store) TouchAuthSessionLastSeen(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.TouchAuthSessionLastSeen(ctx, gen.TouchAuthSessionLastSeenParams{
		LastSeenAt: at,
		ID:         id,
	})
	if err != nil {
		return false, fmt.Errorf("touch auth session %s: %w", id, err)
	}
	return n > 0, nil
}

// RevokeAuthSessionByTokenHash marks a session revoked at the given time.
// Returns false if no matching non-revoked session was found.
func (s *Store) RevokeAuthSessionByTokenHash(ctx context.Context, tokenHash string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeAuthSessionByTokenHash(ctx, gen.RevokeAuthSessionByTokenHashParams{
		RevokedAt: timePtrToNullTime(&at),
		TokenHash: tokenHash,
	})
	if err != nil {
		return false, fmt.Errorf("revoke auth session: %w", err)
	}
	return n > 0, nil
}

// RevokeAllAuthSessionsForUser revokes every non-revoked session for a user
// (e.g. on password change/disable). Returns the number of rows revoked.
func (s *Store) RevokeAllAuthSessionsForUser(ctx context.Context, userID domain.UserID, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeAllAuthSessionsForUser(ctx, gen.RevokeAllAuthSessionsForUserParams{
		RevokedAt: timePtrToNullTime(&at),
		UserID:    userID,
	})
	if err != nil {
		return 0, fmt.Errorf("revoke auth sessions for user %s: %w", userID, err)
	}
	return n, nil
}

func authSessionFromRow(row gen.AuthSession) domain.AuthSession {
	return domain.AuthSession{
		ID:         row.ID,
		UserID:     row.UserID,
		TokenHash:  row.TokenHash,
		AuthMethod: row.AuthMethod,
		Issuer:     row.Issuer,
		Subject:    row.Subject,
		CreatedAt:  row.CreatedAt,
		ExpiresAt:  row.ExpiresAt,
		LastSeenAt: row.LastSeenAt,
		RevokedAt:  nullTimeToTimePtr(row.RevokedAt),
	}
}
