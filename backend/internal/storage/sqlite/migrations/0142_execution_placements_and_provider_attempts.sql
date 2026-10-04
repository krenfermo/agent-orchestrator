-- P1-D: the frozen execution placement, and the durable provider-attempt ledger.
--
-- Two tables, two invariants that the SCHEMA carries rather than the code:
--
--   * execution_placements — "where does this task's work happen" stops being a
--     derivation over project config and becomes a row written once, before any
--     mutation. UNIQUE(workflow_run_id, task_id, workflow_step_id,
--     placement_generation) makes a re-freeze idempotent; the partial unique
--     index on the non-terminal generation makes "at most one live placement
--     per obligation" a constraint, so two passes racing to freeze cannot
--     produce two worktrees.
--
--   * provider_attempts — a provider attempt is NOT a task generation. The run,
--     step, task and lifecycle generation are the obligation and are unchanged
--     by a failover; the ordinal is how many hops from the preferred provider
--     AO currently is. UNIQUE(workflow_run_id, workflow_step_id,
--     lifecycle_generation, ordinal) is what makes the failover budget a
--     durable fact rather than an in-memory counter that a restart resets.
--
-- Both tables retain terminal rows. They are the evidence of what held an
-- authority and why it stopped, which is what makes a stale-writer refusal
-- diagnosable after the fact instead of reconstructed from logs.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE execution_placements (
    id                   TEXT PRIMARY KEY,
    workflow_run_id      TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    task_id              TEXT NOT NULL DEFAULT '',
    workflow_step_id     TEXT NOT NULL DEFAULT '',
    project_id           TEXT NOT NULL DEFAULT '',
    -- The placement's OWN generation, distinct from the task's lifecycle
    -- generation. It advances only when the physical placement is replaced; a
    -- provider failover advances neither.
    placement_generation INTEGER NOT NULL,
    lifecycle_generation INTEGER NOT NULL DEFAULT 0,
    -- Two values only. smart_parallel_worktrees is a scheduling permission and
    -- materialises identically to isolated_worktree; storing it here would make
    -- the frozen record change meaning when the planner's classification did.
    placement_type       TEXT NOT NULL CHECK (placement_type IN ('direct_branch','isolated_worktree')),
    repo_path            TEXT NOT NULL,
    base_branch          TEXT NOT NULL DEFAULT '',
    base_sha             TEXT NOT NULL DEFAULT '',
    execution_branch     TEXT NOT NULL,
    worktree_path        TEXT NOT NULL DEFAULT '',
    worktree_record_id   TEXT NOT NULL DEFAULT '',
    merge_target         TEXT NOT NULL DEFAULT '',
    -- AO's own name for the daemon instance that froze this. No secret.
    owner_token          TEXT NOT NULL DEFAULT '',
    state                TEXT NOT NULL CHECK (state IN (
                            'selected','waiting','preparing','ready','active','reviewing',
                            'integrating','integrated','conflict','preserved','terminal')),
    provenance           TEXT NOT NULL DEFAULT 'frozen_at_selection'
                            CHECK (provenance IN ('frozen_at_selection','recovered_from_durable_facts')),
    waiting_reason       TEXT NOT NULL DEFAULT '',
    integrated_sha       TEXT NOT NULL DEFAULT '',
    detail               TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMP NOT NULL,
    updated_at           TIMESTAMP NOT NULL,
    finalized_at         TIMESTAMP,
    -- A direct-branch placement must NEVER name a worktree. Recording one
    -- would be fabricating an identity AO did not create, which is exactly what
    -- §A forbids for legacy rows -- and it is the asymmetry that matters here.
    -- An isolated placement's path is left open at freeze time on purpose: the
    -- placement is frozen BEFORE the checkout exists, so at that moment the
    -- path is a plan rather than a fact. It is filled in by
    -- RecordExecutionPlacementPreparation when the worktree becomes real, and
    -- a `ready` isolated placement without one is refused in code.
    CHECK (placement_type <> 'direct_branch' OR worktree_path = ''),
    UNIQUE (workflow_run_id, task_id, workflow_step_id, placement_generation)
);
-- +goose StatementEnd

-- At most ONE live placement per obligation. Two passes racing to freeze cannot
-- both win, so "the placement is frozen once" is a database property.
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_execution_placements_live
    ON execution_placements(workflow_run_id, task_id, workflow_step_id)
    WHERE state NOT IN ('integrated','preserved','terminal');
-- +goose StatementEnd

-- The authoritative lookup: newest generation for one obligation.
-- +goose StatementBegin
CREATE INDEX idx_execution_placements_obligation
    ON execution_placements(workflow_run_id, task_id, workflow_step_id, placement_generation DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_execution_placements_run ON execution_placements(workflow_run_id, state);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE provider_attempts (
    id                       TEXT PRIMARY KEY,
    workflow_run_id          TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    workflow_step_id         TEXT NOT NULL DEFAULT '',
    task_id                  TEXT NOT NULL DEFAULT '',
    project_id               TEXT NOT NULL DEFAULT '',
    -- The OBLIGATION. A failover changes neither of these.
    lifecycle_generation     INTEGER NOT NULL DEFAULT 0,
    -- The frozen placement this attempt is entitled to touch.
    placement_generation     INTEGER NOT NULL DEFAULT 0,
    -- 1 for the preferred provider, 2 for its first fallback, and so on. This
    -- is what the failover budget counts.
    ordinal                  INTEGER NOT NULL,
    provider                 TEXT NOT NULL DEFAULT '',
    profile_id               TEXT NOT NULL DEFAULT '',
    state                    TEXT NOT NULL CHECK (state IN (
                                'planned','admitted','launching','running','completed',
                                'failed_safe','failed_ambiguous','superseded','abandoned')),
    failure_reason           TEXT NOT NULL DEFAULT '',
    failure_class            TEXT NOT NULL DEFAULT '',
    failover_safety          TEXT NOT NULL DEFAULT '',
    -- The workspace fingerprint that PROVES safe_after_proven_no_mutation.
    -- Its absence is what makes that class unclaimable without proof.
    mutation_evidence_digest TEXT NOT NULL DEFAULT '',
    runtime_session_id       TEXT NOT NULL DEFAULT '',
    capacity_claim_id        TEXT NOT NULL DEFAULT '',
    predecessor_attempt_id   TEXT NOT NULL DEFAULT '',
    successor_attempt_id     TEXT NOT NULL DEFAULT '',
    created_at               TIMESTAMP NOT NULL,
    updated_at               TIMESTAMP NOT NULL,
    terminal_at              TIMESTAMP,
    -- One attempt per hop per obligation. The failover budget is therefore a
    -- durable fact a restart cannot reset.
    UNIQUE (workflow_run_id, workflow_step_id, lifecycle_generation, ordinal)
);
-- +goose StatementEnd

-- At most one AUTHORITATIVE attempt per obligation: a second live attempt would
-- be two providers holding one worktree, which is the failure this whole
-- ledger exists to prevent.
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_provider_attempts_authoritative
    ON provider_attempts(workflow_run_id, workflow_step_id, lifecycle_generation)
    WHERE state IN ('planned','admitted','launching','running');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_provider_attempts_obligation
    ON provider_attempts(workflow_run_id, workflow_step_id, lifecycle_generation, ordinal DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_provider_attempts_run ON provider_attempts(workflow_run_id, state);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS provider_attempts;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS execution_placements;
-- +goose StatementEnd
