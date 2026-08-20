-- Checkpoint 8P-E.11: durable execution locks for direct-branch mode.
--
-- In isolated-worktree mode two concurrent workflows can never collide: each
-- one gets its own physical worktree on its own generated ao/* branch. Direct
-- branch removes that isolation on purpose, so the mutual exclusion has to
-- become explicit and durable instead. One row here means "workflow run R is
-- the current writer of repository P on branch B."
--
-- Design notes:
--
--   * The lock identity is (repo_path, branch), NOT (project_id, branch). A
--     workspace project registers several independent Git repositories, and
--     each one has its own configured branch -- medusa on main and
--     medusa/backend_node on medusa_back_v2 are two separate locks that must
--     never serialize against each other. Keying by project would collapse
--     them into one and silently block legitimate parallel work; keying by
--     repo path also correctly serializes two *different* projects that
--     happen to point at the same repository on disk.
--
--   * lock_key is the canonical, already-normalized repo path and branch
--     joined by a unit separator, computed in Go (see
--     domain.BranchLockKey) so SQLite never has to reimplement path
--     canonicalization.
--
--   * Mutual exclusion is enforced by the database, not by application code:
--     the partial UNIQUE index below makes a second held row for the same
--     lock_key a constraint violation. Two daemons, or two goroutines racing
--     inside one daemon, therefore cannot both believe they hold it -- one
--     INSERT simply fails. Released rows are retained as history and are
--     excluded from the index, so the same pair can be locked again later.
--
--   * owner_token identifies the daemon instance that acquired the lock. It
--     is what makes restart reconciliation decidable: after a crash, a held
--     row whose owner_token belongs to a previous daemon instance and whose
--     workflow run is no longer live is a stale lock that can be released,
--     while a row owned by the current instance with a live run is a
--     legitimate lock that must be preserved. Reconciliation never guesses
--     from timestamps alone.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE branch_locks (
    id               TEXT PRIMARY KEY,
    lock_key         TEXT NOT NULL,
    project_id       TEXT NOT NULL,
    repo_path        TEXT NOT NULL,
    -- '__root__' for a single-repo project or a workspace project's root,
    -- otherwise the registered child repo name.
    repo_name        TEXT NOT NULL DEFAULT '__root__',
    branch           TEXT NOT NULL,
    workflow_run_id  TEXT NOT NULL,
    workflow_step_id TEXT,
    session_id       TEXT,
    owner_token      TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('held','released')),
    -- The SHA the repository was on when the lock was acquired. Persisted so a
    -- later reconciliation or report can state truthfully what the run started
    -- from, rather than re-deriving it from a working tree that has moved on.
    base_sha         TEXT NOT NULL DEFAULT '',
    acquired_at      TIMESTAMP NOT NULL,
    renewed_at       TIMESTAMP NOT NULL,
    released_at      TIMESTAMP,
    release_reason   TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
-- The mutual-exclusion primitive itself: at most one held lock per
-- repository+branch, enforced by SQLite rather than by application code.
CREATE UNIQUE INDEX idx_branch_locks_held_key
    ON branch_locks (lock_key) WHERE state = 'held';
-- +goose StatementEnd

-- +goose StatementBegin
-- Run-scoped release cascade and "which locks does this run hold" lookups.
CREATE INDEX idx_branch_locks_run_state
    ON branch_locks (workflow_run_id, state);
-- +goose StatementEnd

-- +goose StatementBegin
-- Project-scoped occupancy reads for Project Settings and the board.
CREATE INDEX idx_branch_locks_project_state
    ON branch_locks (project_id, state);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS branch_locks;
-- +goose StatementEnd
