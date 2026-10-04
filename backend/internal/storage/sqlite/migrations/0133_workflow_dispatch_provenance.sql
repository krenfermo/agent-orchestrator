-- Durable storage for three facts AO currently only holds as free text inside
-- workflow_checkpoints.retry_state, keyed by a durable_phase string.
--
-- That encoding works, and it is the reason three separate incidents were hard
-- to answer. A checkpoint row says "durable_phase = worker_dispatched" and
-- carries a JSON blob whose shape is whatever the Go struct looked like on the
-- day it was written; nothing in SQL can be asked "which launch evidence does
-- this attempt have", "who last mutated this worktree", or "which reviewed
-- artifact is this verify attempt actually judging". Every one of those
-- questions is answered today by reading every checkpoint of a run in Go,
-- filtering on a phase constant and json-decoding the survivors.
--
-- So the three facts get columns:
--
--   * workflow_dispatch_checkpoints -- one row per dispatch boundary AO
--     crosses, carrying the launch evidence (stage, outcome, error class,
--     the runtime's own words) for that boundary.
--   * workflow_mutation_provenance -- one row per AO-owned mutation of a
--     workspace: which run/step/attempt/harness/session made it, in which
--     branch and worktree, from which base SHA to which HEAD SHA, and what
--     the workspace fingerprint was before and after. This is the durable
--     form of workflow.WorkspaceProvenanceRecord (see workspace_provenance.go
--     for the incident that produced it: verification could see that a
--     fingerprint had moved and had no way to say whose change it was).
--   * workflow_attempts.deadline_at + review-target columns -- a verify
--     attempt already records started_at/finished_at; what it could not record
--     is when it must conclude by, and which reviewed artifact it is verifying.
--
-- STRICTLY ADDITIVE. Two new tables plus four ALTER TABLE ADD COLUMN, every one
-- of them nullable or defaulted, so every row written before this migration
-- stays readable and simply has no provenance: a legacy attempt reads back with
-- a nil deadline and an empty review target, and a legacy run reads back with
-- zero dispatch-checkpoint and zero provenance rows. Nothing is backfilled --
-- inventing provenance for a mutation nobody observed would be exactly the
-- fabrication this table exists to make impossible.
--
-- No CHECK allowlist on phase/class/stage/outcome. Every neighbouring workflow
-- table that took one (0096, 0097, 0099, 0100, 0102) has since had to be
-- rebuilt whole to widen it, and these vocabularies are still growing. The Go
-- side owns validity; unknown values read back as themselves rather than
-- wedging a write.

-- +goose Up
-- +goose StatementBegin
-- Append-only. One row per dispatch boundary; never updated.
CREATE TABLE workflow_dispatch_checkpoints (
    id               TEXT PRIMARY KEY,
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs (id) ON DELETE CASCADE,
    workflow_step_id TEXT REFERENCES workflow_steps (id),
    attempt_id       TEXT REFERENCES workflow_attempts (id),
    -- The append-only checkpoint this dispatch was recorded alongside, when
    -- there was one. Nullable so a dispatch boundary can be recorded even if
    -- the checkpoint write is what failed.
    checkpoint_id    TEXT REFERENCES workflow_checkpoints (id),
    -- The durable_phase vocabulary this row mirrors: 'worker_dispatched',
    -- 'worker_launch_error', 'worker_launch_human_retry', and the reviewer's
    -- equivalents. Empty is legal and means "a dispatch happened and nobody
    -- named the phase".
    phase            TEXT NOT NULL DEFAULT '',
    -- The outbox idempotency key the dispatch was made under, which is what
    -- ties a launch to the exact command that asked for it.
    idempotency_key  TEXT NOT NULL DEFAULT '',
    harness          TEXT NOT NULL DEFAULT '',
    session_id       TEXT REFERENCES sessions (id),
    -- How far the launch got before it stopped: preflight | runtime_env |
    -- spawn (workflow.workerLaunchStage).
    launch_stage     TEXT NOT NULL DEFAULT '',
    -- dispatched | failed | ambiguous. 'ambiguous' is the one that matters:
    -- AO could not prove whether the spawn completed, and must never retry it.
    launch_outcome   TEXT NOT NULL DEFAULT '',
    error_class      TEXT NOT NULL DEFAULT '',
    -- The runtime's own words plus whatever else was observed at the boundary.
    -- Defaulted to an empty object so a legacy-shaped read never json-decodes
    -- an empty string.
    evidence_json    TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(evidence_json)),
    detail           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_dispatch_checkpoints_run
    ON workflow_dispatch_checkpoints (workflow_run_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_dispatch_checkpoints_step
    ON workflow_dispatch_checkpoints (workflow_step_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
-- Append-only. One row per AO-owned mutation boundary; never updated.
CREATE TABLE workflow_mutation_provenance (
    id               TEXT PRIMARY KEY,
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs (id) ON DELETE CASCADE,
    workflow_step_id TEXT REFERENCES workflow_steps (id),
    attempt_id       TEXT REFERENCES workflow_attempts (id),
    -- The planned task, when the mutation belongs to one. Deliberately NOT a
    -- foreign key into workflow_tasks: that table already has four ON DELETE
    -- CASCADE dependants, and 0130/0132 both warn that a naive rebuild of it
    -- cascades them all away. An append-only evidence table is not worth
    -- making that trap deeper. Empty when the run has no task decomposition.
    task_id          TEXT NOT NULL DEFAULT '',
    -- Whose change this is, in domain.WorkflowMutationClass's vocabulary
    -- (AUTHORIZED_WORK, AUTHORIZED_FIX, PREEXISTING, OTHER_AO_TASK, EXTERNAL,
    -- CONFLICTING, UNKNOWN). UNKNOWN is the default precisely because it is
    -- the honest answer whenever a required fact could not be read.
    provenance_class TEXT NOT NULL DEFAULT 'UNKNOWN',
    harness          TEXT NOT NULL DEFAULT '',
    session_id       TEXT REFERENCES sessions (id),
    branch           TEXT NOT NULL DEFAULT '',
    worktree_path    TEXT NOT NULL DEFAULT '',
    -- What the mutation started from and what it produced. base_sha is the
    -- commit the work was authorized against; head_sha is where the branch
    -- actually ended up, which is the only thing that makes a later diff
    -- honest once the target branch has moved.
    base_sha         TEXT NOT NULL DEFAULT '',
    head_sha         TEXT NOT NULL DEFAULT '',
    -- The workspace fingerprints either side of the mutation
    -- (workflow.WorkspaceFingerprint), so "the tree changed" can be attributed
    -- instead of merely observed.
    fingerprint_before TEXT NOT NULL DEFAULT '',
    fingerprint_after  TEXT NOT NULL DEFAULT '',
    -- Why this mutation happened, in words: the dispatch that caused it, the
    -- reviewer finding it answers, the human action that authorized it.
    reason           TEXT NOT NULL DEFAULT '',
    evidence_json    TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(evidence_json)),
    -- When the mutation itself was observed, which is not always when the row
    -- was written -- a provenance record reconstructed at a verification
    -- boundary is written long after the commit it describes. Nullable: a
    -- writer that cannot honestly say when the mutation happened leaves it
    -- unset rather than substituting its own clock.
    observed_at      TIMESTAMP,
    created_at       TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_mutation_provenance_run
    ON workflow_mutation_provenance (workflow_run_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_mutation_provenance_step
    ON workflow_mutation_provenance (workflow_step_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
-- When this attempt must have concluded by. NULL for every attempt written
-- before this migration and for every attempt kind that has no deadline; it is
-- never derived from a default duration, because "no deadline was recorded" and
-- "the deadline was the default" are different facts and only one of them is
-- true of a legacy row.
ALTER TABLE workflow_attempts ADD COLUMN deadline_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
-- Which reviewed artifact this attempt targets. A verify attempt is only
-- meaningful against the exact thing a reviewer approved, and until now that
-- linkage lived in the review step's mutable review_run_id plus a fingerprint
-- buried in a checkpoint's JSON -- so a re-review could silently move the
-- target out from under an in-flight verification. NULL / empty for every
-- attempt that predates this migration and for every non-verify attempt.
ALTER TABLE workflow_attempts ADD COLUMN review_target_review_run_id TEXT REFERENCES review_run (id);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_attempts ADD COLUMN review_target_fingerprint TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_attempts ADD COLUMN review_target_head_sha TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflow_attempts DROP COLUMN review_target_head_sha;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_attempts DROP COLUMN review_target_fingerprint;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_attempts DROP COLUMN review_target_review_run_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_attempts DROP COLUMN deadline_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_mutation_provenance_step;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_mutation_provenance_run;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_mutation_provenance;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_dispatch_checkpoints_step;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_dispatch_checkpoints_run;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_dispatch_checkpoints;
-- +goose StatementEnd
