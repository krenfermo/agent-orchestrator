-- P4-E: external work-management integration (Plane).
--
-- Four tables, and the reason there are four rather than one is that they have
-- four different lifetimes:
--
--   work_item_configs        standing configuration. Changes when a person
--                            changes it, and holds the only secret here.
--   work_item_links          durable associations. Outlive runs, outlive
--                            restarts, and are what reconciliation is built on.
--   work_item_sync_outbox    in-flight intent. Drains, retries, and is the one
--                            table that may be empty at rest.
--   work_item_sync_audit     history. Append-only, never read by the sync path.
--
-- WHY NONE OF THEM CARRIES tenant_id, given that P4-E §10 requires tenant
-- isolation. Because P4-C answered this already, and answered it the other way
-- deliberately: only `projects` and `teams` carry a tenant, and everything
-- reached THROUGH a project -- sessions, runs, notifications, usage, memory,
-- the code graph -- resolves its tenancy from projects.tenant_id. 0156 states
-- the reason: a denormalized copy that can drift out of step with the authority
-- it copies is not a safety property, it is a second thing to keep true.
--
-- Every table here hangs off project_id with ON DELETE CASCADE, so the
-- isolation §10 asks for is the isolation the project already has: a user who
-- cannot reach the project cannot reach its Plane configuration, its links, or
-- its sync queue, and that is enforced by the same authz.Authorize() call as
-- everything else rather than by a second mechanism that could disagree.
--
-- AO REMAINS CANONICAL. Nothing here is read by the workflow engine, the
-- lifecycle manager, or the reducer that decides a run's state. The links are
-- an annotation on AO's work, not an input to it -- which is what makes Plane
-- being down a reason to defer a sync and never a reason to stop executing.

-- +goose Up

