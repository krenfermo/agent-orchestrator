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

// InsertExternalIdentity links a federated (issuer, subject) to an AO user.
// The (issuer, subject) uniqueness is a database fact, not an application
// check, so two concurrent first-logins for the same external identity cannot
// both provision a user.
func (s *Store) InsertExternalIdentity(ctx context.Context, ident domain.ExternalIdentity) (domain.ExternalIdentity, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.InsertExternalIdentity(ctx, gen.InsertExternalIdentityParams{
		ID:            ident.ID,
		UserID:        ident.UserID,
		Issuer:        ident.Issuer,
		Subject:       ident.Subject,
		Email:         ident.Email,
		EmailVerified: ident.EmailVerified,
		DisplayName:   ident.DisplayName,
		CreatedAt:     ident.CreatedAt,
		UpdatedAt:     ident.UpdatedAt,
		LastLoginAt:   timePtrToNullTime(ident.LastLoginAt),
	})
	if err != nil {
		return domain.ExternalIdentity{}, fmt.Errorf("insert external identity: %w", err)
	}
	return externalIdentityFromRow(row), nil
}

// GetExternalIdentityByIssuerSubject resolves the canonical external identity
// key, or (zero, false, nil) when this issuer has never seen this subject.
func (s *Store) GetExternalIdentityByIssuerSubject(ctx context.Context, issuer, subject string) (domain.ExternalIdentity, bool, error) {
	row, err := s.qr.GetExternalIdentityByIssuerSubject(ctx, gen.GetExternalIdentityByIssuerSubjectParams{
		Issuer:  issuer,
		Subject: subject,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ExternalIdentity{}, false, nil
		}
		return domain.ExternalIdentity{}, false, fmt.Errorf("get external identity: %w", err)
	}
	return externalIdentityFromRow(row), true, nil
}

// ListExternalIdentitiesForUser returns every federated identity linked to a
// user (one per issuer, at most).
func (s *Store) ListExternalIdentitiesForUser(ctx context.Context, userID domain.UserID) ([]domain.ExternalIdentity, error) {
	rows, err := s.qr.ListExternalIdentitiesForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list external identities for user %s: %w", userID, err)
	}
	out := make([]domain.ExternalIdentity, 0, len(rows))
	for _, row := range rows {
		out = append(out, externalIdentityFromRow(row))
	}
	return out, nil
}

// UpdateExternalIdentityClaims refreshes the display-only claim snapshot after
// a successful login. It never touches issuer/subject: those are the identity,
// and a provider that changed them would be a different identity.
func (s *Store) UpdateExternalIdentityClaims(ctx context.Context, id, email string, emailVerified bool, displayName string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.UpdateExternalIdentityClaims(ctx, gen.UpdateExternalIdentityClaimsParams{
		Email:         email,
		EmailVerified: emailVerified,
		DisplayName:   displayName,
		UpdatedAt:     at,
		LastLoginAt:   timePtrToNullTime(&at),
		ID:            id,
	})
	if err != nil {
		return false, fmt.Errorf("update external identity %s: %w", id, err)
	}
	return n > 0, nil
}

func externalIdentityFromRow(row gen.ExternalIdentity) domain.ExternalIdentity {
	return domain.ExternalIdentity{
		ID:            row.ID,
		UserID:        row.UserID,
		Issuer:        row.Issuer,
		Subject:       row.Subject,
		Email:         row.Email,
		EmailVerified: row.EmailVerified,
		DisplayName:   row.DisplayName,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
		LastLoginAt:   nullTimeToTimePtr(row.LastLoginAt),
	}
}
