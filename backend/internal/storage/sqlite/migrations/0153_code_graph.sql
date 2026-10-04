-- Code graph: the structural half of project memory.
--
-- P2-A gave AO durable project memory over this database: what a repository IS
-- at the level of modules, documents, dependencies and conventions. What it
-- did not give -- and what internal/codegraph built but never persisted here --
-- is the level below that: the symbols, and the relations between them.
--
-- The gap was concrete. project_memory_relations knows repository -> module ->
-- file and module -> module. domain.NodeSymbol and RelationDefinedIn have
-- existed since 0144 and nothing has ever written one. So an agent asked to
-- "add export permissions to the Supervisor role" could be told which module
-- holds the service and never which function decides the answer, which route
-- reaches it, which table it writes, or which test covers it.
--
-- This schema is where those facts live. Four properties are load-bearing, and
-- each is a rule the brief states directly:
--
--   * ONE canonical graph per registered project. The key is
--     (project_id, repo_id), the same identity project memory uses, and repo_id
--     is derived from the canonical repository root. A task worktree is not a
--     repository, so it cannot mint a second graph -- see P2-E and the linked
--     worktree refusal in projectmemory.EnsureFresh.
--
--   * Staging by generation. Every row carries the generation that wrote it and
--     the generation is part of its primary key, so a full rebuild at N+1
--     accumulates BESIDE the served generation N instead of overwriting it. A
--     reader filters on code_graph_index.served_generation and therefore sees a
--     complete graph or the previous complete graph, never a half-built one.
--     Completion is a single UPDATE that moves served_generation forward; a
--     crash before it leaves the old graph serving and the partial one
--     collectable.
--
--   * Incremental in place. A diff-driven update rewrites only the paths the
--     diff names, at the served generation, inside one transaction. That is
--     atomic by definition, so it needs no staging -- and it is what makes the
--     common case (one file changed) cost one file's work rather than a
--     repository's.
--
--   * Generation-conditioned CAS on the pass itself. Two dispatches starting at
--     once must not both rebuild: ClaimCodeGraphBuild succeeds only from a
--     terminal phase, and every advance is conditional on the generation the
--     caller believes it holds. Exactly one wins, which is the same fence
--     project_memory_index already uses.
--
-- What this schema deliberately does NOT hold is source code. A symbol row
-- carries where the declaration is, what its contract says, and the first
-- sentence its author wrote about it. The file remains the source of truth, and
-- every row carries the content hash and the commit it was observed at so it
-- can be disproved.

-- +goose Up

-- One row per (project, repository): the durable state of the code graph and
-- the pass that maintains it.
-- +goose StatementBegin
CREATE TABLE code_graph_index (
    project_id        TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id           TEXT NOT NULL,
    -- The canonical repository root repo_id was derived from. Stored so the
    -- hashed identity stays explainable, and so a graph built from a worktree
    -- path is detectable rather than silently wrong.
    repo_path         TEXT NOT NULL,
    -- Which MemoryGraph backend produced this graph. 'local' is the in-tree
    -- indexer and the default; an external adapter would name itself here, so
    -- operator output can never mistake one for the other.
    backend           TEXT NOT NULL DEFAULT 'local',
    -- generation is the pass allocator: monotonic, bumped by the claim.
    generation        INTEGER NOT NULL DEFAULT 0,
    -- served_generation is what readers filter on. It moves only when a full
    -- build completes, which is what makes a partial build invisible.
    served_generation INTEGER NOT NULL DEFAULT 0,
    phase             TEXT NOT NULL DEFAULT 'idle'
                      CHECK (phase IN ('idle','building','failed')),
    -- The commit the served graph describes, and the one an in-flight build is
    -- moving to. Separate for the same reason project_memory_index separates
    -- them: a build that dies must not advance the revision the next
    -- incremental update diffs from.
    indexed_commit    TEXT NOT NULL DEFAULT '',
    pending_commit    TEXT NOT NULL DEFAULT '',
    branch            TEXT NOT NULL DEFAULT '',
    -- The repository identity the served graph was derived under (the same
    -- proof P2-D records for memory items). A mismatch means the checkout is
    -- not the one these facts came from, and the graph fails closed.
    repo_identity     TEXT NOT NULL DEFAULT '',
    -- Counts of the SERVED graph, so status is one row read.
    file_count        INTEGER NOT NULL DEFAULT 0,
    symbol_count      INTEGER NOT NULL DEFAULT 0,
    edge_count        INTEGER NOT NULL DEFAULT 0,
    -- What the last sync did. These are the P3-E measurements: parsed versus
    -- reused is the whole claim that an incremental sync is cheaper, and it is
    -- recorded rather than asserted.
    last_sync_kind    TEXT NOT NULL DEFAULT ''
                      CHECK (last_sync_kind IN ('','full','incremental','noop')),
    last_files_parsed INTEGER NOT NULL DEFAULT 0,
    last_files_reused INTEGER NOT NULL DEFAULT 0,
    last_files_removed INTEGER NOT NULL DEFAULT 0,
    last_symbols_added INTEGER NOT NULL DEFAULT 0,
    last_symbols_removed INTEGER NOT NULL DEFAULT 0,
    last_edges_added  INTEGER NOT NULL DEFAULT 0,
    last_edges_removed INTEGER NOT NULL DEFAULT 0,
    last_duration_ms  INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT '',
    -- The bounded architecture summary, rendered at build time from the graph
    -- the build already held in memory. Derived once and served from one row,
    -- rather than recomputed per dispatch over every file.
    architecture      TEXT NOT NULL DEFAULT '',
    architecture_json TEXT NOT NULL DEFAULT '',
    started_at        TIMESTAMP,
    completed_at      TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL,
    PRIMARY KEY (project_id, repo_id),
    CHECK (generation >= 0),
    CHECK (served_generation >= 0)
);
-- +goose StatementEnd

