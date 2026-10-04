-- +goose Up
-- +goose StatementBegin
-- P5: one new error_class, invalid_placement. SQLite cannot widen a CHECK in
-- place, so the column forces a table rebuild.
--
-- THE INCIDENT THIS FILE IS THE FIX FOR. The first version of 0160 copied the
-- rebuild recipe from 0096/0097/0099/0100/0102 verbatim. That recipe was safe
-- when it was written and is not safe now: since 0133, THREE tables carry a
-- foreign key into workflow_attempts(id) --
--
--     workflow_checkpoints.attempt_id
--     workflow_dispatch_checkpoints.attempt_id
--     workflow_mutation_provenance.attempt_id
--
-- all ON DELETE NO ACTION. Foreign keys are enforced while goose runs, and
-- SQLite treats DROP TABLE as deleting every row of the dropped table for FK
-- purposes, so `DROP TABLE workflow_attempts` raised
-- SQLITE_CONSTRAINT_FOREIGNKEY (787) against the 249 child rows that reference
-- it on a real ~/.ao/data/ao.db, and the daemon exited 1 on boot. The
-- transaction rolled back whole -- no partial effects -- but AO would not
-- start.
--
-- THE REMEDY is 0130's, which learned it from 0119 against a copy of a real
-- database: park the references, rebuild, restore. The difference here is that
-- these three children are NO ACTION rather than CASCADE, so their ROWS are
-- never at risk and never touched -- only the attempt_id VALUE is parked and
-- put back. No table is dropped except the one being rebuilt, no row is
-- deleted, and every other column of all 3,436 child rows is left alone.
--
-- Foreign keys stay ON for the whole migration. Turning them off would have
-- been the other documented way to rebuild, and it is deliberately not used:
-- it would let a genuine referential error through this migration silently,
-- and the failure being fixed here is precisely one that FK enforcement caught
-- correctly.
--
-- WHY THIS FILE IS CORRECTED IN PLACE rather than superseded by 0161. The
-- broken version cannot have been applied anywhere that could diverge: on any
-- database holding a single child reference it fails and rolls back, leaving
-- goose at 159 (which is exactly the state the incident left). A database with
-- no such rows would have applied it and recorded 160 -- and this version
-- produces a byte-identical final schema, so goose skipping it there is
-- correct. A new 0161 could not have helped: the broken 0160 runs first and
-- still fails.
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
-- Park every incoming reference, then clear it, so the rebuild below cannot
-- orphan anything. child_id is each child's own primary key (all three key on
-- `id`), which is what makes the restore exact rather than positional.
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
-- Narrowing the CHECK again means rows carrying the new class would violate it,
-- so they are neutralised to NULL first. Losing the class is the honest cost of
-- going back to a schema that cannot express it; nothing else about the attempt
-- is touched.
UPDATE workflow_attempts SET error_class = NULL WHERE error_class = 'invalid_placement';
-- The Down rebuild has the same three incoming foreign keys to protect, so it
-- parks and restores them exactly as the Up does.
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
