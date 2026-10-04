-- 0145_project_memory_context_manifest.sql — what one execution was actually
-- told (P2-C §16).
--
-- P2-A can say what AO remembers. P2-B can say what a pack cost. Neither can
-- answer the question that matters when a task went wrong: **what did this
-- particular execution know?** The pack digest in the evidence record proves
-- two dispatches got the same memory, but it cannot be expanded back into the
-- facts it covered, so "the Worker was working from a stale decision" is a
-- suspicion rather than something anyone can check.
--
-- This table is that answer, and it is deliberately small. It stores the
-- IDENTITIES of the facts an execution received — not their content, which is
-- already in project_memory_items, and certainly not the prompt they were
-- rendered into. A manifest is therefore a few hundred bytes and stays useful
-- after the items themselves have been superseded, which is exactly the case
-- it exists for.
--
-- The row id is derived in Go from (project, run, task, role, pack digest), so
-- re-provisioning the same context after a restart addresses the same row
-- instead of appending a second one. A manifest is an observation, not an
-- event log: what a restart must reproduce is the same answer, not a longer
-- history of having asked.
-- +goose Up

-- +goose StatementBegin
CREATE TABLE project_memory_context_manifests (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo_id          TEXT NOT NULL DEFAULT '',
    -- The execution this context was assembled for. Both may be empty: a
    -- dispatch outside a workflow still has a role and still received facts.
    workflow_run_id  TEXT NOT NULL DEFAULT '',
    task_ref         TEXT NOT NULL DEFAULT '',
    role             TEXT NOT NULL,
    -- The digest of the rendered pack. Two manifests with the same digest
    -- describe the same memory, whatever else differs.
    pack_digest      TEXT NOT NULL DEFAULT '',
    -- The version of the selection policy that produced it. A pack that would
    -- be assembled differently today is recognisable rather than silently
    -- compared against one from a different policy.
    policy_version   INTEGER NOT NULL DEFAULT 0,
    -- The memory generation and commit the facts were served from.
    generation       INTEGER NOT NULL DEFAULT 0,
    indexed_commit   TEXT NOT NULL DEFAULT '',
    -- The item ids, in the order the pack presented them. Identities only.
    item_ids_json    TEXT NOT NULL DEFAULT '[]',
    item_count       INTEGER NOT NULL DEFAULT 0,
    selected_bytes   INTEGER NOT NULL DEFAULT 0,
    estimated_tokens INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL,
    CHECK (item_count >= 0),
    CHECK (selected_bytes >= 0),
    CHECK (generation >= 0)
);
-- +goose StatementEnd

-- The two reads: everything one task execution was told, and everything one
-- run's executions were told.
-- +goose StatementBegin
CREATE INDEX idx_project_memory_manifests_task
    ON project_memory_context_manifests (project_id, task_ref, created_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_project_memory_manifests_run
    ON project_memory_context_manifests (project_id, workflow_run_id, created_at DESC);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_manifests_run;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_project_memory_manifests_task;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS project_memory_context_manifests;
-- +goose StatementEnd
