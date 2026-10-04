-- +goose Up
-- +goose StatementBegin
-- The attempt error_class CHECK drifted from the classes AO writes.
--
-- 0160 is the last migration that set this CHECK. Since then the Go side has
-- grown seven classes that are written to workflow_attempts.error_class and
-- that the CHECK rejects:
--
--   provider_auth_required, provider_workspace_trust_required,
--   provider_preflight_failed, provider_auth_interactive
--       the provider-preflight refusals (provider_preflight.go). Recorded via
--       concludeWorkerAttemptFailure before any spawn.
--   deliverable_not_observable
--       the pre-dispatch deliverable-observability refusal, same path.
--   superseded
--       a fix attempt closed because its cycle stopped being authorized
--       (fix_attempt_terminalization.go).
--   integration_failed
--       declared persistable by domain.WorkflowErrorClass.Valid.
--
-- On a real database each of those writes fails with
--   CHECK constraint failed: error_class IS NULL OR error_class IN (...)
-- so a refusal AO is designed to record legibly -- "this provider needs a
-- login", "git cannot see this deliverable" -- instead fails the whole
-- operation: StartRun returns 500 and the run is left `running` with its work
-- step `ready`. Nothing is spawned (every one of these refusals happens before
-- the spawn), but the readable stop the refusal exists to produce never lands.
-- Every unit test drove a fake store without the CHECK; the Frente 1
-- workflow-cycle E2E (real daemon, real SQLite) is what surfaced it.
--
-- The rebuild is 0160's corrected recipe verbatim apart from the list: the
-- three incoming NO ACTION references are parked and restored, foreign keys
-- stay enforced throughout, no child row is deleted, and columns are listed
-- explicitly. TestTableRebuildInventoryIsComplete derives the incoming
-- foreign keys from the schema and requires this rebuild to be registered.
CREATE TABLE workflow_attempts_fk_bak (
    child      TEXT NOT NULL,
    child_id   TEXT NOT NULL,
    attempt_id TEXT NOT NULL
);
INSERT INTO workflow_attempts_fk_bak (child, child_id, attempt_id)
SELECT 'workflow_checkpoints', id, attempt_id
FROM workflow_checkpoints WHERE attempt_id IS NOT NULL;
INSERT INTO workflow_attempts_fk_bak (child, child_id, attempt_id)
SELECT 'workflow_dispatch_checkpoints', id, attempt_id
FROM workflow_dispatch_checkpoints WHERE attempt_id IS NOT NULL;
INSERT INTO workflow_attempts_fk_bak (child, child_id, attempt_id)
SELECT 'workflow_mutation_provenance', id, attempt_id
FROM workflow_mutation_provenance WHERE attempt_id IS NOT NULL;

UPDATE workflow_checkpoints          SET attempt_id = NULL WHERE attempt_id IS NOT NULL;
UPDATE workflow_dispatch_checkpoints SET attempt_id = NULL WHERE attempt_id IS NOT NULL;
UPDATE workflow_mutation_provenance  SET attempt_id = NULL WHERE attempt_id IS NOT NULL;

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
        'capacity_exhausted','binary_missing','invalid_placement',
        'provider_auth_required','provider_workspace_trust_required','provider_preflight_failed',
        'provider_auth_interactive','deliverable_not_observable','integration_failed','superseded'
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

-- Put every parked reference back. The rebuilt table carries the same ids, so
-- each UPDATE re-establishes a reference that is satisfied the moment it lands.
UPDATE workflow_checkpoints SET attempt_id = (
    SELECT b.attempt_id FROM workflow_attempts_fk_bak b
    WHERE b.child = 'workflow_checkpoints' AND b.child_id = workflow_checkpoints.id)
WHERE id IN (SELECT child_id FROM workflow_attempts_fk_bak WHERE child = 'workflow_checkpoints');
UPDATE workflow_dispatch_checkpoints SET attempt_id = (
    SELECT b.attempt_id FROM workflow_attempts_fk_bak b
    WHERE b.child = 'workflow_dispatch_checkpoints' AND b.child_id = workflow_dispatch_checkpoints.id)
