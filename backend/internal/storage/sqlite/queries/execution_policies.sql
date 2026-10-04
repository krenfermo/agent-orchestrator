-- Checkpoint 8P-C: per-user execution policy. One row per user (upsert via
-- UpsertUserExecutionPolicy); every read is scoped by user_id, mirroring
-- provider_profiles.sql's ownership-at-the-SQL-layer convention.

-- name: UpsertUserExecutionPolicy :one
INSERT INTO user_execution_policies (
    id, user_id, version, autonomous_mode, planner_priority, worker_priority,
    reviewer_priority, decision_resolver_priority, fallback_behavior,
    review_independence, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET
    version = excluded.version,
    autonomous_mode = excluded.autonomous_mode,
    planner_priority = excluded.planner_priority,
    worker_priority = excluded.worker_priority,
    reviewer_priority = excluded.reviewer_priority,
    decision_resolver_priority = excluded.decision_resolver_priority,
    fallback_behavior = excluded.fallback_behavior,
    review_independence = excluded.review_independence,
    updated_at = excluded.updated_at
RETURNING id, user_id, version, autonomous_mode, planner_priority, worker_priority,
    reviewer_priority, decision_resolver_priority, fallback_behavior,
    review_independence, created_at, updated_at;

-- name: GetUserExecutionPolicyByUser :one
SELECT id, user_id, version, autonomous_mode, planner_priority, worker_priority,
    reviewer_priority, decision_resolver_priority, fallback_behavior,
    review_independence, created_at, updated_at
FROM user_execution_policies WHERE user_id = ?;
