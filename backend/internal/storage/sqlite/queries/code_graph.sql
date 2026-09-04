-- Code graph (migration 0153).
--
-- A note on every ORDER BY here: it leads with the columns the WHERE clause
-- pins, not with the tiebreak alone. That is not style. SQLite will happily
-- choose the primary key over a selective index when the primary key also
-- delivers the requested order, and then scan an entire generation to filter
-- one path out of it -- which is exactly what a 340ms "indexed" lookup turned
-- out to be. Ordering by the index's own column prefix leaves nothing to sort,
-- and the selective index wins.
--
-- Two write disciplines live here and they must not be confused.
--
--   * A FULL build writes at a staging generation and is made visible by one
--     statement, CompleteCodeGraphBuild. Nothing it writes is readable before
--     that, because readers filter on served_generation.
--   * An INCREMENTAL update writes at the served generation, inside one
--     transaction. It is atomic by definition, so it needs no staging.
--
-- Every statement that advances the pass is generation-conditioned. A build
-- that stalls and wakes up after a newer one finished matches zero rows.

-- name: GetCodeGraphIndex :one
SELECT * FROM code_graph_index WHERE project_id = ? AND repo_id = ?;

-- name: ListCodeGraphIndexes :many
SELECT * FROM code_graph_index WHERE project_id = ? ORDER BY repo_id;

-- name: EnsureCodeGraphIndex :execrows
-- Register a repository for code-graph indexing. Idempotent.
INSERT OR IGNORE INTO code_graph_index (
    project_id, repo_id, repo_path, backend, updated_at
) VALUES (?, ?, ?, ?, ?);

-- name: UpdateCodeGraphRepoPath :execrows
UPDATE code_graph_index SET repo_path = ?, backend = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ?;

-- name: ClaimCodeGraphBuild :execrows
-- Take the next generation and claim a full build, in one atomic statement.
-- It applies only from a terminal phase, so a second caller arriving during a
-- live build matches zero rows and reads that as "somebody else is building".
UPDATE code_graph_index
SET generation = generation + 1,
    phase = 'building',
    pending_commit = ?,
    branch = ?,
    last_error = '',
    started_at = ?, completed_at = NULL, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND phase IN ('idle','failed');

-- name: ReclaimCodeGraphBuild :execrows
-- Take over a build left in flight by a crash, conditional on the generation
-- the caller read. Two restarts racing to recover one abandoned build cannot
-- both proceed.
UPDATE code_graph_index
SET pending_commit = ?, branch = ?, last_error = '', started_at = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND generation = ? AND phase = 'building';

-- name: CompleteCodeGraphBuild :execrows
-- Publish a completed build. This single statement is what makes a partial
-- graph invisible: until it runs, readers are still served the previous
-- generation.
UPDATE code_graph_index
SET served_generation = ?,
    phase = 'idle',
    indexed_commit = ?,
    pending_commit = '',
    repo_identity = ?,
    file_count = ?, symbol_count = ?, edge_count = ?,
    last_sync_kind = ?,
    last_files_parsed = ?, last_files_reused = ?, last_files_removed = ?,
    last_symbols_added = ?, last_symbols_removed = ?,
    last_edges_added = ?, last_edges_removed = ?,
    last_duration_ms = ?,
    architecture = ?, architecture_json = ?,
    last_error = '',
    completed_at = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND generation = ?;

-- name: RecordCodeGraphIncremental :execrows
-- Record what an incremental update did. It does NOT move served_generation:
-- an incremental update wrote in place at the generation already being served.
UPDATE code_graph_index
SET indexed_commit = ?,
    repo_identity = ?,
    file_count = ?, symbol_count = ?, edge_count = ?,
    last_sync_kind = ?,
    last_files_parsed = ?, last_files_reused = ?, last_files_removed = ?,
    last_symbols_added = ?, last_symbols_removed = ?,
    last_edges_added = ?, last_edges_removed = ?,
    last_duration_ms = ?,
    architecture = ?, architecture_json = ?,
    last_error = '',
    completed_at = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND served_generation = ? AND phase = 'idle';

-- name: FailCodeGraphBuild :execrows
-- End a build on an error, keeping the generation so a reclaim can prove which
-- attempt it is taking over. served_generation is untouched, so the previous
-- complete graph keeps serving.
UPDATE code_graph_index
SET phase = 'failed', last_error = ?, pending_commit = '', completed_at = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND generation = ?;

-- name: PutCodeGraphFile :exec
INSERT INTO code_graph_files (
    project_id, repo_id, generation, path, language, role, content_hash, size_bytes, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, repo_id, generation, path) DO UPDATE SET
    language = excluded.language,
    role = excluded.role,
    content_hash = excluded.content_hash,
    size_bytes = excluded.size_bytes,
    updated_at = excluded.updated_at;

