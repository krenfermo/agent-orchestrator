-- Skills phase 10: registry configuration and install provenance. No trailing
-- ORDER BY/LIMIT -- see users.sql's note on the sqlc v1.31.1 SQLite codegen
-- bug; sort in Go, where skillregistry.SortRegistries is the one order every
-- surface shares.

-- name: UpsertSkillRegistry :one
INSERT INTO skill_registries (
    id, display_name, type, location, enabled, trust_policy, pinned_publisher,
    priority, tenant_id, credential_secret_name, auth_type, api_key_header,
    network_policy, created_at, created_by, updated_at, updated_by
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    display_name = excluded.display_name,
    type = excluded.type,
    location = excluded.location,
    enabled = excluded.enabled,
    trust_policy = excluded.trust_policy,
    pinned_publisher = excluded.pinned_publisher,
    priority = excluded.priority,
    tenant_id = excluded.tenant_id,
    credential_secret_name = excluded.credential_secret_name,
    auth_type = excluded.auth_type,
    api_key_header = excluded.api_key_header,
    network_policy = excluded.network_policy,
    updated_at = excluded.updated_at,
    updated_by = excluded.updated_by
RETURNING id, display_name, type, location, enabled, trust_policy,
    pinned_publisher, priority, tenant_id, credential_secret_name,
    auth_type, api_key_header, network_policy,
    created_at, created_by, updated_at, updated_by;

-- name: GetSkillRegistry :one
SELECT id, display_name, type, location, enabled, trust_policy,
    pinned_publisher, priority, tenant_id, credential_secret_name,
    auth_type, api_key_header, network_policy,
    created_at, created_by, updated_at, updated_by
FROM skill_registries WHERE id = ?;

-- name: ListSkillRegistries :many
SELECT id, display_name, type, location, enabled, trust_policy,
    pinned_publisher, priority, tenant_id, credential_secret_name,
    auth_type, api_key_header, network_policy,
    created_at, created_by, updated_at, updated_by
FROM skill_registries;

-- name: DeleteSkillRegistry :execrows
DELETE FROM skill_registries WHERE id = ?;

-- name: UpsertSkillInstallOrigin :one
INSERT INTO skill_install_origins (
    skill_id, version, registry_id, registry_name, registry_type,
    registry_location, publisher, source_url, manifest_digest, artifact_digest,
    trust_state, trust_policy, signature_format, signature, key_id,
    attestation_url, compatibility_verdict, published_at, installed_at,
    installed_by, revoked_at, revocation_reason, revocation_seen_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (skill_id, version) DO UPDATE SET
    registry_id = excluded.registry_id,
    registry_name = excluded.registry_name,
    registry_type = excluded.registry_type,
    registry_location = excluded.registry_location,
    publisher = excluded.publisher,
    source_url = excluded.source_url,
    manifest_digest = excluded.manifest_digest,
    artifact_digest = excluded.artifact_digest,
    trust_state = excluded.trust_state,
    trust_policy = excluded.trust_policy,
    signature_format = excluded.signature_format,
    signature = excluded.signature,
    key_id = excluded.key_id,
    attestation_url = excluded.attestation_url,
    compatibility_verdict = excluded.compatibility_verdict,
    published_at = excluded.published_at,
    installed_at = excluded.installed_at,
    installed_by = excluded.installed_by
RETURNING skill_id, version, registry_id, registry_name, registry_type,
    registry_location, publisher, source_url, manifest_digest, artifact_digest,
    trust_state, trust_policy, signature_format, signature, key_id,
    attestation_url, compatibility_verdict, published_at, installed_at,
    installed_by, revoked_at, revocation_reason, revocation_seen_at;

-- name: GetSkillInstallOrigin :one
SELECT skill_id, version, registry_id, registry_name, registry_type,
    registry_location, publisher, source_url, manifest_digest, artifact_digest,
    trust_state, trust_policy, signature_format, signature, key_id,
    attestation_url, compatibility_verdict, published_at, installed_at,
    installed_by, revoked_at, revocation_reason, revocation_seen_at
FROM skill_install_origins WHERE skill_id = ? AND version = ?;

-- name: ListSkillInstallOrigins :many
SELECT skill_id, version, registry_id, registry_name, registry_type,
    registry_location, publisher, source_url, manifest_digest, artifact_digest,
    trust_state, trust_policy, signature_format, signature, key_id,
    attestation_url, compatibility_verdict, published_at, installed_at,
    installed_by, revoked_at, revocation_reason, revocation_seen_at
FROM skill_install_origins;

-- name: ListSkillInstallOriginsForSkill :many
SELECT skill_id, version, registry_id, registry_name, registry_type,
    registry_location, publisher, source_url, manifest_digest, artifact_digest,
    trust_state, trust_policy, signature_format, signature, key_id,
    attestation_url, compatibility_verdict, published_at, installed_at,
    installed_by, revoked_at, revocation_reason, revocation_seen_at
FROM skill_install_origins WHERE skill_id = ?;

-- MarkSkillInstallOriginRevoked records a revocation AO OBSERVED after the
-- install. It removes nothing: a revoked release already on this host is a
-- decision waiting for a human, and uninstalling it here would be AO acting on
-- a remote instruction.
-- name: MarkSkillInstallOriginRevoked :execrows
UPDATE skill_install_origins
SET trust_state = 'revoked',
    revoked_at = COALESCE(revoked_at, ?),
    revocation_reason = ?,
    revocation_seen_at = ?
WHERE skill_id = ? AND version = ? AND revoked_at IS NULL;

-- Skills phase 11: what the last connection test and the last sync said.
--
-- A side table, not columns on skill_registries: the configuration is what an
-- administrator wrote and this is what the network answered, and merging them
-- would make "when was this registry last changed" and "when did it last
-- answer" the same timestamp.

-- name: UpsertSkillRegistryStatus :one
INSERT INTO skill_registry_status (
    registry_id, last_probe_state, last_probe_detail, last_probe_at,
    last_probe_latency_ms, last_sync_at, last_revocation_sync_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (registry_id) DO UPDATE SET
    last_probe_state = excluded.last_probe_state,
    last_probe_detail = excluded.last_probe_detail,
    last_probe_at = excluded.last_probe_at,
    last_probe_latency_ms = excluded.last_probe_latency_ms,
    last_sync_at = COALESCE(excluded.last_sync_at, skill_registry_status.last_sync_at),
    last_revocation_sync_at = COALESCE(
        excluded.last_revocation_sync_at, skill_registry_status.last_revocation_sync_at),
    updated_at = excluded.updated_at
RETURNING registry_id, last_probe_state, last_probe_detail, last_probe_at,
    last_probe_latency_ms, last_sync_at, last_revocation_sync_at, updated_at;

-- name: GetSkillRegistryStatus :one
SELECT registry_id, last_probe_state, last_probe_detail, last_probe_at,
    last_probe_latency_ms, last_sync_at, last_revocation_sync_at, updated_at
FROM skill_registry_status WHERE registry_id = ?;

-- name: ListSkillRegistryStatuses :many
SELECT registry_id, last_probe_state, last_probe_detail, last_probe_at,
    last_probe_latency_ms, last_sync_at, last_revocation_sync_at, updated_at
FROM skill_registry_status;

-- Skills phase 11: what a registry says is withdrawn.
--
-- Persisted while the OFFER deliberately is not (see 0164). Caching "this
-- release is fine" can only be wrong in the dangerous direction; caching "this
-- release is withdrawn" can only be wrong in the safe one.

-- name: UpsertSkillRegistryRevocation :one
INSERT INTO skill_registry_revocations (
    registry_id, skill_id, version, reason, revoked_at, observed_at
)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (registry_id, skill_id, version) DO UPDATE SET
    reason = excluded.reason,
    revoked_at = COALESCE(skill_registry_revocations.revoked_at, excluded.revoked_at)
RETURNING registry_id, skill_id, version, reason, revoked_at, observed_at;

-- name: GetSkillRegistryRevocation :one
SELECT registry_id, skill_id, version, reason, revoked_at, observed_at
FROM skill_registry_revocations
WHERE registry_id = ? AND skill_id = ? AND version = ?;

-- name: ListSkillRegistryRevocations :many
SELECT registry_id, skill_id, version, reason, revoked_at, observed_at
FROM skill_registry_revocations WHERE registry_id = ?;

-- name: ListAllSkillRegistryRevocations :many
SELECT registry_id, skill_id, version, reason, revoked_at, observed_at
FROM skill_registry_revocations;
