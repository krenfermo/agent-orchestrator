-- P2-D: memory integrity, drift and provenance.
--
-- P2-A gave project memory a durable store, P2-B put it on the normal
-- execution path, and P2-C made one task's knowledge reusable by the next.
-- Each of those made memory MORE available. None of them made AO able to
-- prove, at read time, that a fact it is about to hand an agent is still
-- vouched for -- and the audit that opened P2-D found three specific ways a
-- fact could survive the thing that made it true:
--
--   1. `workflow_mutation_provenance` (migration 0133) had a schema, a store
--      method and no production writer at all. Every question of the form
--      "which workflow/task produced the change this memory is derived from"
--      was answered by re-reading `workflow_checkpoints.retry_state` JSON, or
--      not answered.
--   2. Promotion to canonical was gated on INFERENCE, not proof. A direct-
--      branch task was canonical because the project's execution MODE said
--      direct branch; a worktree task was canonical because a caller passed a
--      non-empty SHA. Neither is a durable record that the work is in the
--      repository's own history.
--   3. Repository identity was `sha256(absolute path)`. Two different
--      repositories checked out at one path over time are one identity, so the
--      second inherits the first's memory as if it were its own.
--
-- This migration adds the columns those three fixes need. It is STRICTLY
-- ADDITIVE: every column is `NOT NULL DEFAULT ''`/`0`, so every row written
-- before it reads back as "this fact carries no P2-D provenance", which is the
-- honest description of a legacy row and is exactly what the legacy
-- classification in internal/projectmemory/legacy.go acts on. Nothing is
-- backfilled. Inventing the provenance of a mutation nobody observed is the
-- fabrication this whole phase exists to make impossible.
--
-- None of the new columns carries a CHECK allowlist, following the convention
-- 0133 set and stated: every neighbouring workflow table that took one has
-- since had to be rebuilt whole to widen it, and these vocabularies are still
-- growing. Go owns validity (domain.MemoryAuthority.Valid,
-- WorkflowMutationBoundary.Valid, ...) and every one of those predicates fails
-- CLOSED, so an unrecognised value read back from an older or newer build
-- withholds the fact rather than being served as if it were understood. That
-- is a strictly stronger guarantee than a CHECK, which can only refuse a
-- write.

-- +goose Up

-- ---------------------------------------------------------------------------
-- 1. Memory authority, as an axis of its own.
-- ---------------------------------------------------------------------------
--
-- `state` (0144) is the DRIFT lifecycle: does this fact's evidence still look
-- the way it did when the fact was derived. `authority` is a different
-- question: can AO still PROVE that whatever licensed this fact is in force.
--
-- They are separate columns rather than more values in `state` because they
-- move independently and for different reasons. A fact whose files have not
-- changed at all (state = valid) can lose its authority the moment the
-- integration that promoted it turns out never to have happened; a fact whose
-- files changed (state = stale) still has a promotion authority worth showing
-- an operator. Collapsing them would make "why is this not being served" have
-- one slot for two answers.
--
-- Servable requires BOTH: state = 'valid' AND authority = 'authoritative'.
-- That is the fail-closed rule in one predicate (domain.ProjectMemoryItem.Servable),
-- and every new authority value is therefore withheld by default rather than
-- served by default.
-- +goose StatementBegin
ALTER TABLE project_memory_items ADD COLUMN authority TEXT NOT NULL DEFAULT 'authoritative';
-- +goose StatementEnd

