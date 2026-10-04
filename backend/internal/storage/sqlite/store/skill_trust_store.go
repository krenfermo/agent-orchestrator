package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// skill_trust_store.go -- the durable half of what AO will verify against
// (migration 0166).
//
// Three kinds of row:
//
//	skill_trust_roots        the anchors this installation chains to
//	skill_signing_keys       the public keys under them
//	skill_trust_revocations  what an ADMINISTRATOR here withdrew, globally
//
// # There is no private key in this file
//
// Nor a column one could go in, nor a function that would write one. The
// daemon verifies; it does not sign. Everything stored here is public: a key
// id, 32 bytes of public key, a fingerprint derived from those bytes, and a
// validity window.
//
// # The one write this store refuses
//
// UpsertSkillSigningKey will not change a key's material. A key id is
// permanent and the key under it never changes, so an upsert whose public key
// differs from the stored one is either a mistake or the substitution attack,
// and both end the same way. The refusal is here AND the column is absent from
// the query's SET list, because a check in one layer is a check somebody
// eventually calls around.

// ErrSigningKeySubstitution is returned when an upsert would change the
// material under an existing key id. It is a sentinel because the service maps
// it onto a distinct refusal and an audit line of its own: somebody trying to
// replace a trusted key is a different event from somebody mistyping a form.
var ErrSigningKeySubstitution = errors.New("store: a signing key's material cannot be replaced")

// UpsertSkillTrustRoot writes one trust root.
//
// The tier and the publisher are INSERT-only -- the query's SET list omits
// them -- so an existing enterprise root cannot be edited into an official
// one, and a root cannot be repointed at a different publisher after keys have
// chained to it.
func (s *Store) UpsertSkillTrustRoot(
	ctx context.Context, root skillregistry.TrustRoot,
) (skillregistry.TrustRoot, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillTrustRoot(ctx, gen.UpsertSkillTrustRootParams{
		ID:               root.ID,
		Tier:             string(root.Tier),
		DisplayName:      root.DisplayName,
		Publisher:        root.Publisher,
		Status:           string(root.Status),
		ValidFrom:        root.ValidFrom.UTC(),
		ValidUntil:       timePtrToNullTime(root.ValidUntil),
		RevokedAt:        timePtrToNullTime(root.RevokedAt),
		RevocationReason: root.RevocationReason,
		CreatedAt:        root.CreatedAt.UTC(),
		CreatedBy:        root.CreatedBy,
		UpdatedAt:        root.UpdatedAt.UTC(),
		UpdatedBy:        root.UpdatedBy,
	})
	if err != nil {
		return skillregistry.TrustRoot{}, fmt.Errorf("upsert skill trust root: %w", err)
	}
	return trustRootFromRow(row), nil
}

// GetSkillTrustRoot returns one root, or (zero, false, nil).
func (s *Store) GetSkillTrustRoot(
	ctx context.Context, id string,
) (skillregistry.TrustRoot, bool, error) {
	row, err := s.qr.GetSkillTrustRoot(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillregistry.TrustRoot{}, false, nil
		}
		return skillregistry.TrustRoot{}, false, fmt.Errorf("get skill trust root: %w", err)
	}
	return trustRootFromRow(row), true, nil
}

// ListSkillTrustRoots returns every configured root.
func (s *Store) ListSkillTrustRoots(ctx context.Context) ([]skillregistry.TrustRoot, error) {
	rows, err := s.qr.ListSkillTrustRoots(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill trust roots: %w", err)
	}
	out := make([]skillregistry.TrustRoot, 0, len(rows))
	for _, row := range rows {
		out = append(out, trustRootFromRow(row))
	}
	return out, nil
}

// RevokeSkillTrustRoot marks one root revoked. It reports whether a row moved;
// a second revocation moves none, so the first reason and timestamp stand.
func (s *Store) RevokeSkillTrustRoot(
	ctx context.Context, id, reason string, at time.Time, actor string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeSkillTrustRoot(ctx, gen.RevokeSkillTrustRootParams{
		RevokedAt: sql.NullTime{Time: at.UTC(), Valid: true}, RevocationReason: reason,
		UpdatedAt: at.UTC(), UpdatedBy: actor, ID: id,
	})
	if err != nil {
		return false, fmt.Errorf("revoke skill trust root: %w", err)
	}
	return n > 0, nil
}

