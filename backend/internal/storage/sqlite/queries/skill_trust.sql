-- Skills phase 12: trust roots, signing keys, and administrative revocations.
--
-- No trailing ORDER BY/LIMIT -- see users.sql's note on the sqlc v1.31.1
-- SQLite codegen bug; sort in Go.
--
-- There is no query here that writes a private key, because there is no column
-- one could go in. The daemon verifies and does not sign.

-- name: UpsertSkillTrustRoot :one
INSERT INTO skill_trust_roots (
    id, tier, display_name, publisher, status, valid_from, valid_until,
    revoked_at, revocation_reason, created_at, created_by, updated_at, updated_by
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    display_name = excluded.display_name,
    status = excluded.status,
    valid_until = excluded.valid_until,
    revoked_at = COALESCE(skill_trust_roots.revoked_at, excluded.revoked_at),
    revocation_reason = excluded.revocation_reason,
    updated_at = excluded.updated_at,
    updated_by = excluded.updated_by
RETURNING id, tier, display_name, publisher, status, valid_from, valid_until,
    revoked_at, revocation_reason, created_at, created_by, updated_at, updated_by;

-- name: GetSkillTrustRoot :one
SELECT id, tier, display_name, publisher, status, valid_from, valid_until,
    revoked_at, revocation_reason, created_at, created_by, updated_at, updated_by
FROM skill_trust_roots WHERE id = ?;

-- name: ListSkillTrustRoots :many
SELECT id, tier, display_name, publisher, status, valid_from, valid_until,
    revoked_at, revocation_reason, created_at, created_by, updated_at, updated_by
FROM skill_trust_roots;

-- RevokeSkillTrustRoot is the only way a root's status becomes 'revoked'
-- through this store, and it is one-way: the WHERE clause refuses a second
-- revocation, so the first reason and the first timestamp are the ones that
-- stand. Un-revoking is deliberately not expressible -- an anchor somebody
-- decided was compromised does not come back because a form was re-submitted.
-- name: RevokeSkillTrustRoot :execrows
UPDATE skill_trust_roots
SET status = 'revoked', revoked_at = ?, revocation_reason = ?, updated_at = ?, updated_by = ?
WHERE id = ? AND status <> 'revoked';

-- name: UpsertSkillSigningKey :one
INSERT INTO skill_signing_keys (
    key_id, trust_root_id, publisher, is_root_key, algorithm, public_key,
    fingerprint, origin, status, valid_from, valid_until, revoked_at,
    revocation_reason, rotated_from_key_id, created_at, created_by,
    updated_at, updated_by
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
-- The public key is deliberately NOT in the SET list. A key id is permanent
-- and the key under it never changes: silently replacing one would be the
-- substitution attack with better manners. The Go store refuses the upsert
-- outright when the material differs; this omission is the second lock, so
-- that even a caller that skipped the check cannot overwrite key material.
ON CONFLICT (key_id) DO UPDATE SET
    status = excluded.status,
    valid_until = excluded.valid_until,
    revoked_at = COALESCE(skill_signing_keys.revoked_at, excluded.revoked_at),
    revocation_reason = excluded.revocation_reason,
    rotated_from_key_id = excluded.rotated_from_key_id,
    updated_at = excluded.updated_at,
    updated_by = excluded.updated_by
RETURNING key_id, trust_root_id, publisher, is_root_key, algorithm, public_key,
    fingerprint, origin, status, valid_from, valid_until, revoked_at,
    revocation_reason, rotated_from_key_id, created_at, created_by,
    updated_at, updated_by;

-- name: GetSkillSigningKey :one
SELECT key_id, trust_root_id, publisher, is_root_key, algorithm, public_key,
    fingerprint, origin, status, valid_from, valid_until, revoked_at,
    revocation_reason, rotated_from_key_id, created_at, created_by,
    updated_at, updated_by
FROM skill_signing_keys WHERE key_id = ?;

-- name: ListSkillSigningKeys :many
SELECT key_id, trust_root_id, publisher, is_root_key, algorithm, public_key,
    fingerprint, origin, status, valid_from, valid_until, revoked_at,
    revocation_reason, rotated_from_key_id, created_at, created_by,
    updated_at, updated_by
FROM skill_signing_keys;

-- name: ListSkillSigningKeysForRoot :many
SELECT key_id, trust_root_id, publisher, is_root_key, algorithm, public_key,
    fingerprint, origin, status, valid_from, valid_until, revoked_at,
    revocation_reason, rotated_from_key_id, created_at, created_by,
    updated_at, updated_by
FROM skill_signing_keys WHERE trust_root_id = ?;

-- One-way, for the same reason RevokeSkillTrustRoot is.
-- name: RevokeSkillSigningKey :execrows
UPDATE skill_signing_keys
SET status = 'revoked', revoked_at = ?, revocation_reason = ?, updated_at = ?, updated_by = ?
WHERE key_id = ? AND status <> 'revoked';

-- RetireSkillSigningKey closes a key's window without calling it compromised.
-- The two are different facts with different consequences for history:
-- a retired key's earlier signatures still verify, and a revoked key's do not.
-- name: RetireSkillSigningKey :execrows
UPDATE skill_signing_keys
SET status = 'retired', valid_until = ?, updated_at = ?, updated_by = ?
WHERE key_id = ? AND status = 'active';

-- name: UpsertSkillTrustRevocation :one
INSERT INTO skill_trust_revocations (subject, subject_id, reason, revoked_at, revoked_by)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (subject, subject_id) DO UPDATE SET
    reason = excluded.reason
RETURNING subject, subject_id, reason, revoked_at, revoked_by;

-- name: GetSkillTrustRevocation :one
SELECT subject, subject_id, reason, revoked_at, revoked_by
FROM skill_trust_revocations WHERE subject = ? AND subject_id = ?;

-- name: ListSkillTrustRevocations :many
SELECT subject, subject_id, reason, revoked_at, revoked_by
FROM skill_trust_revocations;

-- Skills phase 13: lifting an administrative revocation.
--
-- It exists because the external subjects are a judgement about a place rather
-- than a cryptographic fact: a repository transferred to somebody who was then
-- vetted, an owner banned during an incident that turned out to be a false
-- alarm. A key or a root revocation is not lifted this way and never should
-- be -- a key somebody else may have held is compromised forever -- but the Go
-- layer is what enforces that, so the refusal can carry a sentence.

-- name: DeleteSkillTrustRevocation :execrows
DELETE FROM skill_trust_revocations WHERE subject = ? AND subject_id = ?;
