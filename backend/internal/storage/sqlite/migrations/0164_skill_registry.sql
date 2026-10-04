-- Skills phase 10: the registry / marketplace foundation.
--
-- Roadmap open question 6 asked what a trust root for PACKAGES looks like once
-- they stop arriving as a local directory somebody already vetted. ADR 0006
-- answers it: an administrator configures a registry, installs one EXACT
-- release from it, and AO records what it verified -- not what it was told.
--
-- Two tables, because two different questions are being answered, and one
-- widened CHECK.
--
--   skill_registries        which sources this installation may install from
--   skill_install_origins   where each installed version actually came from,
--                           and what AO checked about it
--   skill_audit             (widened) the registry decisions, in the trail that
--                           already answers "who installed this package"
--
-- WHY skill_install_origins IS A SIDE TABLE AND NOT COLUMNS ON skill_installs.
-- skill_installs has an incoming foreign key from skill_activations, so
-- widening it means a create-copy-drop-rename that SQLite would treat as
-- deleting every activation row (the 0160 incident, see
-- migrate_rebuild_fk_safety_test.go). A side table adds an OUTGOING reference
-- instead, which costs nothing and touches no existing row. It is also the
-- honest shape: a local-directory install has no registry origin at all, and a
-- nullable half of a table is worse than a row that is simply absent.
--
-- WHY registry_id IS NOT A FOREIGN KEY. "Una release ya instalada debe
-- conservar provenance aunque desaparezca del Registry." An installed package's
-- provenance is AO's record of what AO verified, and it has to outlive the
-- registry row -- removing a compromised registry must not erase the evidence
-- of what it once served. The registry's identity, display name, type and
-- location are therefore COPIED here at install time rather than joined, for
-- exactly the same reason skill_audit is not foreign-keyed to skill_installs.
--
-- WHY THERE IS NO CACHED CATALOG OF AVAILABLE RELEASES. A search re-reads the
-- registry. A cached listing would let an install act on a release that was
-- revoked since somebody last looked, which is the one thing revocation has to
-- prevent.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE skill_registries (
    id            TEXT      PRIMARY KEY,
    display_name  TEXT      NOT NULL,
    -- https and git are DECLARED and refused by the Go validator: the column
    -- accepts them so a future implementation needs no migration, and
    -- skillregistry.RegistryType.Implemented is what keeps an unreadable
    -- registry from being configured today.
    type          TEXT      NOT NULL CHECK (type IN ('local','https','git')),
    location      TEXT      NOT NULL,
    -- Disabled keeps the row, its installs and their provenance, and stops it
    -- answering searches or serving installs. Removing is a different act.
    enabled       INTEGER   NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    -- What this registry must satisfy BEYOND integrity. Integrity itself is
    -- not an option here: every install verifies both digests over the bytes
    -- AO fetched, and a "skip the hash" setting would be a setting to install
    -- something other than what was resolved.
    trust_policy  TEXT      NOT NULL CHECK (trust_policy IN ('digest','pinned_publisher','signed')),
    pinned_publisher TEXT   NOT NULL DEFAULT '',
    -- Display ordering when two registries offer the same skill id. It never
    -- decides which registry an install comes from: an install names one.
    priority      INTEGER   NOT NULL DEFAULT 100,
    -- NULL means installation-wide. A non-null tenant limits the registry to
    -- one organization, checked against the CALLER's memberships rather than
    -- against anything in a request.
    tenant_id     TEXT      REFERENCES tenants (id) ON DELETE CASCADE,
    -- The NAME of a sealed secret, never a value. internal/secretbox and
    -- skill_secrets already seal credentials under an administrator-managed
    -- key; a second, plaintext home for one would be a second way to leak it.
    -- The Go validator enforces UPPER_SNAKE_CASE and refuses it outright for a
    -- local registry, which reads a directory and needs no credential.
    credential_secret_name TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL,
    created_by    TEXT      NOT NULL DEFAULT '',
    updated_at    TIMESTAMP NOT NULL,
    updated_by    TEXT      NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_registries_priority ON skill_registries (priority, id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE skill_install_origins (
    skill_id      TEXT      NOT NULL,
    version       TEXT      NOT NULL,
    -- Copied, not joined. See the header note.
    registry_id   TEXT      NOT NULL,
    registry_name TEXT      NOT NULL DEFAULT '',
    registry_type TEXT      NOT NULL,
    registry_location TEXT  NOT NULL DEFAULT '',
    -- The publisher as the RELEASE declared it, after AO checked it against
    -- the manifest inside the fetched bytes. The two disagreeing is a refusal,
    -- so a stored row means they agreed.
    publisher     TEXT      NOT NULL,
    source_url    TEXT      NOT NULL DEFAULT '',
    -- The two digests that together cover every byte: skillcatalog's package
    -- digest deliberately excludes the manifest that carries it.
    manifest_digest TEXT    NOT NULL,
    artifact_digest TEXT    NOT NULL,
    -- What AO can honestly say. 'trusted' is in the CHECK because it is in the
    -- vocabulary; this build never writes it, because AO verifies no
    -- signature and integrity checked by hash is not provenance (ADR 0006).
    trust_state   TEXT      NOT NULL CHECK (trust_state IN ('revoked','unverified','verified','trusted')),
    trust_policy  TEXT      NOT NULL,
    -- Provenance the release CLAIMED, recorded verbatim and interpreted by
    -- nothing. Kept so an installation that later gains verification can
    -- re-check what was installed under the old rules.
    signature_format TEXT   NOT NULL DEFAULT '',
    signature        TEXT   NOT NULL DEFAULT '',
    key_id           TEXT   NOT NULL DEFAULT '',
    attestation_url  TEXT   NOT NULL DEFAULT '',
    -- Whether the compatibility check RAN, not only what it said. A source
    -- build reports an unparseable version, and 'unknown' must not read as a
    -- pass.
    compatibility_verdict TEXT NOT NULL CHECK (
        compatibility_verdict IN ('compatible','ao-too-old','ao-too-new','unknown')
    ),
    published_at  TIMESTAMP,
    installed_at  TIMESTAMP NOT NULL,
    installed_by  TEXT      NOT NULL DEFAULT '',
    -- Revocation OBSERVED after the fact. AO never uninstalls on its own: a
    -- revoked release that is already here is a decision waiting for a human,
    -- and deleting somebody's installed package because a registry changed its
    -- mind would be AO acting on a remote instruction.
    revoked_at        TIMESTAMP,
    revocation_reason TEXT  NOT NULL DEFAULT '',
    revocation_seen_at TIMESTAMP,
    PRIMARY KEY (skill_id, version),
    -- Outgoing only. Uninstalling drops the origin row with the install it
    -- describes; the audit trail is what keeps the history, and it is
    -- deliberately not foreign-keyed to anything.
    FOREIGN KEY (skill_id, version) REFERENCES skill_installs (skill_id, version) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_install_origins_registry ON skill_install_origins (registry_id, skill_id);
-- +goose StatementEnd

-- Widen the audit vocabulary for the registry decisions. Same trail as the
-- install/enable/image actions, because it is the same reviewer asking the same
-- kind of question, and two audit tables would be two places to forget to look.
--
-- NOT ADDED, deliberately: skill_searched and skill_release_viewed. A search
-- query is text a person typed, it can name an internal package or a
-- vulnerability they are hunting, and a row per keystroke-worth-of-search buys
-- an auditor nothing they cannot get from the install trail. The phase brief
-- asked for them "only if they genuinely add value"; they do not, and storing
-- queries would be storing something worth not storing.
--
-- SQLite has no ALTER for a CHECK, so this is the documented rebuild: create
-- beside, copy, drop, rename, recreate indexes. Every existing row survives --
-- the new CHECK is a superset. Nothing references skill_audit by foreign key,
-- which is what makes the rebuild safe; migration 0163 did the same.
-- +goose StatementBegin
CREATE TABLE skill_audit_new (
    id           TEXT      PRIMARY KEY,
    occurred_at  TIMESTAMP NOT NULL,
    actor        TEXT      NOT NULL DEFAULT '',
    action       TEXT      NOT NULL CHECK (action IN (
                     'install','uninstall','enable','disable','grant_changed','install_rejected',
                     'image_approved','image_revoked','run_executed','run_refused',
                     'registry_added','registry_updated','registry_removed',
                     'install_refused','update_available','update_installed','release_revoked_seen'
                 )),
    skill_id     TEXT      NOT NULL,
    version      TEXT      NOT NULL DEFAULT '',
    project_id   TEXT,
    digest       TEXT      NOT NULL DEFAULT '',
    capabilities TEXT      NOT NULL DEFAULT '[]',
    detail       TEXT      NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO skill_audit_new (
    id, occurred_at, actor, action, skill_id, version, project_id,
    digest, capabilities, detail
)
SELECT id, occurred_at, actor, action, skill_id, version, project_id,
    digest, capabilities, detail
FROM skill_audit;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE skill_audit;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE skill_audit_new RENAME TO skill_audit;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_audit_occurred_at ON skill_audit (occurred_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_audit_skill ON skill_audit (skill_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS skill_install_origins;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_registries;
-- +goose StatementEnd
