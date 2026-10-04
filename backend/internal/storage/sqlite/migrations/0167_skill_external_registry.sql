-- Skills phase 13: external registries, and what a release from somebody
-- else's forge is allowed to be.
--
-- Phases 10 to 12 all assumed the same thing: a registry is something an
-- administrator stood up. A directory on this host, a company mirror behind
-- HTTPS, an AO Official endpoint that does not exist yet. Every one of those
-- serves ONE IMMUTABLE ARTIFACT per version, so "version 1.2.3" was an
-- identity and two digests were enough to pin it.
--
-- A git forge breaks that assumption in one specific way, and everything below
-- is a consequence of it: THE NAMES ARE MUTABLE. A branch is a different tree
-- every afternoon. A tag is whatever the publisher last pointed it at, and
-- re-pointing one takes a single command and leaves no trace on the name. So a
-- release from a forge is identified by a COMMIT SHA, the tag is recorded as a
-- label, and AO remembers where each tag pointed so that it moving is a fact
-- somebody is told about rather than a substitution nobody sees.
--
-- Five changes:
--
--   skill_registries               (rebuilt) type 'github', four external
--                                  trust policies, owner/repository scope,
--                                  and the org allowlist
--   skill_install_origins          (widened) the git provenance an installed
--                                  release keeps forever
--   skill_external_tags            NEW: where a tag pointed, and whether it
--                                  has moved since
--   skill_trust_revocations        (rebuilt) an administrator may withdraw an
--                                  owner, a repository or a commit
--   skill_audit                    (widened) the external decisions
--
-- WHY 'github' IS A TYPE AND NOT A FLAVOUR OF 'git'.
-- 0164 declared 'git' and refused it, with the reason recorded there: a git
-- remote is a whole history fetched by a program that runs hooks, and "clone
-- and trust the working tree" is a trust story nobody had written down. That
-- is still true and 'git' is still refused. What this phase adds is not a
-- clone: it is a metadata API and one archive of one commit, fetched by AO's
-- own HTTP client, through the same origin guard, the same size ceilings and
-- the same quarantine every remote registry already passes through. There is
-- no shell, no working tree and no hook. A separate type is what keeps those
-- two things from being confused by anybody reading the column.
--
-- WHY THE EXTERNAL TRUST POLICIES ARE FOUR NEW VALUES AND NOT THE OLD FOUR.
-- 'digest' on a company mirror means "our registry, and AO hashed the bytes".
-- The same word on a stranger's repository would mean something materially
-- weaker while reading identically in a settings screen. The Go validator
-- refuses the two vocabularies mixing in either direction, so an external
-- registry cannot carry 'digest' and a private one cannot carry
-- 'external_org_allowlist'. What none of the four does is grant TRUSTED for
-- being hosted somewhere well known: external_signed reaches trusted through
-- exactly the same signature chain phase 12 built, and the other three reach
-- verified and stop.
--
-- WHY allowed_owners IS JSON AND HAS NO WILDCARDS.
-- JSON because it is a list that will grow neighbours (an allowlist of
-- publishers, a required topic) and a column each time is churn. No wildcards
-- because a pattern in an allowlist is an allowlist somebody widens by
-- accident, and '*' is the shortest way to switch this control off while
-- leaving it looking switched on. The Go validator refuses pattern characters
-- outright.
--
-- WHY skill_external_tags IS A LEDGER AND NOT A COLUMN.
-- A tag belongs to a (registry, repository) pair and not to an install: AO has
-- to be able to notice that v1.2.3 moved whether or not anybody ever installed
-- it, and it has to keep noticing after the install is removed. The row also
-- KEEPS the commit the tag moved away from rather than overwriting it, because
-- "this tag moved in March" is precisely the fact an incident review needs and
-- precisely the one an overwrite destroys.
--
-- WHY AN INSTALLED RELEASE'S COMMIT IS NEVER REWRITTEN.
-- The bytes on this host came from commit A and AO verified them against A's
-- digests. That record is true forever. Updating it because a stranger moved a
-- pointer would be AO editing its own history on somebody else's instruction,
-- and it would erase the single most useful piece of evidence there is: what
-- was actually installed. So the provenance columns below are written once, at
-- install, and the moved-tag ledger is a separate table that says the world
-- changed rather than that the record was wrong.
--
-- WHY EXTERNAL REVOCATION IS ADMINISTRATIVE AND LOCAL.
-- A forge publishes no revocation feed. There is no endpoint AO could poll to
-- learn that a repository was compromised, and inventing one would mean
-- inventing semantics GitHub does not have. So the three new subjects live in
-- skill_trust_revocations -- the table that already holds what an
-- ADMINISTRATOR HERE decided, globally, through settings.manage -- and NOT in
-- skill_registry_revocations, which holds what a registry claimed. A registry
-- asserting "this whole account is banned" would be a registry claiming an
-- authority the transport never gave it, and the Go validator refuses it.
--
-- WHY skill_registries IS REBUILT WITH PRAGMA foreign_keys=OFF.
-- Same reason as 0166, and the same recipe: the type and trust_policy CHECKs
-- both have to widen, SQLite has no ALTER for a CHECK, and
-- skill_registry_status and skill_registry_revocations both reference this
-- table ON DELETE CASCADE. SQLite treats DROP TABLE as deleting every row for
-- foreign-key purposes, so a naive rebuild would cascade away every stored
-- revocation -- which is not data loss, it is the silent removal of the rows
-- that block installs of withdrawn releases. The pragma is set together with
-- the goose no-transaction annotation, because inside goose's transaction the
-- pragma is silently ignored and one without the other does nothing. This
-- rebuild is declared in migrate_rebuild_fk_safety_test.go.

