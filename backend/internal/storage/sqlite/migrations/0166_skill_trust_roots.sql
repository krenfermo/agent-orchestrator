-- Skills phase 12: signatures, trust roots, and what TRUSTED is allowed to mean.
--
-- Phases 10 and 11 could verify BYTES. Two digests, computed by AO over the
-- tree it fetched, matched against the release it resolved. That catches a
-- registry that lies about a digest and nothing else: a registry that honestly
-- serves malicious code under a correct digest passed every check, and TLS did
-- not help, because TLS authenticates the SERVER and the question is who wrote
-- the CODE. 'trusted' was therefore DEFINED AND UNREACHABLE, on purpose, so
-- that 'verified' could not quietly become the top of the ladder.
--
-- This migration is the durable half of making it reachable. See ADR 0008.
--
-- Five changes:
--
--   skill_trust_roots           the anchors this installation will chain to
--   skill_signing_keys          the public keys under them
--   skill_trust_revocations     administrative withdrawal of a key, a
--                               publisher or a root -- global, and AO's own
--   skill_registries            (rebuilt) trust_policy gains 'official'
--   skill_registry_revocations  (rebuilt) a revocation names its SUBJECT
--   skill_install_origins       (widened) the verified provenance chain
--   skill_audit                 (widened) the trust decisions
--
-- WHY THERE IS NO PRIVATE KEY COLUMN, ANYWHERE.
-- The daemon verifies; it does not sign. A consumer that held a signing key
-- would be a consumer that could mint its own trusted releases, at which point
-- the signature proves only that this host trusts itself. There is no column a
-- private key could go in, and skillregistry has no function that would write
-- one. Signing happens offline, by whoever holds the root, and in this
-- repository only inside test fixtures.
--
-- WHY ROOTS AND KEYS ARE TWO TABLES.
-- Rotation. A root is a long-lived anchor identity; a signing key is a thing
-- that gets replaced on a schedule and occasionally in a hurry. Putting the
-- key inside the root would make rotation mean overwriting a root's public key
-- under the same id -- which is precisely the silent key substitution the
-- whole scheme exists to detect. Two tables make an overlap window, a
-- retirement and a revocation expressible, and make the history of which key
-- signed what survive the rotation.
--
-- WHY A KEY'S VALIDITY IS CHECKED AGAINST THE SIGNING TIME AND NOT AGAINST NOW.
-- Otherwise every historical release stops verifying the day its key expires,
-- which is a system that punishes the passage of time rather than one that
-- detects compromise. REVOCATION is the exception and is absolute: a revoked
-- key may have been in somebody else's hands for an unknown period before
-- anybody noticed, so "the signature predates the revocation" establishes
-- nothing about who made it.
--
-- WHY REVOCATION IS SPLIT IN TWO TABLES WITH DIFFERENT AUTHORS.
-- skill_registry_revocations is what a REGISTRY said. AO accepts it because a
-- registry's word can only ever REFUSE an install, which is the asymmetry ADR
-- 0007 recorded: caching "this is withdrawn" is wrong in the safe direction.
-- But a registry that could revoke AO's official signing key GLOBALLY could
-- switch off every trusted install on the machine, so a registry's word about
-- a key, a publisher or a root is scoped to installs from that registry.
-- skill_trust_revocations is what an ADMINISTRATOR here decided, through
-- settings.manage, and that one is global. Two authors, two blast radii, two
-- tables.
--
-- WHY skill_registries IS REBUILT WITH PRAGMA foreign_keys=OFF.
-- The trust_policy CHECK has to accept 'official' and SQLite has no ALTER for
-- a CHECK. Unlike 0165's rebuild, this one is NOT free: 0165 itself gave
-- skill_registries two incoming references (skill_registry_status.registry_id
-- and skill_registry_revocations.registry_id, both ON DELETE CASCADE), and
-- SQLite treats DROP TABLE as deleting every row for foreign-key purposes. A
-- naive rebuild would therefore CASCADE AWAY EVERY STORED REVOCATION -- which
-- is not merely data loss, it is the silent removal of the rows that block
-- installs of withdrawn releases. So enforcement is turned off for real:
-- `PRAGMA foreign_keys=OFF` together with the goose "no transaction"
-- annotation below, because the pragma is silently ignored inside goose's
-- transaction and one without the other does nothing. The child tables are
-- rebuilt in the same block, and `PRAGMA foreign_key_check` runs before
-- enforcement comes back on. This rebuild is declared in
-- migrate_rebuild_fk_safety_test.go, which exercises it against a database
-- that actually holds the referencing rows.
--
-- WHY skill_install_origins IS ALTERED AND NOT REBUILT.
-- Only new columns are needed; its trust_state CHECK already listed 'trusted'
-- because 0164 put the whole vocabulary in the column even though the build
-- could not reach the top of it. ALTER TABLE ADD COLUMN touches no existing
-- row and needs no rebuild, so the table that carries every install's
-- provenance is never dropped.

