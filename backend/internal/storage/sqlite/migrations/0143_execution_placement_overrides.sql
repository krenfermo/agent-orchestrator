-- P1-E §B/§C: the per-task placement OVERRIDE request, and the explicit
-- generation TRANSITION that is the only way an already-frozen placement moves.
--
-- P1-D deliberately shipped every placement route read-only, and recorded the
-- reason: a write that re-points a placement is a write that can aim a running
-- agent at a different checkout. It is not that the affordance is unsafe, it is
-- that it needs its own model. This migration is that model, and it is two
-- tables because a REQUEST and a TRANSITION are different facts:
--
--   * execution_placement_overrides — what somebody ASKED for, before anything
--     was frozen. It is consumed by the freeze and is never itself an
--     authority: the frozen placement row remains the only answer to "where
--     does this work happen". The partial unique index over the outstanding
--     state is what makes a repeated request idempotent rather than a queue of
--     competing wishes.
--
--   * execution_placement_transitions — the durable account of one placement
--     generation being replaced by another. It is written BEFORE the
--     replacement it describes, so a crash leaves an explanation for a move
--     that may not have happened (recoverable) rather than a move nobody can
--     account for — the same ordering the branch-lock cession ledger uses.
--     UNIQUE(run, task, step, from_generation) is what makes a repeated
--     transition request converge on the transition that already happened
--     instead of minting a second generation (§B.10).
--
-- Neither table is ever consulted to decide whether a placement is CURRENT.
-- That question is answered where it always was: by
-- MaxExecutionPlacementGeneration over execution_placements.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE execution_placement_overrides (
    id                   TEXT PRIMARY KEY,
    workflow_run_id      TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    task_id              TEXT NOT NULL DEFAULT '',
    workflow_step_id     TEXT NOT NULL DEFAULT '',
    project_id           TEXT NOT NULL DEFAULT '',
    -- 'auto' is a real request, not the absence of one: it means "withdraw any
    -- standing override and let selection policy decide", which an operator who
    -- has changed their mind needs to be able to say.
    requested_placement  TEXT NOT NULL CHECK (requested_placement IN ('auto','direct_branch','isolated_worktree')),
    -- The operator identity and the reason are REQUIRED by the caller, not by
    -- the schema, because an empty string here would be a row that records a
    -- decision nobody made. The refusal lives in code so it can say why.
    requested_by         TEXT NOT NULL DEFAULT '',
    reason               TEXT NOT NULL DEFAULT '',
    state                TEXT NOT NULL CHECK (state IN ('requested','applied','superseded','refused')),
    -- The generation this request was actually consumed by, once it was. Zero
    -- while the request is outstanding.
    applied_generation   INTEGER NOT NULL DEFAULT 0,
    detail               TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMP NOT NULL,
    updated_at           TIMESTAMP NOT NULL,
    resolved_at          TIMESTAMP
);
-- +goose StatementEnd

-- At most ONE outstanding request per obligation. A second request from a
-- person who clicked twice supersedes the first rather than stacking, and the
-- store's write is conditioned on that so the two cannot race.
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_execution_placement_overrides_outstanding
    ON execution_placement_overrides(workflow_run_id, task_id, workflow_step_id)
    WHERE state = 'requested';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_execution_placement_overrides_run
    ON execution_placement_overrides(workflow_run_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE execution_placement_transitions (
    id                    TEXT PRIMARY KEY,
    workflow_run_id       TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    task_id               TEXT NOT NULL DEFAULT '',
    workflow_step_id      TEXT NOT NULL DEFAULT '',
    project_id            TEXT NOT NULL DEFAULT '',
    -- The two generations this row binds. from_generation is part of the unique
    -- key: one placement generation is superseded at most once, so a repeated
    -- request is the SAME transition rather than another one.
    from_generation       INTEGER NOT NULL,
    to_generation         INTEGER NOT NULL DEFAULT 0,
    -- Source provenance, copied from the old placement at request time so the
    -- audit row still describes what was replaced after the placement row's own
    -- state has moved on.
    from_placement_type   TEXT NOT NULL DEFAULT '',
    from_repo_path        TEXT NOT NULL DEFAULT '',
    from_execution_branch TEXT NOT NULL DEFAULT '',
    from_worktree_path    TEXT NOT NULL DEFAULT '',
    from_base_sha         TEXT NOT NULL DEFAULT '',
    -- What was asked for, and what it resolved to.
    requested_placement   TEXT NOT NULL CHECK (requested_placement IN ('auto','direct_branch','isolated_worktree')),
    to_placement_type     TEXT NOT NULL DEFAULT '',
    -- Operator authority and intent. A transition with no named requester is
    -- refused in code: an unattributed re-pointing of a running obligation is
    -- exactly what this table exists to make impossible.
    requested_by          TEXT NOT NULL DEFAULT '',
    reason                TEXT NOT NULL DEFAULT '',
    -- The placement state the requester asserted the old generation was in.
    -- A mismatch is a refusal, not a correction: it means the world moved
    -- between the operator reading it and asking, and the request describes a
    -- situation that no longer exists.
    expected_state        TEXT NOT NULL DEFAULT '',
    -- The quiescence proof: which authorities were checked, and the digest of
    -- what they answered. Recorded because "it was safe" has to be inspectable
    -- afterwards rather than re-derived against a world that has moved.
    quiescence_digest     TEXT NOT NULL DEFAULT '',
    state                 TEXT NOT NULL CHECK (state IN ('requested','applied','refused')),
    refusal_reason        TEXT NOT NULL DEFAULT '',
    detail                TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMP NOT NULL,
    updated_at            TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- One SURVIVING transition per superseded generation: a placement generation is
-- replaced at most once, so a repeated request converges on the transition that
-- already happened instead of minting a second generation (§B.10).
--
-- Refusals are excluded from the key deliberately. A transition refused because
-- a capacity claim was still held must be retryable once that claim clears, and
-- a refusal that occupied the idempotency key would make the first "not yet"
-- permanent. Refused rows therefore accumulate as the audit trail of what was
-- asked and why AO said no, and none of them blocks the eventual yes.
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_execution_placement_transitions_surviving
    ON execution_placement_transitions(workflow_run_id, task_id, workflow_step_id, from_generation)
    WHERE state <> 'refused';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_execution_placement_transitions_run
    ON execution_placement_transitions(workflow_run_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS execution_placement_transitions;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS execution_placement_overrides;
-- +goose StatementEnd