WHERE id IN (SELECT child_id FROM workflow_attempts_fk_bak WHERE child = 'workflow_dispatch_checkpoints');
UPDATE workflow_mutation_provenance SET attempt_id = (
    SELECT b.attempt_id FROM workflow_attempts_fk_bak b
    WHERE b.child = 'workflow_mutation_provenance' AND b.child_id = workflow_mutation_provenance.id)
WHERE id IN (SELECT child_id FROM workflow_attempts_fk_bak WHERE child = 'workflow_mutation_provenance');

DROP TABLE workflow_attempts_fk_bak;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Narrowing the CHECK back to 0160's list means rows carrying a class it
-- cannot express would violate it, so those are neutralised to NULL first.
-- Nothing else about any attempt is touched.
UPDATE workflow_attempts SET error_class = NULL WHERE error_class IN (
    'provider_auth_required','provider_workspace_trust_required','provider_preflight_failed',
    'provider_auth_interactive','deliverable_not_observable','integration_failed','superseded'
);
CREATE TABLE workflow_attempts_fk_bak (
    child      TEXT NOT NULL,
    child_id   TEXT NOT NULL,
    attempt_id TEXT NOT NULL
);
INSERT INTO workflow_attempts_fk_bak (child, child_id, attempt_id)
SELECT 'workflow_checkpoints', id, attempt_id
FROM workflow_checkpoints WHERE attempt_id IS NOT NULL;
INSERT INTO workflow_attempts_fk_bak (child, child_id, attempt_id)
SELECT 'workflow_dispatch_checkpoints', id, attempt_id
FROM workflow_dispatch_checkpoints WHERE attempt_id IS NOT NULL;
INSERT INTO workflow_attempts_fk_bak (child, child_id, attempt_id)
SELECT 'workflow_mutation_provenance', id, attempt_id
FROM workflow_mutation_provenance WHERE attempt_id IS NOT NULL;

UPDATE workflow_checkpoints          SET attempt_id = NULL WHERE attempt_id IS NOT NULL;
UPDATE workflow_dispatch_checkpoints SET attempt_id = NULL WHERE attempt_id IS NOT NULL;
UPDATE workflow_mutation_provenance  SET attempt_id = NULL WHERE attempt_id IS NOT NULL;

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
        'capacity_exhausted','binary_missing','invalid_placement'
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

-- Put every parked reference back. The rebuilt table carries the same ids, so
-- each UPDATE re-establishes a reference that is satisfied the moment it lands.
UPDATE workflow_checkpoints SET attempt_id = (
    SELECT b.attempt_id FROM workflow_attempts_fk_bak b
    WHERE b.child = 'workflow_checkpoints' AND b.child_id = workflow_checkpoints.id)
WHERE id IN (SELECT child_id FROM workflow_attempts_fk_bak WHERE child = 'workflow_checkpoints');
UPDATE workflow_dispatch_checkpoints SET attempt_id = (
    SELECT b.attempt_id FROM workflow_attempts_fk_bak b
    WHERE b.child = 'workflow_dispatch_checkpoints' AND b.child_id = workflow_dispatch_checkpoints.id)
WHERE id IN (SELECT child_id FROM workflow_attempts_fk_bak WHERE child = 'workflow_dispatch_checkpoints');
UPDATE workflow_mutation_provenance SET attempt_id = (
    SELECT b.attempt_id FROM workflow_attempts_fk_bak b
    WHERE b.child = 'workflow_mutation_provenance' AND b.child_id = workflow_mutation_provenance.id)
WHERE id IN (SELECT child_id FROM workflow_attempts_fk_bak WHERE child = 'workflow_mutation_provenance');

DROP TABLE workflow_attempts_fk_bak;
-- +goose StatementEnd
