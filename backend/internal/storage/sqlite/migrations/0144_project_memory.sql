-- P2-A: durable, incremental project memory.
--
-- Before this migration AO had two memory-shaped stores and no production
-- writer for either: internal/codegraph wrote a per-project JSON graph and
-- internal/projectmemory wrote a per-project JSON fact file, and the only
-- non-test callers of both constructed them as READERS (see
-- docs/p2-project-memory-audit.md §4.2). Every planner call therefore re-read
-- the same six documents, every plan-reuse assessment re-read them again, and
-- every Worker/Reviewer/Repair harness re-derived the repository's shape from
-- scratch inside its own worktree.
--
-- Project memory is the durable, provenance-carrying cache that lets those
-- roles skip what has not changed. It lives in SQLite rather than beside the
-- JSON stores for four reasons that are all load-bearing:
--
--   * Generation-conditioned CAS. An indexing pass that stalls and wakes up
--     after a newer pass finished must not overwrite the newer pass's memory.
--     "The write applied only if the row's generation is still mine" is a
--     WHERE clause here; over a read-modify-write JSON file it is a hope.
--   * Path-indexed invalidation. Incremental update asks "which items were
--     derived from this changed path". That is an indexed join
--     (project_memory_sources), not a full scan of every fact.
--   * A restart-safe cursor. project_memory_index is both the resume point and
--     the generation allocator, so a crash mid-index resumes instead of
--     starting over, and two daemons cannot index one repository at once.
--   * One transaction. An item, its source rows and its relations are written
--     together or not at all, so a partial write cannot leave a fact whose
--     provenance says it came from nowhere.
--
-- What this schema deliberately does NOT do is become a source of truth. Every
-- row is derived from the repository or from AO's own durable workflow facts,
-- and every row carries enough provenance (source paths, source commit, source
-- digest) to be *disproved*. A fact that cannot be shown to still hold is
-- marked stale and withheld rather than served -- the fail-closed rule in
-- docs/project-memory.md.

-- +goose Up

-- One row per (project, repository). It is the pass's durable state machine:
-- which generation is current, which phase a pass reached, which path it had
-- got to, and which commit the last COMPLETED pass indexed at.
--
-- indexed_commit and pending_commit are separate on purpose. A pass that dies
-- must not advance the commit incremental update diffs from, or the changes it
-- never got to would be invisible forever; pending_commit holds the in-flight
-- target and is promoted only on completion.
-- +goose StatementBegin
CREATE TABLE project_memory_index (
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id           TEXT NOT NULL,
    -- The absolute repository root repo_id was hashed from. Stored so the
    -- hashed identity stays explainable and a moved checkout is detectable
    -- rather than silently mismatched.
    repo_path         TEXT NOT NULL,
    -- Monotonic. Allocated by the same conditional write that claims a pass,
    -- which is what makes concurrent index attempts resolve to one winner.
    generation        INTEGER NOT NULL DEFAULT 0,
    phase             TEXT NOT NULL DEFAULT 'idle'
                      CHECK (phase IN ('idle','scanning','summarizing','linking','finalizing','failed')),
    indexed_commit    TEXT NOT NULL DEFAULT '',
    pending_commit    TEXT NOT NULL DEFAULT '',
    branch            TEXT NOT NULL DEFAULT '',
    -- Resume point within the current phase: the last path the scan admitted.
    resume_cursor     TEXT NOT NULL DEFAULT '',
    files_seen        INTEGER NOT NULL DEFAULT 0,
    files_indexed     INTEGER NOT NULL DEFAULT 0,
    files_skipped     INTEGER NOT NULL DEFAULT 0,
    items_written     INTEGER NOT NULL DEFAULT 0,
    relations_written INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT '',
    started_at        TIMESTAMP,
    completed_at      TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL,
    PRIMARY KEY (project_id, repo_id)
);
-- +goose StatementEnd

-- One durable fact. id is derived from (project, repo, type, scope, key) in Go
-- (domain.ProjectMemoryKey.ID), so two indexers that discover the same fact
-- address the same row and re-indexing is an UPDATE rather than an append.
--
-- The two CHECKs encode the drift model in the schema instead of in whichever
-- writer ran last: a non-valid item must say why it is not valid, and a
-- task-local item must name the task whose unintegrated view it carries.
-- +goose StatementBegin
CREATE TABLE project_memory_items (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id           TEXT NOT NULL,
    item_type         TEXT NOT NULL,
    scope             TEXT NOT NULL,
    item_key          TEXT NOT NULL DEFAULT '',
    -- 'canonical' is integrated repository knowledge; 'task_local' is one
    -- task's view of changes that are not integrated anywhere and must never
    -- become another task's premise.
    origin            TEXT NOT NULL DEFAULT 'canonical'
                      CHECK (origin IN ('canonical','task_local')),
    origin_ref        TEXT NOT NULL DEFAULT '',
    summary           TEXT NOT NULL,
    content           TEXT NOT NULL DEFAULT '',
    source_paths_json TEXT NOT NULL DEFAULT '[]',
    source_commit     TEXT NOT NULL DEFAULT '',
    -- Hash over the content of source_paths as they stood at source_commit.
    -- It is what lets drift detection answer "did my sources actually move"
    -- without re-deriving the fact, and it is why a branch that moves without
    -- touching the sources invalidates nothing.
    source_digest     TEXT NOT NULL DEFAULT '',
    generation        INTEGER NOT NULL DEFAULT 0,
    state             TEXT NOT NULL DEFAULT 'valid'
                      CHECK (state IN ('valid','stale','invalidated','rebuilding')),
    state_reason      TEXT NOT NULL DEFAULT '',
    confidence        REAL NOT NULL DEFAULT 0,
    metadata_json     TEXT NOT NULL DEFAULT '{}',
    content_hash      TEXT NOT NULL,
    created_at        TIMESTAMP NOT NULL,
    updated_at        TIMESTAMP NOT NULL,
    invalidated_at    TIMESTAMP,
    CHECK (state = 'valid' OR state_reason <> ''),
    CHECK (origin = 'canonical' OR origin_ref <> ''),
    CHECK (confidence >= 0 AND confidence <= 1),
    CHECK (generation >= 0)
);
-- +goose StatementEnd