-- +goose NO TRANSACTION

-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

CREATE TABLE skill_registries_p13 (
    id            TEXT      PRIMARY KEY,
    display_name  TEXT      NOT NULL,
    -- 'github' joins the vocabulary and is READABLE by this build. 'git'
    -- stays declared and refused: see the header note on why they are
    -- different things.
    type          TEXT      NOT NULL CHECK (type IN ('local','https','git','github')),
    -- For github this is the API BASE URL, reduced by the Go validator to
    -- exactly one origin. It is deliberately not a repository page: a client
    -- pointed at an HTML page is a client that follows whatever that page
    -- redirects to.
    location      TEXT      NOT NULL,
    enabled       INTEGER   NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    trust_policy  TEXT      NOT NULL CHECK (trust_policy IN (
                                'digest','pinned_publisher','signed','official',
                                'external_integrity','external_signed',
                                'external_org_allowlist','external_deny'
                            )),
    -- Required by pinned_publisher, optional on an external registry (where it
    -- is the "this repository must keep publishing as acme" control), and
    -- refused everywhere else.
    pinned_publisher TEXT   NOT NULL DEFAULT '',
    priority      INTEGER   NOT NULL DEFAULT 100,
    tenant_id     TEXT      REFERENCES tenants (id) ON DELETE CASCADE,
    credential_secret_name TEXT NOT NULL DEFAULT '',
    auth_type     TEXT      NOT NULL DEFAULT 'none'
                            CHECK (auth_type IN ('none','bearer','api_key_header')),
    api_key_header TEXT     NOT NULL DEFAULT '',
    network_policy TEXT     NOT NULL DEFAULT '{}',
    -- The account or organization an external registry reads. Required for
    -- one, refused for every other type: a column that is stored, displayed
    -- and enforced by nothing is a control an administrator believes they have.
    owner         TEXT      NOT NULL DEFAULT '',
    -- Empty means every repository under owner, discovered under a hard
    -- request budget. Bounded rather than open, because an unbounded scan of
    -- somebody else's account spends their rate limit on their behalf.
    repository    TEXT      NOT NULL DEFAULT '',
    -- The external_org_allowlist control, as a JSON array of plain owner
    -- names. No patterns; see the header note.
    allowed_owners TEXT     NOT NULL DEFAULT '[]',
    created_at    TIMESTAMP NOT NULL,
    created_by    TEXT      NOT NULL DEFAULT '',
    updated_at    TIMESTAMP NOT NULL,
    updated_by    TEXT      NOT NULL DEFAULT ''
);

INSERT INTO skill_registries_p13 (
    id, display_name, type, location, enabled, trust_policy, pinned_publisher,
    priority, tenant_id, credential_secret_name, auth_type, api_key_header,
    network_policy, created_at, created_by, updated_at, updated_by
)
SELECT id, display_name, type, location, enabled, trust_policy, pinned_publisher,
    priority, tenant_id, credential_secret_name, auth_type, api_key_header,
    network_policy, created_at, created_by, updated_at, updated_by
FROM skill_registries;

DROP TABLE skill_registries;
ALTER TABLE skill_registries_p13 RENAME TO skill_registries;

CREATE INDEX idx_skill_registries_priority ON skill_registries (priority, id);
CREATE INDEX idx_skill_registries_owner ON skill_registries (owner, repository);

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd

-- The git provenance an installed release keeps forever.
--
-- ADD COLUMN, never a rebuild: this table carries every install's provenance
-- and must not be dropped. Every column is written ONCE, at install, from what
-- AO itself resolved -- never from a descriptor a publisher wrote, and never
-- again afterwards.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN source_provider TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN source_owner TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN source_repository TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- The tag AS IT WAS at install time. It is a LABEL, kept because it is how a
-- person recognises a version, and it is explicitly not the identity: if the
-- tag later points elsewhere this column still says what it said, because the
-- release this row describes is the commit below.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN source_tag TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- The identity. A full 40-character lowercase SHA, or empty for every install
-- that did not come from a forge.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN source_commit TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN source_path TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- What the forge said about the repository when AO looked: 'public' or
-- 'private'. Display metadata and a warning surface, never a trust input.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN source_visibility TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- When AO fetched these bytes from the forge. Distinct from installed_at,
-- which is when they reached the catalog, and from metadata_fetched_at, which
-- is when the description AO acted on was read.
-- +goose StatementBegin
ALTER TABLE skill_install_origins ADD COLUMN source_fetched_at TIMESTAMP;
-- +goose StatementEnd

