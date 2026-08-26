-- The launch evidence a dispatch record still could not hold.
--
-- 0133 gave workflow_dispatch_checkpoints the boundary's own facts: which
-- run/step/attempt was being launched, under which outbox key, on which
-- harness, into which session, how far the launch got, what it concluded, and
-- the runtime's own words. Those answer "what happened at this boundary".
--
-- What they do not answer is "which workspace did it happen to, and which
-- process came out of it" -- and that is exactly the half a person needs when
-- a launch lands ambiguous. A row saying
--
--   phase=worker_dispatched outcome=ambiguous session=ses-7
--
-- cannot be checked against anything: not against the worktree it was supposed
-- to enter, not against the commit it was authorized from, not against the tree
-- state at the moment of the launch, and not against a live process, because
-- the AO session id survives a restart while the runtime instance behind it
-- does not. The sibling table (workflow_mutation_provenance) already records
-- branch, worktree, base SHA and fingerprint for a MUTATION; the dispatch that
-- caused the mutation recorded none of them, so the two halves of the same
-- incident could not be joined on the workspace they share.
--
-- So the dispatch record gets the remaining launch evidence, as columns:
--
--   * branch / worktree_path -- where the launch was aimed. Without them a
--     dispatch cannot be tied to the mutation provenance for the same tree.
--   * base_sha -- the commit the launch was authorized against
--     (SessionMetadata.DiffBaseSHA), which is what makes any later diff honest
--     once the target branch has moved.
--   * workspace_fingerprint -- the tree as it stood at the boundary
--     (workflow.WorkspaceFingerprint), so "the worker changed nothing" can be
--     decided against what the workspace looked like when the worker started
--     rather than against nothing at all.
--   * runtime_handle_id / runtime_launch_id / agent_session_id -- the process
--     and session ownership evidence. The launch id is the generation fence
--     (ports.SupervisedProcessRef): it is what distinguishes "the process AO
--     started here is still alive" from "a process is alive in a runtime AO
--     reused", and the difference between those two decides whether a retry
--     creates a second agent on one worktree.
--   * launched_at -- when the launch itself happened, which is NOT created_at.
--     created_at is when the row was written, and the two diverge in exactly
--     the cases that matter: a preflight or runtime_env failure writes a row
--     for a launch that never happened at all, and an adoption written at
--     recovery time describes a spawn from before the daemon restarted.
--
-- STRICTLY ADDITIVE, and additive to the table 0133 created -- there is no
-- second dispatch-evidence table. Every string column is NOT NULL DEFAULT ''
-- and launched_at is nullable, matching 0133's own convention: a fact the
-- writer could not read stays empty or NULL. Nothing is backfilled and nothing
-- is derived. In particular launched_at is never defaulted to created_at,
-- because "the launch happened when the row was written" is a claim, and for
-- every pre-0134 row and every failed-before-spawn row it is a false one.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN branch TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN worktree_path TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN base_sha TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN workspace_fingerprint TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN runtime_handle_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN runtime_launch_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN agent_session_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN launched_at TIMESTAMP;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints DROP COLUMN launched_at;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints DROP COLUMN agent_session_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints DROP COLUMN runtime_launch_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints DROP COLUMN runtime_handle_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints DROP COLUMN workspace_fingerprint;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints DROP COLUMN base_sha;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints DROP COLUMN worktree_path;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_dispatch_checkpoints DROP COLUMN branch;
-- +goose StatementEnd