-- ---------------------------------------------------------------------------
-- 1. Configuration, one row per project.
-- ---------------------------------------------------------------------------
--
-- Project-scoped rather than installation-scoped because two projects in one
-- installation legitimately belong to two different Plane projects -- often in
-- two different workspaces, and under P4-C, two different organizations. An
-- installation-wide row would make the common multi-project case unmappable.
-- +goose StatementBegin
CREATE TABLE work_item_configs (
    project_id   TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    -- The provider vocabulary is domain.WorkItemProvider. No CHECK allowlist,
    -- following the convention 0133/0146 set: Go owns validity, its predicate
    -- fails closed, and the vocabulary is expected to grow.
    provider     TEXT NOT NULL DEFAULT 'plane',
    -- The provider origin WITHOUT its API version prefix. Empty means the
    -- provider's own default (Plane Cloud).
    base_url     TEXT NOT NULL DEFAULT '',
    -- The provider's workspace identifier (a slug for Plane).
    workspace    TEXT NOT NULL DEFAULT '',
    -- The provider-side project this AO project maps to (a UUID for Plane),
    -- and its human prefix, cached for rendering a key like "PROJ-123"
    -- without a lookup.
    external_project_id   TEXT NOT NULL DEFAULT '',
    external_project_name TEXT NOT NULL DEFAULT '',
    external_project_key  TEXT NOT NULL DEFAULT '',

    -- Ciphertext produced by internal/secretbox, exactly as app_settings does
    -- for the SMTP password. The column never holds plaintext, and the key
    -- lives in a 0600 file outside the database -- so a copied ~/.ao/data or a
    -- support bundle does not yield the token.
    --
    -- An API token is a credential AO must PRESENT on every request, so a hash
    -- cannot do the job here any more than it can for SMTP.
    api_token_encrypted TEXT NOT NULL DEFAULT '',

    -- Three independent switches, because they are three different consents.
    --
    -- enabled          may AO talk to the provider at all
    -- sync_states      may AO move a linked item's state
    -- sync_comments    may AO post progress notes
    --
    -- A person who wants the link visible in the UI and nothing written back
    -- turns the last two off. Collapsing them into one flag would make
    -- "show me the link" and "let AO edit my board" the same decision.
    enabled       INTEGER NOT NULL DEFAULT 0,
    sync_states   INTEGER NOT NULL DEFAULT 1,
    sync_comments INTEGER NOT NULL DEFAULT 1,

    -- The last preflight result, so the settings surface can say whether the
    -- connection works without probing on every page load. last_error is
    -- provider text, already truncated by the adapter, and never a header.
    last_check_at      TIMESTAMP,
    last_check_ok      INTEGER NOT NULL DEFAULT 0,
    last_check_error   TEXT NOT NULL DEFAULT '',

    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 2. Links.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE TABLE work_item_links (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- domain.WorkItemLinkScope: 'project', 'run' or 'task'.
    scope       TEXT NOT NULL,
    -- The run id or task id. Empty exactly for a project-scoped link, which
    -- the unique index below relies on.
    scope_id    TEXT NOT NULL DEFAULT '',

    provider    TEXT NOT NULL DEFAULT 'plane',
    workspace   TEXT NOT NULL,
    -- The provider-side project and item. Identifiers ONLY: §5's "persist only
    -- identifiers required for durable reconciliation".
    external_project_id TEXT NOT NULL,
    external_item_id    TEXT NOT NULL,
    -- The human key ("PROJ-123"). Display only; nothing resolves by it,
    -- because a project rename changes it and the UUID above does not.
    external_item_key   TEXT NOT NULL DEFAULT '',

    -- domain.WorkItemLinkOrigin: 'manual' or 'created'. It decides what AO may
    -- do with the item -- an item AO created it may keep updating, an item a
    -- person linked it comments on but never re-titles.
    origin       TEXT NOT NULL DEFAULT 'manual',
    sync_enabled INTEGER NOT NULL DEFAULT 1,

    -- A display cache, explicitly labelled with its age. It is what the UI
    -- renders when the provider is unreachable; last_seen_at is what stops
    -- anybody mistaking it for current.
    last_seen_title TEXT NOT NULL DEFAULT '',
    last_seen_state TEXT NOT NULL DEFAULT '',
    last_seen_at    TIMESTAMP,

    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL,

    CHECK (scope IN ('project','run','task')),
    CHECK ((scope = 'project') = (scope_id = ''))
);
-- +goose StatementEnd

-- One AO thing links to at most one external item. Re-linking replaces rather
-- than accumulating: two links on one run would make "which item does this
-- run update" a question with two answers, and the sync worker would post
-- every note twice.
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_work_item_links_scope
    ON work_item_links (project_id, scope, scope_id);
-- +goose StatementEnd

-- The reconciliation read: "which AO work points at this external item".
-- Needed by the inbound path, which starts from an item and has to find what
-- it is about.
-- +goose StatementBegin
CREATE INDEX idx_work_item_links_external
    ON work_item_links (provider, workspace, external_item_id);
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 3. The sync outbox.
-- ---------------------------------------------------------------------------
--
-- An OUTBOX rather than a goroutine that posts inline, for the reason §9 and
-- §13 both point at: a sync that happens on the lifecycle path can fail the
-- lifecycle. A row written in the same breath as the state change, and drained
-- by a worker that may fail freely, cannot.
--
-- It is also what makes the integration restart-safe without a checkpoint
-- table: the queue IS the checkpoint. A daemon that dies mid-post comes back to
-- a pending row and tries again, and the provider's own external-id dedupe
-- makes that second attempt harmless.
-- +goose StatementBegin
CREATE TABLE work_item_sync_outbox (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    link_id     TEXT NOT NULL REFERENCES work_item_links(id) ON DELETE CASCADE,

    -- domain.WorkItemSyncEvent.
    event       TEXT NOT NULL,
    -- The comment body AO will post, already rendered and bounded. Stored
    -- rather than recomputed so a retry posts the same words the first attempt
    -- would have -- a note that changes between attempts is a note nobody can
    -- deduplicate.
    body        TEXT NOT NULL DEFAULT '',
    -- The target state group, empty when this event moves no state.
    target_state TEXT NOT NULL DEFAULT '',

    -- domain.SyncDedupeKey. UNIQUE: one real-world moment produces exactly one
    -- row, however many times the lifecycle observes it. This is where
    -- duplicate completion callbacks and re-entrant reducers stop.
    dedupe_key  TEXT NOT NULL,

    -- pending -> done, or pending -> failed once attempts are exhausted.
    status      TEXT NOT NULL DEFAULT 'pending',
    attempts    INTEGER NOT NULL DEFAULT 0,
    -- The earliest time a worker may try this row. Backoff is a TIMESTAMP
    -- rather than a sleep so it survives a restart, and so a queue with one
    -- backed-off row does not hold up the rest.
    next_attempt_at TIMESTAMP,
    last_error  TEXT NOT NULL DEFAULT '',

    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL,

    CHECK (status IN ('pending','done','failed'))
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_work_item_sync_outbox_dedupe
    ON work_item_sync_outbox (dedupe_key);
-- +goose StatementEnd

-- The worker's claim query: the due pending rows, oldest first.
-- +goose StatementBegin
CREATE INDEX idx_work_item_sync_outbox_due
    ON work_item_sync_outbox (status, next_attempt_at);
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 4. The audit trail (§14).
-- ---------------------------------------------------------------------------
--
-- Append-only, and deliberately NOT read by anything on the sync path: it
-- exists so an operator can answer "what did AO send to Plane, and what came
-- back", which is a question about history rather than a state the worker
-- needs.
--
-- What it records is the operation, the target and the outcome. What it must
-- never record is the token, a request header, or a response body AO has not
-- truncated -- see the store, which writes only adapter-produced messages.
-- +goose StatementBegin
CREATE TABLE work_item_sync_audit (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    link_id     TEXT NOT NULL DEFAULT '',
    provider    TEXT NOT NULL DEFAULT 'plane',
    operation   TEXT NOT NULL,
    -- The external item this touched, for the operator's "what happened to
    -- PROJ-123" question.
    external_item_id  TEXT NOT NULL DEFAULT '',
    external_item_key TEXT NOT NULL DEFAULT '',
    -- ok | retryable | failed | skipped
    outcome     TEXT NOT NULL,
    -- The adapter's own classified error kind, so an operator can tell a bad
    -- token from an outage without reading prose.
    error_kind  TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    attempts    INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_work_item_sync_audit_project
    ON work_item_sync_audit (project_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS work_item_sync_audit;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS work_item_sync_outbox;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS work_item_links;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS work_item_configs;
-- +goose StatementEnd