-- Where each tag pointed, and whether it has moved since.
-- +goose StatementBegin
CREATE TABLE skill_external_tags (
    registry_id   TEXT      NOT NULL REFERENCES skill_registries (id) ON DELETE CASCADE,
    owner         TEXT      NOT NULL,
    repository    TEXT      NOT NULL,
    tag           TEXT      NOT NULL,
    -- The commit the tag points at as of last_observed_at.
    commit_sha    TEXT      NOT NULL,
    -- When AO first saw this tag at all, and when it last confirmed it. The
    -- first is what an incident review reads.
    first_observed_at TIMESTAMP NOT NULL,
    last_observed_at  TIMESTAMP NOT NULL,
    -- Where it USED to point, and when it moved. Kept rather than overwritten:
    -- an overwrite would leave "this tag moved" unprovable ten minutes later.
    moved_from_commit TEXT  NOT NULL DEFAULT '',
    moved_at      TIMESTAMP,
    PRIMARY KEY (registry_id, owner, repository, tag)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_external_tags_repo ON skill_external_tags (owner, repository);
-- +goose StatementEnd

-- An administrator may now withdraw an owner, a repository or a commit.
--
-- Rebuilt rather than altered because the subject CHECK has to widen and
-- SQLite has no ALTER for one. Nothing references skill_trust_revocations by
-- foreign key, which is what makes this rebuild free -- unlike the one above.
-- +goose StatementBegin
CREATE TABLE skill_trust_revocations_p13 (
    subject       TEXT      NOT NULL CHECK (subject IN (
                                'signing_key','publisher','trust_root',
                                'external_owner','external_repository','external_commit'
                            )),
    -- A key id, a publisher, a trust root id, an owner, 'owner/repo', or
    -- 'owner/repo@<sha>'. Release revocations still do NOT live here: a
    -- release is withdrawn by the registry that published it and recorded on
    -- skill_install_origins, and a second home for the same fact is a second
    -- place to forget to look.
    subject_id    TEXT      NOT NULL,
    reason        TEXT      NOT NULL,
    revoked_at    TIMESTAMP NOT NULL,
    revoked_by    TEXT      NOT NULL DEFAULT '',
    PRIMARY KEY (subject, subject_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO skill_trust_revocations_p13 (subject, subject_id, reason, revoked_at, revoked_by)
SELECT subject, subject_id, reason, revoked_at, revoked_by FROM skill_trust_revocations;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE skill_trust_revocations;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE skill_trust_revocations_p13 RENAME TO skill_trust_revocations;
-- +goose StatementEnd

-- Widen the audit vocabulary for the external decisions.
--
-- NOT ADDED, still: skill_searched. 0164, 0165 and 0166 each recorded the
-- reason and it has not changed. Searching somebody else's forge makes the
-- query MORE sensitive, not less: it can name a package an organization is
-- evaluating, or a repository they suspect.
--
-- ALSO NOT ADDED: any row that could carry a credential or an archive body.
-- The new actions record a tag that moved (two SHAs, both public), a release
-- pinned to a commit (a SHA), and an administrative revocation (an owner, a
-- repository or a commit). None of them has a place for a token.
--
-- Nothing references skill_audit by foreign key, which is what makes this
-- rebuild safe; 0163 through 0166 did the same.
-- +goose StatementBegin
CREATE TABLE skill_audit_p13 (
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
                     'publisher_revoked','trusted_install','trusted_install_refused',
                     'external_release_pinned','external_tag_moved',
                     'external_revoked','external_revocation_lifted','external_install_refused'
                 )),
    skill_id     TEXT      NOT NULL,
    version      TEXT      NOT NULL DEFAULT '',
    project_id   TEXT,
    digest       TEXT      NOT NULL DEFAULT '',
    capabilities TEXT      NOT NULL DEFAULT '[]',
    detail       TEXT      NOT NULL DEFAULT ''
);

INSERT INTO skill_audit_p13 (
    id, occurred_at, actor, action, skill_id, version, project_id,
    digest, capabilities, detail
)
SELECT id, occurred_at, actor, action, skill_id, version, project_id,
    digest, capabilities, detail
FROM skill_audit;

DROP TABLE skill_audit;
ALTER TABLE skill_audit_p13 RENAME TO skill_audit;

CREATE INDEX idx_skill_audit_occurred_at ON skill_audit (occurred_at);
CREATE INDEX idx_skill_audit_skill ON skill_audit (skill_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS skill_external_tags;
-- +goose StatementEnd