-- name: PutCodeGraphSymbol :exec
INSERT INTO code_graph_symbols (
    project_id, repo_id, generation, symbol_id, path, name, kind, language,
    line, end_line, signature, doc, summary, summary_source, exported, body_hash, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, repo_id, generation, symbol_id) DO UPDATE SET
    path = excluded.path,
    name = excluded.name,
    kind = excluded.kind,
    language = excluded.language,
    line = excluded.line,
    end_line = excluded.end_line,
    signature = excluded.signature,
    doc = excluded.doc,
    summary = excluded.summary,
    summary_source = excluded.summary_source,
    exported = excluded.exported,
    body_hash = excluded.body_hash,
    updated_at = excluded.updated_at;

-- name: PutCodeGraphEdge :exec
INSERT INTO code_graph_edges (
    project_id, repo_id, generation, edge_id, path, kind, from_key, to_key, line, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, repo_id, generation, edge_id) DO UPDATE SET
    path = excluded.path,
    kind = excluded.kind,
    from_key = excluded.from_key,
    to_key = excluded.to_key,
    line = excluded.line,
    updated_at = excluded.updated_at;

-- name: GetCodeGraphFile :one
SELECT * FROM code_graph_files
WHERE project_id = ? AND repo_id = ? AND generation = ? AND path = ?;

-- name: ListCodeGraphFiles :many
SELECT * FROM code_graph_files
WHERE project_id = ? AND repo_id = ? AND generation = ?
ORDER BY path;

-- name: CountCodeGraphRows :one
SELECT
    (SELECT COUNT(*) FROM code_graph_files AS f
        WHERE f.project_id = ? AND f.repo_id = ? AND f.generation = ?) AS file_count,
    (SELECT COUNT(*) FROM code_graph_symbols AS s
        WHERE s.project_id = ? AND s.repo_id = ? AND s.generation = ?) AS symbol_count,
    (SELECT COUNT(*) FROM code_graph_edges AS e
        WHERE e.project_id = ? AND e.repo_id = ? AND e.generation = ?) AS edge_count;

-- name: CountCodeGraphSymbolsForPath :one
SELECT COUNT(*) FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation = ? AND path = ?;

-- name: CountCodeGraphEdgesForPath :one
SELECT COUNT(*) FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation = ? AND path = ?;

-- name: DeleteCodeGraphFile :execrows
DELETE FROM code_graph_files
WHERE project_id = ? AND repo_id = ? AND generation = ? AND path = ?;

-- name: DeleteCodeGraphSymbolsForPath :execrows
DELETE FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation = ? AND path = ?;

-- name: DeleteCodeGraphEdgesForPath :execrows
DELETE FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation = ? AND path = ?;

-- name: CopyCodeGraphFileForward :execrows
-- Carry one unchanged path from the served generation into the staging one
-- without re-reading or re-parsing it. This is what makes a full rebuild of a
-- quiet repository cheap: the file is hashed, matched, and copied by the
-- database rather than analysed again.
INSERT OR REPLACE INTO code_graph_files (
    project_id, repo_id, generation, path, language, role, content_hash, size_bytes, updated_at
)
SELECT src.project_id, src.repo_id, @to_generation, src.path, src.language, src.role,
       src.content_hash, src.size_bytes, src.updated_at
FROM code_graph_files AS src
WHERE src.project_id = @project_id AND src.repo_id = @repo_id
  AND src.generation = @from_generation AND src.path = @path;

-- name: CopyCodeGraphSymbolsForward :execrows
INSERT OR REPLACE INTO code_graph_symbols (
    project_id, repo_id, generation, symbol_id, path, name, kind, language,
    line, end_line, signature, doc, summary, summary_source, exported, body_hash, updated_at
)
SELECT src.project_id, src.repo_id, @to_generation, src.symbol_id, src.path, src.name,
       src.kind, src.language, src.line, src.end_line, src.signature, src.doc, src.summary,
       src.summary_source, src.exported, src.body_hash, src.updated_at
FROM code_graph_symbols AS src
WHERE src.project_id = @project_id AND src.repo_id = @repo_id
  AND src.generation = @from_generation AND src.path = @path;

-- name: CopyCodeGraphEdgesForward :execrows
INSERT OR REPLACE INTO code_graph_edges (
    project_id, repo_id, generation, edge_id, path, kind, from_key, to_key, line, updated_at
)
SELECT src.project_id, src.repo_id, @to_generation, src.edge_id, src.path, src.kind,
       src.from_key, src.to_key, src.line, src.updated_at
FROM code_graph_edges AS src
WHERE src.project_id = @project_id AND src.repo_id = @repo_id
  AND src.generation = @from_generation AND src.path = @path;

-- name: PruneCodeGraphFilesBelowGeneration :execrows
DELETE FROM code_graph_files
WHERE project_id = ? AND repo_id = ? AND generation < ?;

-- name: PruneCodeGraphSymbolsBelowGeneration :execrows
DELETE FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation < ?;

-- name: PruneCodeGraphEdgesBelowGeneration :execrows
DELETE FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation < ?;