// UpsertSkillSigningKey writes one public key.
//
// It reads before it writes, which is the whole point: the pre-read is what
// turns "this key id already exists with different bytes" into a refusal
// rather than into an UPDATE. Under s.writeMu, so the read and the write are
// one decision.
func (s *Store) UpsertSkillSigningKey(
	ctx context.Context, key skillregistry.SigningKey,
) (skillregistry.SigningKey, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	existing, err := s.qr.GetSkillSigningKey(ctx, key.KeyID)
	switch {
	case err == nil:
		if existing.PublicKey != key.PublicKey {
			return skillregistry.SigningKey{}, fmt.Errorf(
				"%w: key %s is already configured with fingerprint %s and this one hashes to %s. "+
					"A key id is permanent; rotating means a NEW key id, so that the old key's "+
					"signatures stay attributable to the old key",
				ErrSigningKeySubstitution, key.KeyID, existing.Fingerprint, key.Fingerprint)
		}
		if existing.TrustRootID != key.TrustRootID {
			return skillregistry.SigningKey{}, fmt.Errorf(
				"%w: key %s is anchored to trust root %s and this one names %s",
				ErrSigningKeySubstitution, key.KeyID, existing.TrustRootID, key.TrustRootID)
		}
	case errors.Is(err, sql.ErrNoRows):
		// New key. Nothing to compare against.
	default:
		return skillregistry.SigningKey{}, fmt.Errorf("read signing key before upsert: %w", err)
	}

	row, err := s.qw.UpsertSkillSigningKey(ctx, gen.UpsertSkillSigningKeyParams{
		KeyID:            key.KeyID,
		TrustRootID:      key.TrustRootID,
		Publisher:        key.Publisher,
		IsRootKey:        boolToInt64(key.IsRootKey),
		Algorithm:        key.Algorithm,
		PublicKey:        key.PublicKey,
		Fingerprint:      key.Fingerprint,
		Origin:           string(key.Origin),
		Status:           string(key.Status),
		ValidFrom:        key.ValidFrom.UTC(),
		ValidUntil:       timePtrToNullTime(key.ValidUntil),
		RevokedAt:        timePtrToNullTime(key.RevokedAt),
		RevocationReason: key.RevocationReason,
		RotatedFromKeyID: key.RotatedFromKeyID,
		CreatedAt:        key.CreatedAt.UTC(),
		CreatedBy:        key.CreatedBy,
		UpdatedAt:        key.UpdatedAt.UTC(),
		UpdatedBy:        key.UpdatedBy,
	})
	if err != nil {
		return skillregistry.SigningKey{}, fmt.Errorf("upsert skill signing key: %w", err)
	}
	return signingKeyFromRow(row), nil
}

// GetSkillSigningKey returns one key, or (zero, false, nil).
func (s *Store) GetSkillSigningKey(
	ctx context.Context, keyID string,
) (skillregistry.SigningKey, bool, error) {
	row, err := s.qr.GetSkillSigningKey(ctx, keyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillregistry.SigningKey{}, false, nil
		}
		return skillregistry.SigningKey{}, false, fmt.Errorf("get skill signing key: %w", err)
	}
	return signingKeyFromRow(row), true, nil
}

// ListSkillSigningKeys returns every key AO holds.
func (s *Store) ListSkillSigningKeys(ctx context.Context) ([]skillregistry.SigningKey, error) {
	rows, err := s.qr.ListSkillSigningKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill signing keys: %w", err)
	}
	out := make([]skillregistry.SigningKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, signingKeyFromRow(row))
	}
	return out, nil
}

// ListSkillSigningKeysForRoot returns the keys under one root.
func (s *Store) ListSkillSigningKeysForRoot(
	ctx context.Context, rootID string,
) ([]skillregistry.SigningKey, error) {
	rows, err := s.qr.ListSkillSigningKeysForRoot(ctx, rootID)
	if err != nil {
		return nil, fmt.Errorf("list skill signing keys for root: %w", err)
	}
	out := make([]skillregistry.SigningKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, signingKeyFromRow(row))
	}
	return out, nil
}

// RevokeSkillSigningKey marks one key revoked. One-way; a second call moves no
// row.
func (s *Store) RevokeSkillSigningKey(
	ctx context.Context, keyID, reason string, at time.Time, actor string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	n, err := s.qw.RevokeSkillSigningKey(ctx, gen.RevokeSkillSigningKeyParams{
		RevokedAt: sql.NullTime{Time: at.UTC(), Valid: true}, RevocationReason: reason,
		UpdatedAt: at.UTC(), UpdatedBy: actor, KeyID: keyID,
	})
	if err != nil {
		return false, fmt.Errorf("revoke skill signing key: %w", err)
	}
	return n > 0, nil
}

// RetireSkillSigningKey closes a key's validity window without calling it
// compromised, which is what an ordinary scheduled rotation does. Signatures
// inside the window keep verifying.
func (s *Store) RetireSkillSigningKey(
	ctx context.Context, keyID string, until time.Time, at time.Time, actor string,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	utc := until.UTC()
	n, err := s.qw.RetireSkillSigningKey(ctx, gen.RetireSkillSigningKeyParams{
		ValidUntil: sql.NullTime{Time: utc, Valid: true},
		UpdatedAt:  at.UTC(), UpdatedBy: actor, KeyID: keyID,
	})
	if err != nil {
		return false, fmt.Errorf("retire skill signing key: %w", err)
	}
	return n > 0, nil
}

