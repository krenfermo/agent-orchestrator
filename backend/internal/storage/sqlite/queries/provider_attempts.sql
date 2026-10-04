-- name: CreateProviderAttempt :execrows
-- Record a provider attempt BEFORE it is admitted or launched, so a crash in
-- the launch window leaves an attempt to reason about rather than nothing.
-- Idempotent by (run, step, lifecycle generation, ordinal); refused by the
-- authoritative partial index when another attempt for the same obligation is
-- still live, which is what stops two providers holding one placement.
INSERT OR IGNORE INTO provider_attempts (
    id, workflow_run_id, workflow_step_id, task_id, project_id,
    lifecycle_generation, placement_generation, ordinal,
    provider, profile_id, state,
    failure_reason, failure_class, failover_safety, mutation_evidence_digest,
    runtime_session_id, capacity_claim_id,
    predecessor_attempt_id, successor_attempt_id,
    created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?,
    ?, ?, ?,
    ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?,
    ?, ?,
    ?, ?
);

-- name: GetProviderAttempt :one
SELECT * FROM provider_attempts WHERE id = ?;

-- name: GetAuthoritativeProviderAttempt :one
-- The attempt currently entitled to act for one obligation. At most one, by
-- index; a caller that finds none has no authority to launch.
SELECT * FROM provider_attempts
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND lifecycle_generation = sqlc.arg(lifecycle_generation)
  AND state IN ('planned','admitted','launching','running');

-- name: MaxProviderAttemptOrdinal :one
-- The failover budget's durable counter. Terminal rows are included on
-- purpose: the budget must survive a restart AND must not be reset by the
-- previous hop reaching a terminal state, which is exactly how an A->B->A loop
-- gets started.
SELECT CAST(COALESCE(MAX(ordinal), 0) AS INTEGER) AS ordinal FROM provider_attempts
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND lifecycle_generation = sqlc.arg(lifecycle_generation);

-- name: TransitionProviderAttemptState :execrows
-- Every transition compare-and-sets on the exact attempt identity and the
-- expected current state. A stale attempt matches zero rows, so it cannot
-- launch, complete, or reopen itself after a successor took over.
UPDATE provider_attempts
SET state = sqlc.arg(next_state),
    failure_reason = sqlc.arg(failure_reason),
    failure_class = sqlc.arg(failure_class),
    failover_safety = sqlc.arg(failover_safety),
    mutation_evidence_digest = sqlc.arg(mutation_evidence_digest),
    updated_at = sqlc.arg(updated_at),
    -- Same shape as the placement transition: the caller stamps it, because
    -- ProviderAttemptState.Terminal() in domain is the one definition of
    -- terminal and a second list here could drift from it.
    terminal_at = COALESCE(sqlc.narg(terminal_at), terminal_at)
WHERE id = sqlc.arg(id)
  AND state = sqlc.arg(expected_state);

-- name: BindProviderAttemptRuntime :execrows
-- Record the runtime and capacity claim this attempt is launching under.
-- Conditional on the attempt still being authoritative, so a superseded
-- attempt cannot attach itself to its successor's runtime.
UPDATE provider_attempts
SET runtime_session_id = sqlc.arg(runtime_session_id),
    capacity_claim_id = sqlc.arg(capacity_claim_id),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND state IN ('planned','admitted','launching','running');

-- name: LinkProviderAttemptSuccessor :execrows
-- Chain a failover. Written on the PREDECESSOR, and only once: a second hop
-- from the same attempt would mean one failure produced two successors.
UPDATE provider_attempts
SET successor_attempt_id = sqlc.arg(successor_attempt_id),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND successor_attempt_id = '';

-- name: ListProviderAttemptsForObligation :many
SELECT * FROM provider_attempts
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND lifecycle_generation = sqlc.arg(lifecycle_generation)
ORDER BY ordinal, id;

-- name: ListProviderAttemptsForRun :many
SELECT * FROM provider_attempts
WHERE workflow_run_id = ?
ORDER BY created_at, ordinal, id;

-- name: AbandonProviderAttemptsForRun :execrows
-- A terminal run's outstanding attempts are abandoned, not failed: nothing went
-- wrong with the provider, the obligation itself went away.
UPDATE provider_attempts
SET state = 'abandoned',
    failure_reason = sqlc.arg(failure_reason),
    updated_at = sqlc.arg(updated_at),
    terminal_at = sqlc.arg(updated_at)
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND state IN ('planned','admitted','launching','running');