-- name: PruneCodeGraphFilesAboveGeneration :execrows
-- Drop an abandoned staging generation. It is the collect half of the staging
-- rule: a build that died left rows nobody can see, and nobody ever will.
DELETE FROM code_graph_files
WHERE project_id = ? AND repo_id = ? AND generation > ?;

-- name: PruneCodeGraphSymbolsAboveGeneration :execrows
DELETE FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation > ?;

-- name: PruneCodeGraphEdgesAboveGeneration :execrows
DELETE FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation > ?;

-- name: ListCodeGraphSymbolsForPath :many
SELECT * FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation = ? AND path = ?
ORDER BY path, symbol_id;

-- name: ListCodeGraphSymbolsByName :many
SELECT * FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation = ? AND name = ?
ORDER BY name, symbol_id
LIMIT ?;

-- name: SearchCodeGraphSymbols :many
-- The candidate read behind task-to-graph retrieval. It matches a term against
-- the symbol's name, its path and its summary, and it is bounded in SQL rather
-- than in Go so a 100k-symbol graph never crosses the boundary.
--
-- instr() rather than LIKE: a task term is arbitrary user text, and under LIKE
-- a term containing % or _ would silently become a wildcard. instr does a
-- literal substring test, so the caller lowercases the term and nothing else
-- has to be escaped.
--
-- One concatenated haystack rather than three tests, because the term is bound
-- once. A term cannot span the joins: normalizeTerms splits on everything that
-- is not a letter, a digit, a dash or an underscore, so no term ever contains
-- the space the fields are joined with.
SELECT * FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation = ?
  AND instr(lower(name) || ' ' || lower(path) || ' ' || lower(summary), @term) > 0
ORDER BY exported DESC, symbol_id
LIMIT ?;

-- name: ListCodeGraphEdgesFrom :many
SELECT * FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation = ? AND from_key = ?
ORDER BY from_key, edge_id
LIMIT ?;

-- name: ListCodeGraphEdgesTo :many
SELECT * FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation = ? AND to_key = ?
ORDER BY to_key, edge_id
LIMIT ?;

-- name: PurgeCodeGraphFiles :execrows
DELETE FROM code_graph_files WHERE project_id = ? AND repo_id = ?;

-- name: PurgeCodeGraphSymbols :execrows
DELETE FROM code_graph_symbols WHERE project_id = ? AND repo_id = ?;

-- name: PurgeCodeGraphEdges :execrows
DELETE FROM code_graph_edges WHERE project_id = ? AND repo_id = ?;

-- name: DeleteCodeGraphIndex :execrows
DELETE FROM code_graph_index WHERE project_id = ? AND repo_id = ?;

-- name: ListCodeGraphSymbols :many
-- The bulk read behind the architecture summary. It runs once per completed
-- build, never on the dispatch path, which is why it is allowed to be whole:
-- the summary is a census and a census needs everything counted.
SELECT * FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation = ?
ORDER BY path, symbol_id;

-- name: ListCodeGraphEdges :many
SELECT * FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation = ?
ORDER BY path, edge_id;

-- name: ListCodeGraphEdgesFromKeys :many
-- The batched form of ListCodeGraphEdgesFrom, and the one retrieval uses.
--
-- One round trip for a whole neighbourhood rather than one per symbol. That is
-- not micro-optimisation: measurement on a synthetic thousand-file repository
-- found retrieval scaling with the graph, and the cause was ninety-six
-- separate statements per query, each paying its own preparation cost.
SELECT * FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation = ?
  AND from_key IN (sqlc.slice('keys'))
ORDER BY from_key, edge_id;

-- name: ListCodeGraphEdgesToKeys :many
SELECT * FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation = ?
  AND to_key IN (sqlc.slice('keys'))
ORDER BY to_key, edge_id;

-- name: ListCodeGraphSymbolsForPaths :many
SELECT * FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation = ?
  AND path IN (sqlc.slice('paths'))
ORDER BY path, symbol_id;

-- name: ListCodeGraphEdgesForPath :many
SELECT * FROM code_graph_edges
WHERE project_id = ? AND repo_id = ? AND generation = ? AND path = ?
ORDER BY path, edge_id;

-- name: SearchCodeGraphSymbolNames :many
-- The NAME-and-path candidate read, and the one retrieval leads with.
--
-- It exists apart from SearchCodeGraphSymbols because the two answer different
-- questions. A summary is prose: on a real repository, "path" appears in
-- hundreds of perfectly true sentences about symbols that have nothing to do
-- with the task. A name is a commitment somebody made about what a thing IS.
-- So a name match is what makes a symbol eligible, and a summary match is what
-- breaks ties between the eligible ones.
SELECT * FROM code_graph_symbols
WHERE project_id = ? AND repo_id = ? AND generation = ?
  AND instr(lower(name) || ' ' || lower(path), @term) > 0
ORDER BY exported DESC, symbol_id
LIMIT ?;