-- One indexed file. content_hash is the gate that lets a pass skip a file it
-- has already read, and it is why an unchanged repository costs a read and a
-- hash per file instead of a parse.
-- +goose StatementBegin
CREATE TABLE code_graph_files (
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id      TEXT NOT NULL,
    generation   INTEGER NOT NULL,
    path         TEXT NOT NULL,
    language     TEXT NOT NULL DEFAULT '',
    -- source | test | migration | query | generated. Retrieval reads it to
    -- prefer a hand-written definition over a generated one, and to answer
    -- "which tests cover this" without pattern-matching names at query time.
    role         TEXT NOT NULL DEFAULT 'source',
    content_hash TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    updated_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (project_id, repo_id, generation, path)
);
-- +goose StatementEnd

-- One declaration. symbol_id is "<path>#<kind>:<qualified name>" and is stable
-- for as long as the symbol keeps those three; line and end_line are
-- observations of where it sat, deliberately NOT part of its identity, so a
-- reformat does not read as a rewrite.
-- +goose StatementBegin
CREATE TABLE code_graph_symbols (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id     TEXT NOT NULL,
    generation  INTEGER NOT NULL,
    symbol_id   TEXT NOT NULL,
    path        TEXT NOT NULL,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    language    TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    end_line    INTEGER NOT NULL DEFAULT 0,
    signature   TEXT NOT NULL DEFAULT '',
    doc         TEXT NOT NULL DEFAULT '',
    -- The bounded description a context pack shows.
    summary     TEXT NOT NULL DEFAULT '',
    -- What produced summary. 'static' is a mechanical derivation from the
    -- declaration; anything else must name its provider, so a generated
    -- sentence can never be mistaken for an observed fact.
    summary_source TEXT NOT NULL DEFAULT 'static',
    exported    INTEGER NOT NULL DEFAULT 0,
    body_hash   TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (project_id, repo_id, generation, symbol_id)
);
-- +goose StatementEnd

-- One relation. The target is a NAME rather than a resolved id on purpose:
-- resolving "service.Delete" to a declaration needs type information the
-- indexer does not build, and a resolved-looking edge that guessed wrong is
-- the one failure a reviewer cannot detect. Resolution happens at read time,
-- where the ambiguity is visible.
--
-- path is the file the relation was observed in, which is its provenance
-- together with the index's indexed_commit. It is also what makes deletion
-- exact: dropping a path drops its edges, so a deleted file cannot leave a
-- dangling edge behind.
-- +goose StatementBegin
CREATE TABLE code_graph_edges (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id     TEXT NOT NULL,
    generation  INTEGER NOT NULL,
    edge_id     TEXT NOT NULL,
    path        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    from_key    TEXT NOT NULL,
    to_key      TEXT NOT NULL,
    line        INTEGER NOT NULL DEFAULT 0,
    updated_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (project_id, repo_id, generation, edge_id)
);
-- +goose StatementEnd

-- Retrieval reads symbols by name and by path; both are indexed because both
-- are on the dispatch path, where a scan of a 100k-symbol table would be paid
-- for on every task.
--
-- Each index ends with the column its query ORDERs BY, and that trailing column
-- is load-bearing rather than tidy. Without it the row order has to be produced
-- by a sort, and SQLite -- offered a primary key that delivers
-- (project, repo, generation) equality AND the required order for free --
-- prefers to walk the whole generation and filter, which is a full scan of
-- every symbol in the repository. Measurement caught it: an "indexed" lookup of
-- one file's relations was taking 340ms against a 130k-edge table. With the
-- ordering column in the index there is no sort to avoid, and the selective
-- index wins.
-- +goose StatementBegin
CREATE INDEX idx_code_graph_symbols_name
    ON code_graph_symbols (project_id, repo_id, generation, name, symbol_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_code_graph_symbols_path
    ON code_graph_symbols (project_id, repo_id, generation, path, symbol_id);
-- +goose StatementEnd

-- Traversal in both directions: what does this reach, and what reaches it.
-- +goose StatementBegin
CREATE INDEX idx_code_graph_edges_from
    ON code_graph_edges (project_id, repo_id, generation, from_key, edge_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_code_graph_edges_to
    ON code_graph_edges (project_id, repo_id, generation, to_key, edge_id);
-- +goose StatementEnd

-- Deleting or re-writing one path's rows, which is the whole of an incremental
-- update's write path.
-- +goose StatementBegin
CREATE INDEX idx_code_graph_edges_path
    ON code_graph_edges (project_id, repo_id, generation, path, edge_id);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_code_graph_edges_path;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_code_graph_edges_to;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_code_graph_edges_from;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_code_graph_symbols_path;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_code_graph_symbols_name;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS code_graph_edges;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS code_graph_symbols;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS code_graph_files;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS code_graph_index;
-- +goose StatementEnd