-- The selection read: everything canonical and valid for one repository, by
-- type. Context-pack assembly is on the dispatch path, so it must not scan.
-- +goose StatementBegin
CREATE INDEX idx_project_memory_items_lookup
    ON project_memory_items (project_id, repo_id, state, origin, item_type);
-- +goose StatementEnd

-- The task-local read: one task's own unintegrated facts, and the sweep that
-- retires them when the task ends.
-- +goose StatementBegin
CREATE INDEX idx_project_memory_items_origin_ref
    ON project_memory_items (project_id, origin, origin_ref);
-- +goose StatementEnd

-- The retire-what-a-pass-superseded read: items of this repository left behind
-- at an older generation.
-- +goose StatementBegin
CREATE INDEX idx_project_memory_items_generation
    ON project_memory_items (project_id, repo_id, generation);
-- +goose StatementEnd

-- One durable edge. The endpoints are (kind, key) rather than memory-item ids
-- so a relation may name a module that has no summary item yet: the graph and
-- the item set are allowed to be at different completeness, and neither blocks
-- the other.
--
-- This table IS the default MemoryGraph backend. An optional Grae/Graphify
-- adapter is a second implementation of the same port, never a replacement for
-- this one -- see docs/project-memory.md, "Connecting an external graph".
-- +goose StatementBegin
CREATE TABLE project_memory_relations (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id           TEXT NOT NULL,
    from_kind         TEXT NOT NULL,
    from_key          TEXT NOT NULL,
    relation_kind     TEXT NOT NULL,
    to_kind           TEXT NOT NULL,
    to_key            TEXT NOT NULL,
    origin            TEXT NOT NULL DEFAULT 'canonical'
                      CHECK (origin IN ('canonical','task_local')),
    origin_ref        TEXT NOT NULL DEFAULT '',
    source_paths_json TEXT NOT NULL DEFAULT '[]',
    source_commit     TEXT NOT NULL DEFAULT '',
    source_digest     TEXT NOT NULL DEFAULT '',
    generation        INTEGER NOT NULL DEFAULT 0,
    state             TEXT NOT NULL DEFAULT 'valid'
                      CHECK (state IN ('valid','stale','invalidated','rebuilding')),
    state_reason      TEXT NOT NULL DEFAULT '',
    confidence        REAL NOT NULL DEFAULT 0,
    metadata_json     TEXT NOT NULL DEFAULT '{}',
    created_at        TIMESTAMP NOT NULL,
    updated_at        TIMESTAMP NOT NULL,
    invalidated_at    TIMESTAMP,
    CHECK (state = 'valid' OR state_reason <> ''),
    CHECK (origin = 'canonical' OR origin_ref <> ''),
    CHECK (confidence >= 0 AND confidence <= 1),
    CHECK (generation >= 0)
);
-- +goose StatementEnd

-- Traversal in both directions, and the generation sweep.
-- +goose StatementBegin
CREATE INDEX idx_project_memory_relations_from
    ON project_memory_relations (project_id, repo_id, from_kind, from_key, state);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_project_memory_relations_to
    ON project_memory_relations (project_id, repo_id, to_kind, to_key, state);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_project_memory_relations_generation
    ON project_memory_relations (project_id, repo_id, generation);
-- +goose StatementEnd

-- The provenance index: one row per (owner, path). Incremental update's whole
-- job is "this path changed -- what did it prove", and that question has to be
-- an indexed lookup rather than a scan of every fact's JSON path list.
--
-- The JSON copy on the owning row stays authoritative for reading a fact back;
-- this table exists to make the reverse lookup cheap, and is rewritten with
-- its owner inside the same transaction so the two cannot drift.
-- +goose StatementBegin
CREATE TABLE project_memory_sources (
    owner_kind TEXT NOT NULL CHECK (owner_kind IN ('item','relation')),
    owner_id   TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id    TEXT NOT NULL,
    path       TEXT NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, path)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_project_memory_sources_path
    ON project_memory_sources (project_id, repo_id, path);
-- +goose StatementEnd

-- One row per file the indexer admitted, with the digest it was admitted at.
-- It is what makes an incremental pass incremental without a git diff: a file
-- whose digest still matches was not re-read, and a file that has disappeared
-- from the walk is a deletion. It is also how a resumed pass knows which paths
-- the crashed pass had already finished.
-- +goose StatementBegin
CREATE TABLE project_memory_files (
    project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id        TEXT NOT NULL,
    path           TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    size_bytes     INTEGER NOT NULL DEFAULT 0,
    -- The generation that last SAW this path. A file left at an older
    -- generation after a completed pass is one the walk no longer finds:
    -- deleted, renamed away, or newly excluded by the bounds.
    generation     INTEGER NOT NULL DEFAULT 0,
    indexed_commit TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMP NOT NULL,
    PRIMARY KEY (project_id, repo_id, path)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_project_memory_files_generation
    ON project_memory_files (project_id, repo_id, generation);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_files_generation;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS project_memory_files;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_sources_path;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS project_memory_sources;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_relations_generation;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_relations_to;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_relations_from;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS project_memory_relations;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_items_generation;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_items_origin_ref;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_items_lookup;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS project_memory_items;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS project_memory_index;
-- +goose StatementEnd
