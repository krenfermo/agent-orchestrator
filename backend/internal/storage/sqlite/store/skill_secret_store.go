package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// This file is the durable half of scoped secret delivery (migration 0162).
//
// Nothing here ever sees plaintext. Values arrive already sealed by
// internal/secretbox and leave the same way; the service that holds the box is
// the only layer that can open one, which keeps the store's error paths — the
// ones that end up in logs — structurally unable to leak.

// SealedSecret is one stored secret. The name is a reference and is safe to
// log; SealedValue is ciphertext.
type SealedSecret struct {
	Name        string
	Description string
	SealedValue string
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
}

// SecretSummary is the shape a listing returns: everything EXCEPT the value.
// It is a separate type rather than SealedSecret-with-a-blank-field so no
// caller can accidentally serialize the ciphertext into a response.
type SecretSummary struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
	CreatedBy   string    `json:"createdBy,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// UpsertSkillSecret stores an already-sealed value.
func (s *Store) UpsertSkillSecret(ctx context.Context, rec SealedSecret) (SealedSecret, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillSecret(ctx, gen.UpsertSkillSecretParams{
		Name:        rec.Name,
		Description: rec.Description,
		SealedValue: rec.SealedValue,
		CreatedAt:   rec.CreatedAt,
		CreatedBy:   rec.CreatedBy,
		UpdatedAt:   rec.UpdatedAt,
	})
	if err != nil {
		return SealedSecret{}, fmt.Errorf("upsert skill secret %q: %w", rec.Name, err)
	}
	return SealedSecret(row), nil
}

// GetSkillSecret returns one sealed secret.
func (s *Store) GetSkillSecret(ctx context.Context, name string) (SealedSecret, bool, error) {
	row, err := s.qr.GetSkillSecret(ctx, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SealedSecret{}, false, nil
		}
		return SealedSecret{}, false, fmt.Errorf("get skill secret %q: %w", name, err)
	}
	return SealedSecret(row), true, nil
}

// ListSkillSecretNames returns every secret WITHOUT its value. The query does
// not select the ciphertext column at all, so a listing cannot carry one.
func (s *Store) ListSkillSecretNames(ctx context.Context) ([]SecretSummary, error) {
	rows, err := s.qr.ListSkillSecretNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill secrets: %w", err)
	}
	out := make([]SecretSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, SecretSummary{
			Name: row.Name, Description: row.Description,
			CreatedAt: row.CreatedAt, CreatedBy: row.CreatedBy, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// DeleteSkillSecret removes a secret. Its grants cascade.
func (s *Store) DeleteSkillSecret(ctx context.Context, name string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteSkillSecret(ctx, name)
	if err != nil {
		return false, fmt.Errorf("delete skill secret %q: %w", name, err)
	}
	return n > 0, nil
}

// UpsertSkillSecretGrant writes one grant, replacing any grant for the same
// (secret, scope) so "what may this scope read" has exactly one answer.
func (s *Store) UpsertSkillSecretGrant(ctx context.Context, g skillsecrets.Grant) (skillsecrets.Grant, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillSecretGrant(ctx, gen.UpsertSkillSecretGrantParams{
		ID:         g.ID,
		SecretName: string(g.Ref),
		TenantID:   g.Scope.TenantID,
		ProjectID:  g.Scope.ProjectID,
		SkillID:    g.Scope.SkillID,
		Version:    g.Scope.Version,
		ModeID:     g.Scope.ModeID,
		GrantedBy:  g.GrantedBy,
		GrantedAt:  g.GrantedAt,
		ExpiresAt:  g.ExpiresAt,
		RevokedAt:  timePtrToNullTime(g.RevokedAt),
	})
	if err != nil {
		return skillsecrets.Grant{}, fmt.Errorf("upsert secret grant: %w", err)
	}
	return secretGrantFromRow(row), nil
}

// ListSkillSecretGrantsForScope returns every grant written for one exact
// scope, active or not. Expiry and revocation are decided in Go so the same
// answer holds for any store.
func (s *Store) ListSkillSecretGrantsForScope(ctx context.Context, scope skillsecrets.Scope) ([]skillsecrets.Grant, error) {
	rows, err := s.qr.ListSkillSecretGrantsForScope(ctx, gen.ListSkillSecretGrantsForScopeParams{
		TenantID:  scope.TenantID,
		ProjectID: scope.ProjectID,
		SkillID:   scope.SkillID,
		Version:   scope.Version,
		ModeID:    scope.ModeID,
	})
	if err != nil {
		return nil, fmt.Errorf("list secret grants for scope: %w", err)
	}
	out := make([]skillsecrets.Grant, 0, len(rows))
	for _, row := range rows {
		out = append(out, secretGrantFromRow(row))
	}
	return out, nil
}

// ListSkillSecretGrantsForSecret returns every grant on one secret.
func (s *Store) ListSkillSecretGrantsForSecret(ctx context.Context, name string) ([]skillsecrets.Grant, error) {
	rows, err := s.qr.ListSkillSecretGrantsForSecret(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("list secret grants: %w", err)
	}
	out := make([]skillsecrets.Grant, 0, len(rows))
	for _, row := range rows {
		out = append(out, secretGrantFromRow(row))
	}
	return out, nil
}

// RevokeSkillSecretGrant marks one grant revoked. It reports whether a row
// changed, so a caller can tell "revoked now" from "already revoked".
func (s *Store) RevokeSkillSecretGrant(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeSkillSecretGrant(ctx, gen.RevokeSkillSecretGrantParams{
		RevokedAt: sql.NullTime{Time: at, Valid: true}, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("revoke secret grant: %w", err)
	}
	return n > 0, nil
}

// RevokeSkillSecretGrantsForSecret revokes every grant on one secret at once,
// which is what "this credential leaked" needs.
func (s *Store) RevokeSkillSecretGrantsForSecret(ctx context.Context, name string, at time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeSkillSecretGrantsForSecret(ctx, gen.RevokeSkillSecretGrantsForSecretParams{
		RevokedAt: sql.NullTime{Time: at, Valid: true}, SecretName: name,
	})
	if err != nil {
		return 0, fmt.Errorf("revoke secret grants: %w", err)
	}
	return n, nil
}

// InsertSkillSecretLease records one attempt's single-use right. The unique
// index on (run_id, attempt_id) means a second lease for the same attempt is a
// constraint violation rather than a silent duplicate.
func (s *Store) InsertSkillSecretLease(ctx context.Context, l skillsecrets.Lease) (skillsecrets.Lease, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	refs, err := json.Marshal(secretRefNames(l.Refs))
	if err != nil {
		return skillsecrets.Lease{}, fmt.Errorf("marshal lease refs: %w", err)
	}
	row, err := s.qw.InsertSkillSecretLease(ctx, gen.InsertSkillSecretLeaseParams{
		ID:         l.ID,
		TenantID:   l.Scope.TenantID,
		ProjectID:  l.Scope.ProjectID,
		SkillID:    l.Scope.SkillID,
		Version:    l.Scope.Version,
		ModeID:     l.Scope.ModeID,
		RunID:      l.RunID,
		AttemptID:  l.AttemptID,
		Refs:       string(refs),
		IssuedAt:   l.IssuedAt,
		ExpiresAt:  l.ExpiresAt,
		ConsumedAt: timePtrToNullTime(l.ConsumedAt),
	})
	if err != nil {
		return skillsecrets.Lease{}, fmt.Errorf("insert secret lease: %w", err)
	}
	return secretLeaseFromRow(row)
}

// GetSkillSecretLease returns one lease.
func (s *Store) GetSkillSecretLease(ctx context.Context, id string) (skillsecrets.Lease, bool, error) {
	row, err := s.qr.GetSkillSecretLease(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillsecrets.Lease{}, false, nil
		}
		return skillsecrets.Lease{}, false, fmt.Errorf("get secret lease: %w", err)
	}
	lease, err := secretLeaseFromRow(row)
	return lease, err == nil, err
}

// ConsumeSkillSecretLease marks a lease redeemed, and reports whether IT did
// the marking. The UPDATE carries the unconsumed-and-unexpired condition, so
// two concurrent deliveries cannot both win: exactly one sees a row change.
func (s *Store) ConsumeSkillSecretLease(ctx context.Context, id string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.ConsumeSkillSecretLease(ctx, gen.ConsumeSkillSecretLeaseParams{
		ConsumedAt: sql.NullTime{Time: now, Valid: true}, ID: id, ExpiresAt: now,
	})
	if err != nil {
		return false, fmt.Errorf("consume secret lease: %w", err)
	}
	return n > 0, nil
}

// DeleteExpiredSkillSecretLeases prunes leases nobody can redeem.
func (s *Store) DeleteExpiredSkillSecretLeases(ctx context.Context, before time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.DeleteExpiredSkillSecretLeases(ctx, before)
	if err != nil {
		return 0, fmt.Errorf("prune secret leases: %w", err)
	}
	return n, nil
}

func secretGrantFromRow(row gen.SkillSecretGrant) skillsecrets.Grant {
	g := skillsecrets.Grant{
		ID:  row.ID,
		Ref: skillsecrets.Ref(row.SecretName),
		Scope: skillsecrets.Scope{
			TenantID:  row.TenantID,
			ProjectID: row.ProjectID,
			SkillID:   row.SkillID,
			Version:   row.Version,
			ModeID:    row.ModeID,
		},
		GrantedBy: row.GrantedBy,
		GrantedAt: row.GrantedAt,
		ExpiresAt: row.ExpiresAt,
	}
	if row.RevokedAt.Valid {
		revoked := row.RevokedAt.Time
		g.RevokedAt = &revoked
	}
	return g
}

func secretLeaseFromRow(row gen.SkillSecretLease) (skillsecrets.Lease, error) {
	var names []string
	if err := json.Unmarshal([]byte(row.Refs), &names); err != nil {
		return skillsecrets.Lease{}, fmt.Errorf("decode lease refs for %s: %w", row.ID, err)
	}
	refs := make([]skillsecrets.Ref, 0, len(names))
	for _, n := range names {
		refs = append(refs, skillsecrets.Ref(n))
	}
	l := skillsecrets.Lease{
		ID: row.ID,
		Scope: skillsecrets.Scope{
			TenantID:  row.TenantID,
			ProjectID: row.ProjectID,
			SkillID:   row.SkillID,
			Version:   row.Version,
			ModeID:    row.ModeID,
		},
		RunID:     row.RunID,
		AttemptID: row.AttemptID,
		Refs:      refs,
		IssuedAt:  row.IssuedAt,
		ExpiresAt: row.ExpiresAt,
	}
	if row.ConsumedAt.Valid {
		consumed := row.ConsumedAt.Time
		l.ConsumedAt = &consumed
	}
	return l, nil
}

func secretRefNames(refs []skillsecrets.Ref) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, string(r))
	}
	return out
}
