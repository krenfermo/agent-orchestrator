-- Skills phase 11: connectivity to a real private registry.
--
-- Phase 10 shipped the contract and a local-directory provider. This adds the
-- durable half of reaching one over the network: how AO authenticates, what it
-- may connect to, what the last attempt said, and what the registry has since
-- withdrawn.
--
-- Four changes, each answering a question the previous phase could not:
--
--   skill_registries              (rebuilt) auth type, api key header, network
--                                 policy -- the three things a remote registry
--                                 needs and a directory has no use for
--   skill_registry_status         what the last connection test and the last
--                                 sync actually said
--   skill_registry_revocations    what a registry says is withdrawn, kept so
--                                 the answer survives being offline
--   skill_audit                   (widened) the connectivity decisions
--
-- WHY skill_registries IS REBUILT AND NOT ALTERED. SQLite has no ALTER for a
-- CHECK, and auth_type without one would be a column that accepts 'bearrer'
-- and silently authenticates as nobody. The rebuild is safe here for the
-- reason 0163's and 0164's were: NOTHING references skill_registries by
-- foreign key -- skill_install_origins deliberately COPIES the registry
-- identity rather than joining it, so an installed package's provenance
-- outlives the registry row. There is no incoming reference to park, which is
-- why this rebuild is absent from knownTableRebuilds in
-- migrate_rebuild_fk_safety_test.go rather than declared in it.
--
-- WHY THE CREDENTIAL IS STILL ONLY A NAME. Nothing below stores a secret
-- value, and there is no column one could go in. credential_secret_name names
-- a row in skill_secrets, which internal/secretbox seals under an
-- administrator-managed key. auth_type says how that value is presented;
-- api_key_header says under which header name. A name and a header are
-- configuration, printed freely. The value is neither.
--
-- WHY REVOCATIONS ARE A TABLE AND THE AVAILABLE CATALOG IS STILL NOT. 0164
-- recorded why there is no cached listing of what a registry offers: a cached
-- listing would let an install act on a release that was withdrawn since
-- somebody last looked. A revocation is the exact opposite fact. Caching "this
-- release is fine" can only ever be wrong in the dangerous direction; caching
-- "this release is withdrawn" can only ever be wrong in the safe one. So the
-- withdrawal is persisted and the offer is not, and an install that cannot
-- reach the registry consults what it already knows rather than reading
-- silence as consent.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE skill_registries_new (
    id            TEXT      PRIMARY KEY,
    display_name  TEXT      NOT NULL,
    -- git is still DECLARED and refused by the Go validator; https became
    -- readable in this phase.
    type          TEXT      NOT NULL CHECK (type IN ('local','https','git')),
    -- For https this is the baseURL, reduced by the Go validator to exactly
    -- one origin: https only, one host, one port, no credentials, no query.
    -- A registry whose address cannot be bounded to one origin is a client
    -- that fetches wherever it is told, which is an SSRF gadget.
    location      TEXT      NOT NULL,
    enabled       INTEGER   NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    trust_policy  TEXT      NOT NULL CHECK (trust_policy IN ('digest','pinned_publisher','signed')),
    pinned_publisher TEXT   NOT NULL DEFAULT '',
    priority      INTEGER   NOT NULL DEFAULT 100,
    tenant_id     TEXT      REFERENCES tenants (id) ON DELETE CASCADE,
    -- The NAME of a sealed secret, never a value.
    credential_secret_name TEXT NOT NULL DEFAULT '',
    -- How that credential is presented. Explicit rather than inferred from the
    -- presence of a secret: "AO guessed bearer and the registry wanted a
    -- header" is a 401 nobody can debug from a settings screen.
    --
    -- There is no 'basic' and no 'query': basic is a password with a worse
    -- cache story, and a query string ends up in access logs, proxy logs and
    -- referrers, which is the definition of a credential somewhere it should
    -- not be.
    auth_type     TEXT      NOT NULL DEFAULT 'none'
                            CHECK (auth_type IN ('none','bearer','api_key_header')),
    -- The header NAME for api_key_header. Configuration, printed freely.
    api_key_header TEXT     NOT NULL DEFAULT '',
    -- The network policy as JSON: today, the private CIDRs this registry --
    -- and only this registry -- may resolve into. It exists because refusing
    -- every private address is right by default and wrong for an installation
    -- whose registry genuinely lives at 10.x, and a control nobody can use is
    -- a control that gets turned off wholesale.
    --
    -- It can never re-open link-local: skillegress.ParsePermittedCIDRs refuses
    -- an overlapping entry, and it is the same validator the skill egress
    -- proxy uses. One policy, two callers.
    network_policy TEXT     NOT NULL DEFAULT '{}',
    created_at    TIMESTAMP NOT NULL,
    created_by    TEXT      NOT NULL DEFAULT '',
    updated_at    TIMESTAMP NOT NULL,
    updated_by    TEXT      NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO skill_registries_new (
    id, display_name, type, location, enabled, trust_policy, pinned_publisher,
    priority, tenant_id, credential_secret_name, created_at, created_by,
    updated_at, updated_by
)
SELECT id, display_name, type, location, enabled, trust_policy, pinned_publisher,
    priority, tenant_id, credential_secret_name, created_at, created_by,
    updated_at, updated_by
FROM skill_registries;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE skill_registries;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE skill_registries_new RENAME TO skill_registries;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_registries_priority ON skill_registries (priority, id);
-- +goose StatementEnd

-- The last thing AO actually observed about one registry.
--
-- It is a SIDE TABLE rather than columns on skill_registries because it has a
-- different lifetime and a different author: the configuration is what an
-- administrator wrote, and this is what the network said. Mixing them would
-- make "when did somebody last change this registry" and "when did it last
-- answer" the same timestamp, and they are different questions.
-- +goose StatementBegin
CREATE TABLE skill_registry_status (
    registry_id   TEXT      PRIMARY KEY REFERENCES skill_registries (id) ON DELETE CASCADE,
    -- The six states a connection test can report. CONNECTED is deliberately
    -- the hardest to reach: a socket opening, TLS verifying and a 200 arriving
    -- are each necessary and none is sufficient, because a load balancer, a
    -- captive portal and an unrelated service on the right port all produce
    -- one. CONNECTED means the registry answered AS ITSELF, speaking a
    -- protocol version this build knows.
    last_probe_state TEXT   NOT NULL DEFAULT ''
                            CHECK (last_probe_state IN (
                                '','CONNECTED','AUTH_FAILED','TLS_FAILED',
                                'UNREACHABLE','INVALID_RESPONSE','POLICY_BLOCKED'
                            )),
    -- The sentence to show. It names a secretRef on an auth failure and never
    -- a credential value.
    last_probe_detail TEXT  NOT NULL DEFAULT '',
    last_probe_at    TIMESTAMP,
    last_probe_latency_ms INTEGER NOT NULL DEFAULT 0,
    -- The last successful metadata read, which is what "as of" on a stale
    -- listing is measured against.
    last_sync_at     TIMESTAMP,
    -- The last revocation sync, separately: a registry can be reachable for
    -- metadata and still have had its withdrawal list not read today.
    last_revocation_sync_at TIMESTAMP,
    updated_at    TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- What a registry says is withdrawn.
--
-- Kept per (registry, skill, version) rather than merged into
-- skill_install_origins because the two answer different questions and only one
-- of them is about something installed here: a revocation must be able to block
-- an install that has not happened yet, including while the registry is
-- unreachable.
-- +goose StatementBegin
CREATE TABLE skill_registry_revocations (
    registry_id   TEXT      NOT NULL REFERENCES skill_registries (id) ON DELETE CASCADE,
    skill_id      TEXT      NOT NULL,
    version       TEXT      NOT NULL,
    -- A revocation that does not say why is indistinguishable from a mistake,
    -- and it is about to block installs. The Go validator requires it.
    reason        TEXT      NOT NULL,
    -- When the REGISTRY says it withdrew the release, and when AO itself
    -- observed it. Two facts, and only the second one is AO's.
    revoked_at    TIMESTAMP,
    observed_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (registry_id, skill_id, version)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_registry_revocations_skill ON skill_registry_revocations (skill_id, version);
-- +goose StatementEnd

-- Widen the audit vocabulary for the connectivity decisions, in the same trail
-- that already answers "who installed this package".
--
-- NOT ADDED, still deliberately: skill_searched. 0164 recorded the reason and
-- it has not changed -- a search query is text a person typed, it can name an
-- internal package or a vulnerability they are hunting, and storing it would be
-- storing something worth not storing. Reaching a private registry does not
-- make the query worth keeping; it makes it more sensitive.
--
-- ALSO NOT ADDED: any row that could carry a credential. The new actions record
-- that a fetch started, that one was refused, that a cached artifact was
-- reused, and that a connection test ran. None of them has a place for a token,
-- a full header set, or an artifact body, and the Go layer builds their detail
-- strings from a secretRef and a digest.
--
-- SQLite has no ALTER for a CHECK, so this is the documented rebuild. Nothing
-- references skill_audit by foreign key, which is what makes it safe; 0163 and
-- 0164 did the same.
-- +goose StatementBegin
CREATE TABLE skill_audit_new (
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
                     'cached_artifact_used','registry_revocations_synced'
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
DROP TABLE IF EXISTS skill_registry_revocations;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_registry_status;
-- +goose StatementEnd