-- +goose NO TRANSACTION

-- +goose Up
-- +goose StatementBegin
CREATE TABLE skill_trust_roots (
    id            TEXT      PRIMARY KEY,
    -- WHO decided this root should be trusted, not how much it is trusted.
    -- 'official' is compiled into the AO build and is never written here by an
    -- administrator: the id 'ao-official' and the publisher 'ao' are reserved
    -- in Go, so the one name an attacker would most like to use is the one
    -- they cannot claim. 'external' is DECLARED and refused -- "who may
    -- publish to everyone" is a decision no phase has made, and the value
    -- exists so external trust cannot arrive by quietly reusing 'enterprise'.
    tier          TEXT      NOT NULL CHECK (tier IN ('official','enterprise','external')),
    display_name  TEXT      NOT NULL,
    -- The identity this root vouches for. A release whose publisher differs
    -- from its key's root does not become trusted however good its signature:
    -- a valid signature by the wrong party is publisher spoofing, not a pass.
    publisher     TEXT      NOT NULL,
    status        TEXT      NOT NULL CHECK (status IN ('active','retired','revoked')),
    valid_from    TIMESTAMP NOT NULL,
    valid_until   TIMESTAMP,
    revoked_at    TIMESTAMP,
    revocation_reason TEXT  NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL,
    created_by    TEXT      NOT NULL DEFAULT '',
    updated_at    TIMESTAMP NOT NULL,
    updated_by    TEXT      NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_trust_roots_publisher ON skill_trust_roots (publisher);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE skill_signing_keys (
    key_id        TEXT      PRIMARY KEY,
    trust_root_id TEXT      NOT NULL REFERENCES skill_trust_roots (id) ON DELETE CASCADE,
    publisher     TEXT      NOT NULL,
    -- A ROOT key authorizes other keys and MUST NOT sign releases. Separating
    -- the two roles is what stops one leaked release-signing key from being
    -- used to mint more keys, which is the difference between an incident and
    -- a permanent foothold.
    is_root_key   INTEGER   NOT NULL DEFAULT 0 CHECK (is_root_key IN (0,1)),
    -- Versioned from the first release. An unknown algorithm is REFUSED, never
    -- best-effort verified: a verifier that falls back to the algorithm it
    -- does know is a verifier an attacker downgrades.
    algorithm     TEXT      NOT NULL CHECK (algorithm IN ('ed25519')),
    -- base64, 32 bytes decoded. PUBLIC. There is no private counterpart in
    -- this process or in this schema.
    public_key    TEXT      NOT NULL,
    -- sha256 over the RAW key bytes, lowercase hex, derived in Go from the
    -- decoded key rather than accepted from a caller -- so there is no path
    -- that stores a fingerprint and a key which were never compared.
    fingerprint   TEXT      NOT NULL,
    -- HOW this key reached the store. 'certificate' means AO verified a
    -- statement signed by a root key it holds -- the chain is cryptographic
    -- end to end. 'administrative' means somebody here said the key is fine,
    -- which is a real and necessary path for a root that keeps its key
    -- offline, and a different assurance. A screen that rendered the two
    -- identically would be overstating one of them.
    origin        TEXT      NOT NULL CHECK (origin IN ('certificate','administrative','built-in')),
    status        TEXT      NOT NULL CHECK (status IN ('active','retired','revoked')),
    valid_from    TIMESTAMP NOT NULL,
    valid_until   TIMESTAMP,
    revoked_at    TIMESTAMP,
    revocation_reason TEXT  NOT NULL DEFAULT '',
    -- What this key replaced, when it replaced one. It is what makes a
    -- rotation legible as a rotation rather than as two unrelated keys that
    -- happened to overlap.
    rotated_from_key_id TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL,
    created_by    TEXT      NOT NULL DEFAULT '',
    updated_at    TIMESTAMP NOT NULL,
    updated_by    TEXT      NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_signing_keys_root ON skill_signing_keys (trust_root_id, key_id);
-- +goose StatementEnd

-- A key id is permanent and a public key under it never changes. Two rows
-- would be the substitution attack in its simplest form; one row that gets
-- UPDATEd would be the same attack with better manners. The Go store refuses
-- an upsert whose public key differs from the stored one, and this unique
-- index is the second lock on the same door.
-- +goose StatementBegin
CREATE UNIQUE INDEX ux_skill_signing_keys_identity ON skill_signing_keys (key_id, public_key);
-- +goose StatementEnd

-- What an ADMINISTRATOR on this installation withdrew. Global, unlike a
-- registry's word.
-- +goose StatementBegin
CREATE TABLE skill_trust_revocations (
    subject       TEXT      NOT NULL CHECK (subject IN ('signing_key','publisher','trust_root')),
    -- A key id, a publisher name, or a trust root id. Release revocations do
    -- NOT live here: a release is withdrawn by the registry that published it
    -- and by skill_install_origins, and duplicating that here would be a
    -- second place to look for the same fact.
    subject_id    TEXT      NOT NULL,
    -- A revocation that does not say why is indistinguishable from a mistake,
    -- and it is about to block every install under it.
    reason        TEXT      NOT NULL,
    revoked_at    TIMESTAMP NOT NULL,
    revoked_by    TEXT      NOT NULL DEFAULT '',
    PRIMARY KEY (subject, subject_id)
);
-- +goose StatementEnd

-- The rebuilds. Enforcement is off for real here -- see the header note on why
-- a naive rebuild would cascade away every stored revocation.
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

CREATE TABLE skill_registries_p12 (
    id            TEXT      PRIMARY KEY,
    display_name  TEXT      NOT NULL,
    type          TEXT      NOT NULL CHECK (type IN ('local','https','git')),
    location      TEXT      NOT NULL,
    enabled       INTEGER   NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    -- 'official' joins the vocabulary. It is 'signed' narrowed to the OFFICIAL
    -- tier, and this build carries no official root, so a registry on it
    -- installs nothing and the refusal says why. That is the same honest shape
    -- 'signed' had in phase 11 -- with the difference that the machinery
    -- behind it now genuinely verifies, and 'signed' against an enterprise
    -- root reaches trusted today.
    trust_policy  TEXT      NOT NULL
                            CHECK (trust_policy IN ('digest','pinned_publisher','signed','official')),
    pinned_publisher TEXT   NOT NULL DEFAULT '',
    priority      INTEGER   NOT NULL DEFAULT 100,
    tenant_id     TEXT      REFERENCES tenants (id) ON DELETE CASCADE,
    credential_secret_name TEXT NOT NULL DEFAULT '',
    auth_type     TEXT      NOT NULL DEFAULT 'none'
                            CHECK (auth_type IN ('none','bearer','api_key_header')),
    api_key_header TEXT     NOT NULL DEFAULT '',
    network_policy TEXT     NOT NULL DEFAULT '{}',
    created_at    TIMESTAMP NOT NULL,
    created_by    TEXT      NOT NULL DEFAULT '',
    updated_at    TIMESTAMP NOT NULL,
    updated_by    TEXT      NOT NULL DEFAULT ''
);

INSERT INTO skill_registries_p12 (
    id, display_name, type, location, enabled, trust_policy, pinned_publisher,
    priority, tenant_id, credential_secret_name, auth_type, api_key_header,
    network_policy, created_at, created_by, updated_at, updated_by
)
SELECT id, display_name, type, location, enabled, trust_policy, pinned_publisher,
    priority, tenant_id, credential_secret_name, auth_type, api_key_header,
    network_policy, created_at, created_by, updated_at, updated_by
FROM skill_registries;

-- The revocation table gains a SUBJECT. Its old primary key
-- (registry_id, skill_id, version) cannot express "this registry withdrew a
-- signing key", so the identity becomes (registry_id, subject, subject_key)
-- where subject_key is 'skillId@version' for a release and the subject's own
-- id otherwise. skill_id and version are KEPT rather than folded into
-- subject_key, because every existing query and every screen reads them and a
-- string that has to be split to be useful is a string somebody splits wrong.
CREATE TABLE skill_registry_revocations_p12 (
    registry_id   TEXT      NOT NULL REFERENCES skill_registries (id) ON DELETE CASCADE,
    subject       TEXT      NOT NULL DEFAULT 'release'
                            CHECK (subject IN ('release','signing_key','publisher','trust_root')),
    -- The canonical identity within (registry_id, subject).
    subject_key   TEXT      NOT NULL,
    -- Present for a release revocation, empty otherwise.
    skill_id      TEXT      NOT NULL DEFAULT '',
    version       TEXT      NOT NULL DEFAULT '',
    reason        TEXT      NOT NULL,
    revoked_at    TIMESTAMP,
    observed_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (registry_id, subject, subject_key)
);

INSERT INTO skill_registry_revocations_p12 (
    registry_id, subject, subject_key, skill_id, version, reason, revoked_at, observed_at
)
SELECT registry_id, 'release', skill_id || '@' || version, skill_id, version,
    reason, revoked_at, observed_at
FROM skill_registry_revocations;

DROP TABLE skill_registry_revocations;
ALTER TABLE skill_registry_revocations_p12 RENAME TO skill_registry_revocations;

DROP TABLE skill_registries;
ALTER TABLE skill_registries_p12 RENAME TO skill_registries;

CREATE INDEX idx_skill_registries_priority ON skill_registries (priority, id);
CREATE INDEX idx_skill_registry_revocations_skill ON skill_registry_revocations (skill_id, version);
CREATE INDEX idx_skill_registry_revocations_subject ON skill_registry_revocations (subject, subject_key);

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd

-- The verified provenance chain, on the row that already records what AO
-- checked. ADD COLUMN, never a rebuild: this table carries every install's
-- provenance and must not be dropped.
--
-- NOT ADDED, deliberately: the signed payload itself, and the public key. The
-- payload is reconstructible from the fields already here, and storing a
-- second copy would create a second thing that can disagree with the first.
-- The key is in skill_signing_keys, addressed by key_id; copying it here would
-- mean a row whose key could differ from the store's, which is a substitution
-- attack with extra steps.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN signature_scheme TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN signature_algorithm TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN signing_key_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN signing_key_fingerprint TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN signing_key_origin TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN trust_root_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN trust_root_tier TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN signature_signed_at TIMESTAMP;
-- +goose StatementEnd
-- The moment AO's own clock says it checked, which is a different fact from
-- when the publisher says they signed. Only the second one is AO's.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN signature_verified_at TIMESTAMP;
-- +goose StatementEnd
-- 'verified', 'refused', or '' for an install that predates signatures or came
-- from a registry that does not serve them. Three states, because "AO checked
-- and refused" and "AO never checked" are different and a boolean would merge
-- them.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN signature_result TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- What the revocation picture looked like at the moment of install, so a later
-- reader can tell "nothing was revoked then" from "AO could not ask".
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN revocation_state_observed TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- When the metadata this install acted on was fetched. Freshness travels with
-- provenance and into neither trust nor compatibility.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN metadata_fetched_at TIMESTAMP;
-- +goose StatementEnd

-- Widen the audit vocabulary for the trust decisions, in the same trail that
-- already answers "who installed this package".
--
-- NOT ADDED, still: skill_searched. 0164 and 0165 both recorded the reason and
-- it has not changed.
--
-- ALSO NOT ADDED: any row that could carry key material. signing_key_seen
-- records a key id and a fingerprint, both public. signature_verified records
-- a key id, a root id and a verdict. Nothing here has a place for a private
-- key, a credential, or the full signed payload -- the payload would carry the
-- release description a second time, and a second copy is a second thing that
-- can disagree with the first.
--
-- Nothing references skill_audit by foreign key, which is what makes this
-- rebuild safe; 0163, 0164 and 0165 did the same.
-- +goose StatementBegin
CREATE TABLE skill_audit_p12 (
    id           TEXT      PRIMARY KEY,
    occurred_at  TIMESTAMP NOT NULL,
    actor        TEXT      NOT NULL DEFAULT '',
    action       TEXT      NOT NULL CHECK (action IN (
                     'install','uninstall','enable','disable','grant_changed','install_rejected',
                     'image_approved','image_revoked','run_executed','run_refused',
                     'registry_added','registry_updated','registry_removed',
                     'install_refused','update_available','update_installed','release_revoked_seen',
                     'registry_enabled','registry_disabled','registry_connection_tested',
                     'registry_auth_failed','skill_fetch_started','skill_fetch_refused',
                     'cached_artifact_used','registry_revocations_synced',
                     'trust_root_added','trust_root_revoked','trust_root_updated',
                     'signing_key_seen','signing_key_added','signing_key_rotated',
                     'signing_key_revoked','key_revoked_seen',
                     'signature_verified','signature_refused','publisher_mismatch',
                     'publisher_revoked','trusted_install','trusted_install_refused'
                 )),
    skill_id     TEXT      NOT NULL,
    version      TEXT      NOT NULL DEFAULT '',
    project_id   TEXT,
    digest       TEXT      NOT NULL DEFAULT '',
    capabilities TEXT      NOT NULL DEFAULT '[]',
    detail       TEXT      NOT NULL DEFAULT ''
);

INSERT INTO skill_audit_p12 (
    id, occurred_at, actor, action, skill_id, version, project_id,
    digest, capabilities, detail
)
SELECT id, occurred_at, actor, action, skill_id, version, project_id,
    digest, capabilities, detail
FROM skill_audit;

DROP TABLE skill_audit;
ALTER TABLE skill_audit_p12 RENAME TO skill_audit;

CREATE INDEX idx_skill_audit_occurred_at ON skill_audit (occurred_at);
CREATE INDEX idx_skill_audit_skill ON skill_audit (skill_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS skill_trust_revocations;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_signing_keys;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_trust_roots;
-- +goose StatementEnd