-- Why authority is not 'authoritative'. Carries the reason vocabulary from
-- docs/project-memory-authority.md (memory_provenance_missing,
-- memory_source_drift, memory_generation_stale, memory_repo_identity_changed,
-- memory_promotion_unprovable, memory_legacy_no_provenance) followed by the
-- human-readable detail, so an operator gets both the class and the specifics.
-- +goose StatementBegin
ALTER TABLE project_memory_items ADD COLUMN authority_reason TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- The durable repository identity this fact was derived under
-- (domain.RepoIdentity). Empty for every pre-P2-D row and for a checkout whose
-- identity could not be read; both cases are 'AO cannot prove this is the same
-- repository', which is why an empty value never MATCHES a non-empty one.
--
-- This is what separates 'the same repository moved to a new path' from 'a
-- different repository was checked out at the old path'. repo_id -- a hash of
-- the path -- cannot tell those apart, and the second is the dangerous one.
-- +goose StatementBegin
ALTER TABLE project_memory_items ADD COLUMN repo_identity TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- How this fact was derived, in domain.MemoryProvenanceKind's vocabulary:
-- 'repo_derivation' (an indexer read files), 'task_outcome' (a finished task's
-- durable facts), 'workflow_knowledge' (a decision or a risk lifted out of a
-- durable workflow row), 'legacy' (written before P2-D). It says which PROOF
-- applies to the row, so validation does not have to guess from the item type.
-- +goose StatementBegin
ALTER TABLE project_memory_items ADD COLUMN provenance_kind TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- The workflow_mutation_provenance row that licensed this fact's promotion to
-- canonical, when one did. This is the join that answers "how did this become
-- project knowledge" with a row instead of an inference, and it is the single
-- field that makes §13's worktree-integration proof checkable after the fact.
-- Empty for facts no promotion produced (repository derivations) and for
-- promotions AO could not prove -- the latter are also authority = 'unprovable'.
-- +goose StatementBegin
ALTER TABLE project_memory_items ADD COLUMN promotion_authority TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- The three commits a promoted fact is pinned to, kept apart because they
-- authorize different things (P2-D §7):
--
--   * source_commit (0144) -- where the CONTENT was read.
--   * verified_commit      -- what verification actually passed on.
--   * integrated_commit    -- the target-branch commit the work became part of.
--
-- A worktree result has a verified commit and no integrated one until an
-- integration is proven; conflating them is precisely how an unintegrated
-- worktree could claim canonical authority.
-- +goose StatementBegin
ALTER TABLE project_memory_items ADD COLUMN verified_commit TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE project_memory_items ADD COLUMN integrated_commit TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- The withheld-items read, and the validation sweep's working set. Both ask
-- "which of this repository's facts are not authoritative", which without an
-- index is a scan of every fact of every repository.
-- +goose StatementBegin
CREATE INDEX idx_project_memory_items_authority
    ON project_memory_items (project_id, repo_id, authority);
-- +goose StatementEnd

-- Edges take the same axis, for the same reason (P2-D §23): an edge derived
-- from a node whose authority has gone must stop being traversed as current,
-- and must not be deleted -- the audit history of how two facts were once
-- related is the thing an operator reads when asking why a decision was made.
-- +goose StatementBegin
ALTER TABLE project_memory_relations ADD COLUMN authority TEXT NOT NULL DEFAULT 'authoritative';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE project_memory_relations ADD COLUMN authority_reason TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE project_memory_relations ADD COLUMN repo_identity TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 2. Context manifests record item VERSIONS, not just item ids (P2-D §18).
-- ---------------------------------------------------------------------------
--
-- 0145 stored the ids of the facts one execution was told. That answers "which
-- facts" and cannot answer "which version of them", and the incident shape
-- P2-D §17 describes is exactly a version question: the worker was told the
-- memory of generation X, the reviewer judged SHA Z, and nothing recorded
-- whether the pack the reviewer got was assembled from the same rows.
--
-- item_versions_json is a JSON array, positionally aligned with the existing
-- item_ids_json, of each item's content_hash at selection time. A manifest is
-- never rewritten, so this is a record of what was true then, forever.
-- +goose StatementBegin
ALTER TABLE project_memory_context_manifests ADD COLUMN item_versions_json TEXT NOT NULL DEFAULT '[]';
-- +goose StatementEnd

