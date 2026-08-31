-- Project memory (migration 0144). Every mutating statement here is either
-- append-safe or generation-conditioned; there is no unconditional UPDATE of a
-- fact on purpose. A writer that cannot prove it is current does not write.

-- name: GetProjectMemoryIndex :one
SELECT * FROM project_memory_index WHERE project_id = ? AND repo_id = ?;

-- name: ListProjectMemoryIndexes :many
SELECT * FROM project_memory_index WHERE project_id = ? ORDER BY repo_id;

-- name: EnsureProjectMemoryIndex :execrows
-- Register a repository for indexing. Idempotent: a repeated registration of
-- the same (project, repo) inserts nothing and leaves the pass state alone.
INSERT OR IGNORE INTO project_memory_index (
    project_id, repo_id, repo_path, generation, phase, updated_at
) VALUES (?, ?, ?, 0, 'idle', ?);

-- name: UpdateProjectMemoryIndexRepoPath :execrows
-- Record that a repository moved. The identity (repo_id) is unchanged -- it is
-- what the caller resolved the row by -- so this only refreshes the
-- explanatory path.
UPDATE project_memory_index SET repo_path = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ?;

-- name: ClaimProjectMemoryIndexPass :execrows
-- Take the next generation and claim the pass, in one atomic statement.
--
-- This is what makes concurrent index attempts resolve to exactly one winner:
-- the claim applies only while the row is in a terminal phase, so a second
-- caller that arrives during a live pass matches zero rows and reads that as
-- "somebody else is indexing". A pass that died leaves the row non-terminal;
-- ReclaimProjectMemoryIndexPass below is the deliberate, generation-checked
-- way to take that over rather than a race this statement permits.
UPDATE project_memory_index
SET generation = generation + 1,
    phase = ?,
    pending_commit = ?,
    branch = ?,
    resume_cursor = '',
    files_seen = 0, files_indexed = 0, files_skipped = 0,
    items_written = 0, relations_written = 0,
    last_error = '',
    started_at = ?, completed_at = NULL, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND phase IN ('idle','failed');

-- name: ReclaimProjectMemoryIndexPass :execrows
-- Resume, or take over, a pass that is still marked in flight.
--
-- Conditional on the generation the caller believes is current, so two
-- restarts racing to recover the same abandoned pass cannot both proceed: the
-- loser's generation is stale by the time it writes. The cursor and counters
-- are deliberately NOT reset -- resuming is the point.
UPDATE project_memory_index
SET phase = ?, pending_commit = ?, branch = ?, last_error = '', updated_at = ?
WHERE project_id = ? AND repo_id = ? AND generation = ? AND phase NOT IN ('idle','failed');

-- name: AdvanceProjectMemoryIndexPass :execrows
-- Move the in-flight pass forward: new phase, new resume cursor, new counters.
-- Generation-conditioned, so a stalled pass that wakes up after a newer one
-- claimed the repository writes nothing.
UPDATE project_memory_index
SET phase = ?, resume_cursor = ?,
    files_seen = ?, files_indexed = ?, files_skipped = ?,
    items_written = ?, relations_written = ?,
    updated_at = ?
WHERE project_id = ? AND repo_id = ? AND generation = ? AND phase NOT IN ('idle','failed');

-- name: CompleteProjectMemoryIndexPass :execrows
-- Promote pending_commit to indexed_commit and return to idle. This is the
-- only statement that advances the commit incremental update diffs from, and
-- it runs only for a pass that reached the end -- a pass that died leaves
-- indexed_commit where it was, so the changes it never reached stay visible to
-- the next pass.
UPDATE project_memory_index
SET phase = 'idle', indexed_commit = pending_commit, pending_commit = '',
    resume_cursor = '',
    files_seen = ?, files_indexed = ?, files_skipped = ?,
    items_written = ?, relations_written = ?,
    last_error = '', completed_at = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND generation = ? AND phase NOT IN ('idle','failed');

