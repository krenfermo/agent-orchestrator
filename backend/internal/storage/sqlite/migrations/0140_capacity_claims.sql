-- P1-C: durable runtime capacity claims.
--
-- Before this table AO had no answer to "may I start one more runtime". Every
-- dispatch site launched what it needed, and the only bound in the system was
-- incidental (a master objective serialises its tasks unless the project runs
-- in smart-parallel mode). A machine hosting a planner, four workers and three
-- reviewers is not faster; it is a laptop where nothing finishes.
--
-- It is a real table rather than another checkpoint payload because the
-- scheduler has to COUNT held claims inside the same write that grants the
-- next one. Counting JSON blobs in an append-only ledger is neither atomic nor
-- indexable, and a scheduler that can double-grant is not a scheduler.
--
-- Three properties are carried by the schema itself rather than by code:
--
--   * UNIQUE(dispatch_key) — one intended launch gets one claim, however many
--     times reconciliation, a wake, a restart or a double click re-derives it.
--     "Duplicate reconcile does not double-claim" is therefore a constraint,
--     not a convention.
--   * state='queued' — the queue is DURABLE. A daemon that restarts
--     reconstructs exactly what was waiting, because what was waiting is a row
--     rather than an in-memory list.
--   * lifecycle_generation — the fence. A claim carrying a generation older
--     than its step's current one describes a launch the lifecycle has moved
--     past; it may neither hold capacity nor release a newer claim.
--
-- Released rows are retained. They are the evidence of what held a slot and
-- why it came back, which is what makes an oversubscription diagnosable after
-- the fact instead of being reconstructed from logs.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE capacity_claims (
    id                   TEXT PRIMARY KEY,
    execution_kind       TEXT NOT NULL CHECK (execution_kind IN ('planner','worker','reviewer','repair')),
    state                TEXT NOT NULL CHECK (state IN ('queued','held','released')),
    workflow_run_id      TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    workflow_step_id     TEXT NOT NULL DEFAULT '',
    task_id              TEXT NOT NULL DEFAULT '',
    lifecycle_generation INTEGER NOT NULL DEFAULT 0,
    -- The launch intent's identity. UNIQUE is load-bearing; see above.
    dispatch_key         TEXT NOT NULL UNIQUE,
    owner_id             TEXT NOT NULL DEFAULT '',
    project_id           TEXT NOT NULL DEFAULT '',
    -- The runtime this claim actually paid for, once one exists. The instance
    -- is the immutable incarnation (tmux's `$N`), never a reusable name: it is
    -- what lets GC tell a claim's own runtime from a stranger that later took
    -- its name.
    runtime_handle       TEXT NOT NULL DEFAULT '',
    runtime_instance_id  TEXT NOT NULL DEFAULT '',
    priority             INTEGER NOT NULL DEFAULT 100,
    enqueued_at          TIMESTAMP NOT NULL,
    held_at              TIMESTAMP,
    released_at          TIMESTAMP,
    release_reason       TEXT NOT NULL DEFAULT '',
    updated_at           TIMESTAMP NOT NULL,
    -- A held claim must name the runtime generation it was granted for.
    CHECK (state <> 'released' OR released_at IS NOT NULL)
);
-- +goose StatementEnd

-- The scheduler's two hot reads: count what is held (globally and per kind),
-- and find the next eligible queued claim in deterministic order.
-- +goose StatementBegin
CREATE INDEX idx_capacity_claims_held ON capacity_claims(state, execution_kind);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_capacity_claims_queue ON capacity_claims(state, priority, enqueued_at, id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_capacity_claims_run ON capacity_claims(workflow_run_id, state);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS capacity_claims;
-- +goose StatementEnd