// UpsertSkillTrustRevocation records an administrative withdrawal.
//
// The timestamp is INSERT-only: a second call may correct the reason and
// cannot move the moment. When something was revoked is a fact about an
// incident, not a field to tidy.
func (s *Store) UpsertSkillTrustRevocation(
	ctx context.Context, rev skillregistry.TrustRevocation,
) (skillregistry.TrustRevocation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	row, err := s.qw.UpsertSkillTrustRevocation(ctx, gen.UpsertSkillTrustRevocationParams{
		Subject:   string(rev.Subject),
		SubjectID: rev.SubjectID,
		Reason:    rev.Reason,
		RevokedAt: rev.RevokedAt.UTC(),
		RevokedBy: rev.RevokedBy,
	})
	if err != nil {
		return skillregistry.TrustRevocation{}, fmt.Errorf("upsert skill trust revocation: %w", err)
	}
	return trustRevocationFromRow(row), nil
}

// GetSkillTrustRevocation returns one administrative revocation.
func (s *Store) GetSkillTrustRevocation(
	ctx context.Context, subject skillregistry.RevocationSubject, subjectID string,
) (skillregistry.TrustRevocation, bool, error) {
	row, err := s.qr.GetSkillTrustRevocation(ctx, gen.GetSkillTrustRevocationParams{
		Subject: string(subject), SubjectID: subjectID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skillregistry.TrustRevocation{}, false, nil
		}
		return skillregistry.TrustRevocation{}, false,
			fmt.Errorf("get skill trust revocation: %w", err)
	}
	return trustRevocationFromRow(row), true, nil
}

// ListSkillTrustRevocations returns every administrative revocation.
func (s *Store) ListSkillTrustRevocations(
	ctx context.Context,
) ([]skillregistry.TrustRevocation, error) {
	rows, err := s.qr.ListSkillTrustRevocations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list skill trust revocations: %w", err)
	}
	out := make([]skillregistry.TrustRevocation, 0, len(rows))
	for _, row := range rows {
		out = append(out, trustRevocationFromRow(row))
	}
	return out, nil
}

func trustRootFromRow(row gen.SkillTrustRoot) skillregistry.TrustRoot {
	return skillregistry.TrustRoot{
		ID:               row.ID,
		Tier:             skillregistry.TrustTier(row.Tier),
		DisplayName:      row.DisplayName,
		Publisher:        row.Publisher,
		Status:           skillregistry.TrustStatus(row.Status),
		ValidFrom:        row.ValidFrom.UTC(),
		ValidUntil:       nullTimeToTimePtr(row.ValidUntil),
		RevokedAt:        nullTimeToTimePtr(row.RevokedAt),
		RevocationReason: row.RevocationReason,
		CreatedAt:        row.CreatedAt.UTC(),
		CreatedBy:        row.CreatedBy,
		UpdatedAt:        row.UpdatedAt.UTC(),
		UpdatedBy:        row.UpdatedBy,
	}
}

func signingKeyFromRow(row gen.SkillSigningKey) skillregistry.SigningKey {
	return skillregistry.SigningKey{
		KeyID:            row.KeyID,
		TrustRootID:      row.TrustRootID,
		Publisher:        row.Publisher,
		IsRootKey:        row.IsRootKey != 0,
		Algorithm:        row.Algorithm,
		PublicKey:        row.PublicKey,
		Fingerprint:      row.Fingerprint,
		Origin:           skillregistry.KeyOrigin(row.Origin),
		Status:           skillregistry.TrustStatus(row.Status),
		ValidFrom:        row.ValidFrom.UTC(),
		ValidUntil:       nullTimeToTimePtr(row.ValidUntil),
		RevokedAt:        nullTimeToTimePtr(row.RevokedAt),
		RevocationReason: row.RevocationReason,
		RotatedFromKeyID: row.RotatedFromKeyID,
		CreatedAt:        row.CreatedAt.UTC(),
		CreatedBy:        row.CreatedBy,
		UpdatedAt:        row.UpdatedAt.UTC(),
		UpdatedBy:        row.UpdatedBy,
	}
}

func trustRevocationFromRow(row gen.SkillTrustRevocation) skillregistry.TrustRevocation {
	return skillregistry.TrustRevocation{
		Subject:   skillregistry.RevocationSubject(row.Subject),
		SubjectID: row.SubjectID,
		Reason:    row.Reason,
		RevokedAt: row.RevokedAt.UTC(),
		RevokedBy: row.RevokedBy,
	}
}