-- name: FailProjectMemoryIndexPass :execrows
-- End a pass on an error. The generation is kept so the failure is
-- diagnosable and so a stale writer still cannot pass the CAS afterwards.
UPDATE project_memory_index
SET phase = 'failed', last_error = ?, completed_at = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND generation = ?;

-- name: GetProjectMemoryItem :one
SELECT * FROM project_memory_items WHERE id = ?;

-- name: InsertProjectMemoryItem :execrows
-- First write of a fact. OR IGNORE rather than OR REPLACE: a row that already
-- exists must go through the generation-conditioned update below, never be
-- silently clobbered by a writer that did not check whose generation it was.
INSERT OR IGNORE INTO project_memory_items (
    id, project_id, repo_id, item_type, scope, item_key,
    origin, origin_ref, summary, content,
    source_paths_json, source_commit, source_digest,
    generation, state, state_reason, confidence, metadata_json, content_hash,
    created_at, updated_at, invalidated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateProjectMemoryItem :execrows
-- Generation-conditioned update: apply only if the stored row is not already
-- at a NEWER generation than the writer's.
--
-- `<=` rather than `=` is deliberate. A pass legitimately writes the same fact
-- more than once within its own generation (a file summary refined after its
-- module is known), and an equal generation is the same pass, not a stale one.
-- What must never apply is a write whose generation is behind the row's --
-- that is a pass the repository has moved past.
UPDATE project_memory_items
SET item_type = ?, scope = ?, item_key = ?,
    summary = ?, content = ?,
    source_paths_json = ?, source_commit = ?, source_digest = ?,
    generation = ?, state = ?, state_reason = ?, confidence = ?,
    metadata_json = ?, content_hash = ?, updated_at = ?, invalidated_at = ?
WHERE id = ? AND generation <= ?;

-- name: TouchProjectMemoryItemProvenance :execrows
-- Re-confirm a fact whose content did not move: refresh only the provenance
-- and the generation, leaving updated_at alone.
--
-- Leaving updated_at alone is the whole point. "This fact has not changed
-- since March" is information a pack's freshness ranking uses, and a re-index
-- that bumped every timestamp would destroy it.
UPDATE project_memory_items
SET source_commit = ?, source_digest = ?, source_paths_json = ?,
    generation = ?, state = 'valid', state_reason = '', invalidated_at = NULL
WHERE id = ? AND generation <= ?;

-- name: MarkProjectMemoryItemState :execrows
-- Move one fact out of (or back into) validity. Generation-conditioned like
-- every other write.
UPDATE project_memory_items
SET state = ?, state_reason = ?, invalidated_at = ?, updated_at = ?
WHERE id = ? AND generation <= ?;

-- name: MarkProjectMemoryItemsStaleByPath :execrows
-- The incremental-update workhorse: every canonical fact derived from one
-- changed path stops being authoritative, in one indexed statement.
--
-- It marks rather than deletes. A stale fact is still the cheapest starting
-- point for re-deriving the new one, and "this went stale at commit X" is
-- itself evidence.
UPDATE project_memory_items
SET state = ?, state_reason = ?, invalidated_at = ?, updated_at = ?
WHERE state = 'valid'
  AND id IN (
    SELECT s.owner_id FROM project_memory_sources s
    WHERE s.owner_kind = 'item' AND s.project_id = ? AND s.repo_id = ? AND s.path = ?
  );

-- name: MarkProjectMemoryItemsStaleBelowGeneration :execrows
-- Retire what a completed pass did not re-confirm. A canonical item left
-- behind at an older generation was not re-derived by a pass that walked the
-- whole repository, which means its subject is gone.
--
-- Task-local items are excluded: they are not produced by the repository walk,
-- so a walk's silence says nothing about them.
UPDATE project_memory_items
SET state = ?, state_reason = ?, invalidated_at = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND origin = 'canonical'
  AND state <> ? AND generation < ?;

-- name: ListProjectMemoryItems :many
-- The selection read. Ordered deterministically so a context pack built twice
-- from the same store is byte-identical: confidence first, then the narrower
-- scope, then freshness, then the derived id as the final tiebreak.
SELECT * FROM project_memory_items
WHERE project_id = ? AND repo_id = ?
ORDER BY confidence DESC, scope, updated_at DESC, id;

-- name: ListProjectMemoryItemsByState :many
SELECT * FROM project_memory_items
WHERE project_id = ? AND repo_id = ? AND state = ?
ORDER BY confidence DESC, scope, updated_at DESC, id;

-- name: ListProjectMemoryItemsForProject :many
SELECT * FROM project_memory_items
WHERE project_id = ?
ORDER BY repo_id, confidence DESC, scope, updated_at DESC, id;

-- name: ListProjectMemoryItemsByOriginRef :many
SELECT * FROM project_memory_items
WHERE project_id = ? AND origin = ? AND origin_ref = ?
ORDER BY confidence DESC, scope, updated_at DESC, id;

-- name: ListProjectMemoryItemsByPath :many
SELECT i.* FROM project_memory_items i
JOIN project_memory_sources s
  ON s.owner_kind = 'item' AND s.owner_id = i.id
WHERE s.project_id = ? AND s.repo_id = ? AND s.path = ?
ORDER BY i.confidence DESC, i.scope, i.updated_at DESC, i.id;

-- name: CountProjectMemoryItemsByState :many
SELECT state, COUNT(*) AS total FROM project_memory_items
WHERE project_id = ? AND repo_id = ? GROUP BY state;

-- name: CountProjectMemoryItemsByType :many
SELECT item_type, COUNT(*) AS total FROM project_memory_items
WHERE project_id = ? AND repo_id = ? GROUP BY item_type;

-- name: CountProjectMemoryTaskLocalItems :one
SELECT COUNT(*) FROM project_memory_items
WHERE project_id = ? AND repo_id = ? AND origin = 'task_local';

-- name: LatestProjectMemoryItemUpdatedAt :one
-- The newest change to any fact of this repository. Written as an ordered
-- LIMIT 1 over the typed column rather than MAX(): an aggregate loses SQLite's
-- declared column type, and the driver hands back the raw stored text instead
-- of a time.Time.
SELECT updated_at FROM project_memory_items
WHERE project_id = ? AND repo_id = ?
ORDER BY updated_at DESC LIMIT 1;

-- name: DeleteProjectMemoryItemsForRepo :execrows
-- Used only by an explicit `memory rebuild --purge` and by repository
-- de-registration. Ordinary invalidation marks; it never deletes.
DELETE FROM project_memory_items WHERE project_id = ? AND repo_id = ?;

-- name: DeleteProjectMemoryTaskLocalItems :execrows
-- Retire one task's unintegrated view when the task ends. This is what stops
-- an isolated worktree from accumulating a permanent parallel memory.
DELETE FROM project_memory_items
WHERE project_id = ? AND origin = 'task_local' AND origin_ref = ?;

-- name: InsertProjectMemorySource :exec
INSERT OR REPLACE INTO project_memory_sources (
    owner_kind, owner_id, project_id, repo_id, path
) VALUES (?, ?, ?, ?, ?);

-- name: DeleteProjectMemorySourcesForOwner :exec
DELETE FROM project_memory_sources WHERE owner_kind = ? AND owner_id = ?;

-- name: ListProjectMemorySourcePaths :many
SELECT DISTINCT path FROM project_memory_sources
WHERE project_id = ? AND repo_id = ? ORDER BY path;

-- name: GetProjectMemoryRelation :one
SELECT * FROM project_memory_relations WHERE id = ?;

-- name: InsertProjectMemoryRelation :execrows
INSERT OR IGNORE INTO project_memory_relations (
    id, project_id, repo_id, from_kind, from_key, relation_kind, to_kind, to_key,
    origin, origin_ref, source_paths_json, source_commit, source_digest,
    generation, state, state_reason, confidence, metadata_json,
    created_at, updated_at, invalidated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateProjectMemoryRelation :execrows
-- Generation-conditioned, for the same reason items are: a stale indexer must
-- not resurrect an edge a newer pass retired.
UPDATE project_memory_relations
SET source_paths_json = ?, source_commit = ?, source_digest = ?,
    generation = ?, state = ?, state_reason = ?, confidence = ?,
    metadata_json = ?, updated_at = ?, invalidated_at = ?
WHERE id = ? AND generation <= ?;

-- name: MarkProjectMemoryRelationsStaleByPath :execrows
UPDATE project_memory_relations
SET state = ?, state_reason = ?, invalidated_at = ?, updated_at = ?
WHERE state = 'valid'
  AND id IN (
    SELECT s.owner_id FROM project_memory_sources s
    WHERE s.owner_kind = 'relation' AND s.project_id = ? AND s.repo_id = ? AND s.path = ?
  );

-- name: MarkProjectMemoryRelationsStaleBelowGeneration :execrows
UPDATE project_memory_relations
SET state = ?, state_reason = ?, invalidated_at = ?, updated_at = ?
WHERE project_id = ? AND repo_id = ? AND origin = 'canonical'
  AND state <> ? AND generation < ?;

-- name: ListProjectMemoryRelations :many
SELECT * FROM project_memory_relations
WHERE project_id = ? AND repo_id = ?
ORDER BY from_kind, from_key, relation_kind, to_kind, to_key, id;

-- name: ListProjectMemoryRelationsFrom :many
SELECT * FROM project_memory_relations
WHERE project_id = ? AND repo_id = ? AND from_kind = ? AND from_key = ? AND state = ?
ORDER BY relation_kind, to_kind, to_key, id;

-- name: ListProjectMemoryRelationsTo :many
SELECT * FROM project_memory_relations
WHERE project_id = ? AND repo_id = ? AND to_kind = ? AND to_key = ? AND state = ?
ORDER BY relation_kind, from_kind, from_key, id;

-- name: CountProjectMemoryRelations :one
SELECT COUNT(*) FROM project_memory_relations
WHERE project_id = ? AND repo_id = ?;

-- name: DeleteProjectMemoryRelationsForRepo :execrows
DELETE FROM project_memory_relations WHERE project_id = ? AND repo_id = ?;

-- name: DeleteProjectMemoryTaskLocalRelations :execrows
DELETE FROM project_memory_relations
WHERE project_id = ? AND origin = 'task_local' AND origin_ref = ?;

-- name: UpsertProjectMemoryFile :exec
-- The per-file digest ledger. OR REPLACE is correct here and nowhere else in
-- this file: a file row is not a derived fact with provenance to protect, it
-- is the observation "at generation N this path hashed to D", and the newest
-- observation is the only one worth keeping.
INSERT OR REPLACE INTO project_memory_files (
    project_id, repo_id, path, content_digest, size_bytes, generation,
    indexed_commit, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetProjectMemoryFile :one
SELECT * FROM project_memory_files
WHERE project_id = ? AND repo_id = ? AND path = ?;

-- name: ListProjectMemoryFiles :many
SELECT * FROM project_memory_files
WHERE project_id = ? AND repo_id = ? ORDER BY path;

-- name: ListProjectMemoryFilesBelowGeneration :many
-- Paths a completed pass did not re-observe: deleted, renamed away, or newly
-- excluded by the bounds. This is how deletions are detected without asking
-- git, so it works for an untracked or non-git checkout too.
SELECT * FROM project_memory_files
WHERE project_id = ? AND repo_id = ? AND generation < ? ORDER BY path;

-- name: DeleteProjectMemoryFilesBelowGeneration :execrows
DELETE FROM project_memory_files
WHERE project_id = ? AND repo_id = ? AND generation < ?;

-- name: DeleteProjectMemoryFilesForRepo :execrows
DELETE FROM project_memory_files WHERE project_id = ? AND repo_id = ?;

-- name: DeleteProjectMemoryFile :execrows
-- Drop one path's ledger entry. Used by incremental update when a diff reports
-- a deletion or a rename away: the path is gone now, and leaving its digest
-- behind would make the next full pass "discover" the deletion a second time.
DELETE FROM project_memory_files
WHERE project_id = ? AND repo_id = ? AND path = ?;