-- The head the role was reasoning about, when the caller knew it. For a
-- reviewer this is the reviewed SHA, which is what makes "the pack was built
-- for SHA A and the reviewer judged SHA B" a diagnosable fact rather than an
-- unexplained disagreement.
-- +goose StatementBegin
ALTER TABLE project_memory_context_manifests ADD COLUMN role_head_sha TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 3. workflow_mutation_provenance gains what a memory promotion must prove.
-- ---------------------------------------------------------------------------
--
-- The 0133 table records a mutation's run/step/attempt/branch/worktree and the
-- SHAs and fingerprints either side of it. That is everything the verification
-- path needed. It is NOT enough for a memory promotion, which has to answer
-- three further questions before it may call a fact canonical:
--
--   * WHICH repository, durably -- a run's branch name is not a repository.
--   * WHICH boundary this row describes -- a worktree result head and an
--     integrated target head are both "a mutation", and only one of them
--     licenses canonical knowledge.
--   * WHICH generation observed it -- so a stale worker/reviewer/repair
--     callback that wakes up late cannot write provenance for, or promote
--     against, a generation that has moved on.
--
-- Still append-only, and still never updated.
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- The durable repository identity (domain.RepoIdentity) and the checkout path
-- it was read from. The path is explanatory; the identity is the one that is
-- compared.
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN repo_identity TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN repo_path TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- How the work was placed: 'direct_branch' or 'isolated_worktree'
-- (domain.WorkflowMutationPlacement). The two have entirely different
-- promotion proofs, and a row that does not say which it was cannot supply
-- either.
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN placement TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- The semantic mutation boundary this row records
-- (domain.WorkflowMutationBoundary): 'dispatch', 'work_result',
-- 'repair_result', 'verified', 'integrated'. AO records boundaries, not write
-- syscalls -- one row per moment at which what the repository contains changed
-- in a way a later reader must be able to attribute.
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN boundary TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- The generation this boundary belongs to. For a work/repair boundary it is
-- the attempt generation; for an integration it is the integration generation.
-- A writer carrying a generation behind the newest row for the same
-- (run, task, boundary) is refused -- see the CAS in
-- workflow/mutation_provenance.go.
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- Where an integration landed. target_ref is the branch integrated INTO;
-- target_before_sha and target_after_sha are that branch either side of the
-- integration. head_sha (0133) stays what it always was: where the SOURCE of
-- the mutation ended up. An integration boundary needs both ends, and a
-- promotion that cannot name the target_after_sha has not proven integration.
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN integration_target_ref TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN integration_target_before_sha TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN integration_target_after_sha TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- How the source reached the target: 'merge', 'fast_forward', 'cherry_pick',
-- 'direct_commit' (domain.WorkflowIntegrationMethod). It is recorded because
-- cherry-pick produces a DIFFERENT SHA for the same content, so "is the
-- worktree result reachable from the target" is the wrong ancestry question
-- for it and the right one for the others.
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN integration_method TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- Exactly-once, across duplicate callbacks and across restarts.
--
-- The key is derived by the writer from the facts of the boundary itself
-- (run, task, boundary, generation, the SHAs), so a callback delivered twice
-- and a daemon that died between the mutation and the row derive the SAME key
-- and produce ONE row. A partial unique index rather than a column constraint
-- because a legacy row -- and any writer that honestly cannot derive a key --
-- has '' here, and '' must not collide with itself.
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance ADD COLUMN idempotency_key TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_workflow_mutation_provenance_idem
    ON workflow_mutation_provenance (idempotency_key)
    WHERE idempotency_key <> '';
-- +goose StatementEnd

-- The promotion read: "what does AO durably know about this task's mutations".
-- Memory promotion asks it once per promotion, and it must not scan the run.
-- +goose StatementBegin
CREATE INDEX idx_workflow_mutation_provenance_task
    ON workflow_mutation_provenance (task_id, boundary, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_mutation_provenance_task;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_mutation_provenance_idem;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN idempotency_key;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN integration_method;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN integration_target_after_sha;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN integration_target_before_sha;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN integration_target_ref;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN generation;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN boundary;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN placement;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN repo_path;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN repo_identity;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_mutation_provenance DROP COLUMN project_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_context_manifests DROP COLUMN role_head_sha;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_context_manifests DROP COLUMN item_versions_json;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_relations DROP COLUMN repo_identity;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_relations DROP COLUMN authority_reason;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_relations DROP COLUMN authority;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_items_authority;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_items DROP COLUMN integrated_commit;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_items DROP COLUMN verified_commit;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_items DROP COLUMN promotion_authority;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_items DROP COLUMN provenance_kind;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_items DROP COLUMN repo_identity;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_items DROP COLUMN authority_reason;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE project_memory_items DROP COLUMN authority;
-- +goose StatementEnd
