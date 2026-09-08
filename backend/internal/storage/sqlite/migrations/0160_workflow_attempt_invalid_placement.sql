-- +goose Up
-- +goose StatementBegin
-- P5: one new error_class, invalid_placement, and the same table-rebuild dance
-- 0096/0097/0099/0100/0102 already performed -- SQLite cannot widen a CHECK
-- constraint in place.
--
-- It records a launch AO refused because it could not establish the run's
-- FROZEN execution placement. That failure used to have no class because it had
-- no failure: frozenPlacementTarget answered the empty placement, the workspace
-- router fell back to the project's CURRENT execution mode, and a run frozen
-- into an isolated worktree inside a project since switched to direct_branch
-- had its worker launched into the operator's own checkout. The class exists so
-- the refusal that replaces it is legible on the attempt row rather than
-- borrowing a provider class that implicates the agent, its credentials or its
-- binary -- none of which are involved, and none of which a failover would fix.
--
-- Columns are listed explicitly rather than copied with SELECT *: 0133 appended
-- four columns by ALTER TABLE, and a rebuild that assumes the 0102 shape would
-- silently mis-map them.
CREATE TABLE workflow_attempts_new (
    id TEXT PRIMARY KEY,
    workflow_step_id TEXT NOT NULL REFERENCES workflow_steps (id) ON DELETE CASCADE,
    attempt_number INTEGER NOT NULL,
    harness TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMP NOT NULL,
    finished_at TIMESTAMP,
    outcome TEXT CHECK (outcome IS NULL OR outcome IN ('succeeded','failed','cancelled')),
    error_class TEXT CHECK (error_class IS NULL OR error_class IN (
        'rate_limited','auth','transient','tool','test_failed','review_changes_requested',
        'session_create_failed','agent_start_failed','prompt_delivery_failed','runtime_failed',
        'worker_terminated_unexpectedly','ambiguous_worker_state','reviewer_launch_failed',
        'fix_budget_exhausted','verify_command_failed','verify_timeout','verify_environment_error',
        'verify_artifact_missing','verify_artifact_mismatch','verify_workspace_changed','verify_ambiguous',
        'capacity_exhausted','binary_missing','invalid_placement'
    )),
    retry_after TIMESTAMP,
    deadline_at TIMESTAMP,
    review_target_review_run_id TEXT REFERENCES review_run (id),
    review_target_fingerprint TEXT NOT NULL DEFAULT '',
    review_target_head_sha TEXT NOT NULL DEFAULT '',
    UNIQUE (workflow_step_id, attempt_number)
);
INSERT INTO workflow_attempts_new (
    id, workflow_step_id, attempt_number, harness, model, started_at, finished_at,
    outcome, error_class, retry_after, deadline_at, review_target_review_run_id,
    review_target_fingerprint, review_target_head_sha
)
SELECT
    id, workflow_step_id, attempt_number, harness, model, started_at, finished_at,
    outcome, error_class, retry_after, deadline_at, review_target_review_run_id,
    review_target_fingerprint, review_target_head_sha
FROM workflow_attempts;
DROP TABLE workflow_attempts;
ALTER TABLE workflow_attempts_new RENAME TO workflow_attempts;
CREATE INDEX idx_workflow_attempts_step ON workflow_attempts (workflow_step_id, attempt_number);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Narrowing the CHECK again means rows carrying the new class would violate it,
-- so they are neutralised to NULL first. Losing the class is the honest cost of
-- going back to a schema that cannot express it; nothing else about the attempt
-- is touched.
UPDATE workflow_attempts SET error_class = NULL WHERE error_class = 'invalid_placement';
CREATE TABLE workflow_attempts_old (
    id TEXT PRIMARY KEY,
    workflow_step_id TEXT NOT NULL REFERENCES workflow_steps (id) ON DELETE CASCADE,
    attempt_number INTEGER NOT NULL,
    harness TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMP NOT NULL,
    finished_at TIMESTAMP,
    outcome TEXT CHECK (outcome IS NULL OR outcome IN ('succeeded','failed','cancelled')),
    error_class TEXT CHECK (error_class IS NULL OR error_class IN (
        'rate_limited','auth','transient','tool','test_failed','review_changes_requested',
        'session_create_failed','agent_start_failed','prompt_delivery_failed','runtime_failed',
        'worker_terminated_unexpectedly','ambiguous_worker_state','reviewer_launch_failed',
        'fix_budget_exhausted','verify_command_failed','verify_timeout','verify_environment_error',
        'verify_artifact_missing','verify_artifact_mismatch','verify_workspace_changed','verify_ambiguous',
        'capacity_exhausted','binary_missing'
    )),
    retry_after TIMESTAMP,
    deadline_at TIMESTAMP,
    review_target_review_run_id TEXT REFERENCES review_run (id),
    review_target_fingerprint TEXT NOT NULL DEFAULT '',
    review_target_head_sha TEXT NOT NULL DEFAULT '',
    UNIQUE (workflow_step_id, attempt_number)
);
INSERT INTO workflow_attempts_old (
    id, workflow_step_id, attempt_number, harness, model, started_at, finished_at,
    outcome, error_class, retry_after, deadline_at, review_target_review_run_id,
    review_target_fingerprint, review_target_head_sha
)
SELECT
    id, workflow_step_id, attempt_number, harness, model, started_at, finished_at,
    outcome, error_class, retry_after, deadline_at, review_target_review_run_id,
    review_target_fingerprint, review_target_head_sha
FROM workflow_attempts;
DROP TABLE workflow_attempts;
ALTER TABLE workflow_attempts_old RENAME TO workflow_attempts;
CREATE INDEX idx_workflow_attempts_step ON workflow_attempts (workflow_step_id, attempt_number);
-- +goose StatementEnd
